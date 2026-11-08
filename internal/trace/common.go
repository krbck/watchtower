package trace

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/krbck/watchtower/internal/checker"
	"github.com/krbck/watchtower/internal/graphid"
)

// entry is the resolved data plane of an entry point: the gateway /
// controller pods, their workloads and the Services in front of them.
// Resolution deliberately spans every namespace — controllers live in
// system namespaces (istio-system, kube-system traefik / k3s svclb).
type entry struct {
	selector  map[string]string
	pods      []*corev1.Pod
	readyPods int
	workloads []string // workload node IDs (pod ID for standalone pods), sorted
	services  []*corev1.Service
}

// entryForSelector resolves pods by label selector (all namespaces).
func (w *world) entryForSelector(selector map[string]string) entry {
	e := entry{selector: selector}
	if len(selector) == 0 {
		return e
	}
	for i := range w.pods {
		if labelsMatch(selector, w.pods[i].Labels) {
			e.pods = append(e.pods, &w.pods[i])
		}
	}
	w.completeEntry(&e)
	return e
}

// entryForService resolves the pods behind a known entry Service.
func (w *world) entryForService(svc *corev1.Service) entry {
	e := entry{selector: svc.Spec.Selector}
	for i := range w.pods {
		p := &w.pods[i]
		if p.Namespace == svc.Namespace && labelsMatch(svc.Spec.Selector, p.Labels) {
			e.pods = append(e.pods, p)
		}
	}
	w.completeEntry(&e)
	// Keep the given Service first even if others select the pods too.
	rest := []*corev1.Service{svc}
	for _, s := range e.services {
		if s != svc {
			rest = append(rest, s)
		}
	}
	e.services = rest
	return e
}

// workloadID maps a pod to its workload node ID exactly like the mapper:
// the owner (graphid.PodOwner) only when that workload exists — a
// ReplicaSet pod without its Deployment stays a standalone "pod:" node —
// so trace highlight IDs match the Overview.
func (w *world) workloadID(p *corev1.Pod) string {
	if id := graphid.PodOwner(p); id != "" && w.workloads[id] {
		return id
	}
	return graphid.Pod(p.Namespace, p.Name)
}

func (w *world) completeEntry(e *entry) {
	seen := map[string]bool{}
	for _, p := range e.pods {
		if podReady(p) {
			e.readyPods++
		}
		id := w.workloadID(p)
		if !seen[id] {
			seen[id] = true
			e.workloads = append(e.workloads, id)
		}
	}
	sort.Strings(e.workloads)
	// Services selecting any of the pods; LoadBalancer first, then
	// NodePort, then ClusterIP (each by ns/name).
	for i := range w.services {
		svc := &w.services[i]
		if len(svc.Spec.Selector) == 0 {
			continue
		}
		for _, p := range e.pods {
			if p.Namespace == svc.Namespace && labelsMatch(svc.Spec.Selector, p.Labels) {
				e.services = append(e.services, svc)
				break
			}
		}
	}
	sort.SliceStable(e.services, func(i, j int) bool { return typeRank(e.services[i]) < typeRank(e.services[j]) })
}

func typeRank(s *corev1.Service) int {
	switch s.Spec.Type {
	case corev1.ServiceTypeLoadBalancer:
		return 0
	case corev1.ServiceTypeNodePort:
		return 1
	}
	return 2
}

// serviceFor returns the first entry Service exposing port (nil if none).
func (e *entry) serviceFor(port int32) (*corev1.Service, *corev1.ServicePort) {
	for _, s := range e.services {
		for i := range s.Spec.Ports {
			if s.Spec.Ports[i].Port == port {
				return s, &s.Spec.Ports[i]
			}
		}
	}
	return nil, nil
}

// frontHalf emits the address → entry Service → workload steps shared by
// every provider. portReason is the reason when no entry Service exposes
// the requested port.
func frontHalf(ctx context.Context, w *world, b *branchBuilder, e entry, req Request, what, portReason string) {
	svc, sp := e.serviceFor(req.Port)

	// (1) External address.
	switch {
	case len(e.services) == 0:
		b.add(Step{
			Name:   "External address",
			Hop:    HopAddress,
			Status: StatusFail,
			Reason: ReasonNoExternalAddress,
			Detail: fmt.Sprintf("No Service selects the %s pods, so nothing exposes it.", what),
			Hint:   "Create a LoadBalancer or NodePort Service selecting the gateway/controller pods.",
		})
	case svc == nil:
		b.add(Step{
			Name:   "External address",
			Hop:    HopAddress,
			Status: StatusUnknown,
			Detail: fmt.Sprintf("No entry Service exposes port %d (see next step).", req.Port),
		})
	default:
		addressStep(b, svc, sp)
	}

	// (2) Entry Service + ready endpoints.
	if svc == nil {
		if len(e.services) > 0 {
			names := make([]string, 0, len(e.services))
			for _, s := range e.services {
				names = append(names, s.Namespace+"/"+s.Name+" ("+portList(s)+")")
			}
			b.add(Step{
				Name:   fmt.Sprintf("Entry Service port %d", req.Port),
				Hop:    HopEntryService,
				Ref:    graphid.Service(e.services[0].Namespace, e.services[0].Name),
				Status: StatusFail,
				Reason: portReason,
				Detail: fmt.Sprintf("None of the Services in front of the %s exposes port %d: %s.", what, req.Port, strings.Join(names, "; ")),
				Hint:   fmt.Sprintf("Add port %d to the entry Service (targetPort = the port the %s listens on), or query the port it does expose.", req.Port, what),
			})
		}
	} else {
		ref := graphid.Service(svc.Namespace, svc.Name)
		slices, err := w.kube.EndpointSlices(ctx, svc.Namespace, svc.Name)
		ready, notReady := checker.CountEndpoints(slices)
		st := Step{
			Name:   fmt.Sprintf("Entry Service %s/%s :%d → %s", svc.Namespace, svc.Name, sp.Port, sp.TargetPort.String()),
			Hop:    HopEntryService,
			Ref:    ref,
			Status: StatusOK,
			Detail: fmt.Sprintf("type=%s; %d ready / %d notReady endpoints.", svc.Spec.Type, ready, notReady),
		}
		switch {
		case err != nil:
			st.Status = StatusUnknown
			st.Detail = fmt.Sprintf("failed to list EndpointSlices: %v", err)
		case ready == 0:
			st.Status = StatusFail
			st.Reason = ReasonNoReadyEndpoints
			st.Hint = fmt.Sprintf("The %s pods are not Ready endpoints of %s/%s: check their readiness.", what, svc.Namespace, svc.Name)
		}
		b.add(st)
	}

	// (3) Gateway / controller workload readiness.
	workloadStep(b, e, what)
}

// addressStep reports how the entry Service is reachable from outside.
func addressStep(b *branchBuilder, svc *corev1.Service, sp *corev1.ServicePort) {
	ref := graphid.TracePrefix + "addr:" + svc.Namespace + "/" + svc.Name
	var addrs []string
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			addrs = append(addrs, ing.IP)
		}
		if ing.Hostname != "" {
			addrs = append(addrs, ing.Hostname)
		}
	}
	addrs = append(addrs, svc.Spec.ExternalIPs...)
	addrs = dedupe(addrs)
	var parts []string
	if len(addrs) > 0 {
		parts = append(parts, strings.Join(addrs, ", ")+" :"+strconv.Itoa(int(sp.Port)))
	}
	if sp.NodePort != 0 {
		parts = append(parts, "NodePort "+strconv.Itoa(int(sp.NodePort))+" on every node")
	}
	meta := map[string]string{"service": svc.Namespace + "/" + svc.Name}
	if len(addrs) > 0 {
		meta["addresses"] = strings.Join(addrs, ", ")
	}
	if sp.NodePort != 0 {
		meta["nodePort"] = strconv.Itoa(int(sp.NodePort))
	}
	label := "external address"
	if len(addrs) > 0 {
		label = addrs[0]
		if len(addrs) > 1 {
			label += fmt.Sprintf(" (+%d)", len(addrs)-1)
		}
	} else if sp.NodePort != 0 {
		label = "NodePort " + strconv.Itoa(int(sp.NodePort))
	}
	b.define(checker.TopologyNode{ID: ref, Kind: "address", Label: label, Meta: meta})

	st := Step{Name: "External address", Hop: HopAddress, Ref: ref, Status: StatusOK, Detail: strings.Join(parts, "; ") + "."}
	switch {
	case svc.Spec.Type == corev1.ServiceTypeLoadBalancer && len(svc.Status.LoadBalancer.Ingress) == 0:
		st.Reason = ReasonLBPending
		st.Status = StatusWarn
		st.Detail = fmt.Sprintf("LoadBalancer %s/%s has no address yet (pending).", svc.Namespace, svc.Name)
		if sp.NodePort != 0 {
			st.Detail += fmt.Sprintf(" NodePort %d still works on every node.", sp.NodePort)
		} else {
			st.Status = StatusFail
		}
		st.Hint = "Check the load balancer implementation (k3s ServiceLB/klipper, MetalLB, cloud LB) and its events: kubectl describe svc."
	case len(parts) == 0:
		st.Reason = ReasonNoExternalAddress
		st.Status = StatusWarn
		st.Detail = fmt.Sprintf("Entry Service %s/%s is %s only: reachable inside the cluster, not from outside.", svc.Namespace, svc.Name, svc.Spec.Type)
		st.Hint = "Something outside the cluster (tunnel, port-forward, external proxy) must forward to it, or change the Service to LoadBalancer/NodePort."
	}
	b.add(st)
}

// workloadStep reports the gateway/controller workload readiness.
func workloadStep(b *branchBuilder, e entry, what string) {
	if len(e.pods) == 0 {
		b.add(Step{
			Name:   strings.ToUpper(what[:1]) + what[1:] + " workload",
			Hop:    HopWorkload,
			Status: StatusFail,
			Reason: ReasonGatewaySelectorNoPods,
			Detail: fmt.Sprintf("Selector %s matches no pod in any namespace.", selectorString(e.selector)),
			Hint:   "Make sure the gateway/controller deployment runs and its pod labels match the selector.",
		})
		return
	}
	ref := e.workloads[0]
	st := Step{
		Name:   fmt.Sprintf("%s workload %s", strings.ToUpper(what[:1])+what[1:], strings.Join(e.workloads, ", ")),
		Hop:    HopWorkload,
		Ref:    ref,
		Status: StatusOK,
		Detail: fmt.Sprintf("%d/%d pods ready (selector %s).", e.readyPods, len(e.pods), selectorString(e.selector)),
	}
	if e.readyPods == 0 {
		st.Status = StatusFail
		st.Reason = ReasonGatewayWorkloadNotReady
		st.Hint = "Inspect the gateway/controller pods (kubectl describe pod) — CrashLoopBackOff, Pending or failing readiness."
	}
	b.add(st)
}

// backendSteps runs checker.ServiceChain for one backend and maps its
// steps onto graph refs: Service-level hops → the Service node, pod-level
// hops → the backend workload.
func backendSteps(ctx context.Context, w *world, b *branchBuilder, ns, name string, port int32, group string) {
	ch := checker.ServiceChain(ctx, w.kube, ns, name, port)
	svcRef := graphid.Service(ns, name)
	wlRef := ""
	if len(ch.Pods) > 0 {
		wlRef = w.workloadID(&ch.Pods[0])
	}
	for _, s := range ch.Steps {
		ref := svcRef
		switch s.Hop {
		case checker.HopPods, checker.HopPort:
			if wlRef != "" {
				ref = wlRef
			}
		}
		if s.Reason == ReasonBackendServiceMissing {
			b.define(checker.TopologyNode{ID: svcRef, Kind: "service", Label: name, Namespace: ns, Detail: "not found"})
		}
		st := Step{
			Name:   s.Name,
			Hop:    s.Hop,
			Ref:    ref,
			Status: s.EffectiveStatus(),
			Reason: s.Reason,
			Detail: s.Detail,
			Hint:   s.Hint,
			Group:  group,
		}
		// Scaled to 0 is intentional (D-035: neutral, not a failure), but
		// for a host trace it means requests reach no pod: say so as a
		// warning instead of a silent pass.
		if ch.ScaledToZero && ch.Ready == 0 && s.Hop == checker.HopEndpoints {
			st.Status = StatusWarn
			st.Reason = ReasonNoReadyEndpoints
			st.Detail += " Requests for this host reach no pod (the gateway answers 503) until the workload is scaled up."
		}
		b.add(st)
	}
}

// --- small helpers --------------------------------------------------------

func podReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func labelsMatch(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func selectorString(m map[string]string) string {
	if len(m) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func portList(s *corev1.Service) string {
	out := make([]string, 0, len(s.Spec.Ports))
	for _, p := range s.Spec.Ports {
		out = append(out, strconv.Itoa(int(p.Port)))
	}
	return strings.Join(out, ",")
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// weightLabel labels a weighted destination sub-path.
func weightLabel(i, n int, weight int32, target string) string {
	if n <= 1 {
		return ""
	}
	w := ""
	if weight > 0 {
		w = fmt.Sprintf(" · weight %d", weight)
	}
	return fmt.Sprintf("backend %d/%d%s → %s", i+1, n, w, target)
}
