package checker

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
)

// Step statuses (D-038). Empty Status means "derive from OK".
const (
	StatusOK      = "ok"
	StatusFail    = "fail"
	StatusWarn    = "warn"
	StatusUnknown = "unknown"
	StatusSkipped = "skipped"
)

// Hops of the service tail (D-038 step order: backend Service + port →
// EndpointSlice ready → pods → targetPort↔containerPort).
const (
	HopBackend   = "backend"
	HopEndpoints = "endpoints"
	HopPods      = "pods"
	HopPort      = "port"
)

// Reasons emitted by the service tail (D-038 vocabulary; stable API codes).
const (
	ReasonBackendServiceMissing = "backendServiceMissing"
	ReasonBackendPortMissing    = "backendPortMissing"
	ReasonNoReadyEndpoints      = "noReadyEndpoints"
)

func statusOf(ok bool) string {
	if ok {
		return StatusOK
	}
	return StatusFail
}

// EffectiveStatus returns s.Status, or ok/fail derived from s.OK.
func (s Step) EffectiveStatus() string {
	if s.Status != "" {
		return s.Status
	}
	return statusOf(s.OK)
}

// WorkloadLister is an optional Kube extension: when the Kube implements
// it, the service tail detects "every selected workload is scaled to 0"
// without knowing a target name (host trace, D-038).
type WorkloadLister interface {
	// SelectedWorkloads returns the desired replicas of every workload in
	// namespace whose pod template labels are selected by selector.
	SelectedWorkloads(ctx context.Context, namespace string, selector map[string]string) ([]int32, error)
}

// PortCheck is the targetPort ↔ containerPort outcome for one Service port.
type PortCheck struct {
	Port    corev1.ServicePort
	NodeID  string // "cport:<n>" (or "cport:<name>" when unresolved)
	Label   string
	Matched bool
	Warning *NodeWarning
}

// chainCut tells where the service tail stopped.
type chainCut int

const (
	cutNone chainCut = iota
	cutServiceMissing
	cutPortMissing
	cutPodListFailed
	cutNoPods
	cutScaledToZero
	cutNoSelector
)

// ChainResult is the outcome of the service tail: the steps plus the facts
// they were derived from, so callers can build their own graph (checker:
// local IDs; trace: mapper IDs).
type ChainResult struct {
	Service *corev1.Service
	// Pods are the pods selected by the Service, sorted by name.
	Pods []corev1.Pod
	// Ready/NotReady count the de-duplicated endpoints (D-035).
	Ready, NotReady int
	readyPods       map[string]bool
	// EndpointsErr is set when the EndpointSlices could not be listed.
	EndpointsErr error
	// ScaledToZero: every selected workload has desired replicas 0.
	ScaledToZero bool
	// Ports holds the targetPort checks (all ports, or the requested one).
	Ports  []PortCheck
	Steps  []Step
	Passed bool
	cut    chainCut
}

// PodReady reports whether a selected pod receives traffic: it is a ready
// endpoint (unknown readiness — slices unreadable — or a scaled-to-zero
// target count as ready, matching the ConnectivityCheck graph).
func (r *ChainResult) PodReady(name string) bool {
	return r.EndpointsErr != nil || r.ScaledToZero || r.readyPods[name]
}

// Cut reports whether the chain stopped before the targetPort checks.
func (r *ChainResult) Cut() bool { return r.cut != cutNone }

// tailOptions tunes the shared tail for its two callers.
type tailOptions struct {
	// port limits the targetPort check to one Service port (0 = all).
	port int32
	// scaleTarget is the ConnectivityCheck target name used for the
	// scaled-to-zero lookup; empty = WorkloadLister (when implemented).
	scaleTarget string
	// consistencyTarget enables the "Target ↔ service consistency" step
	// (ConnectivityCheck with an explicit, differently named service).
	consistencyTarget string
}

// ServiceChain runs the service tail for a Service referenced by name and
// port (host trace, D-038): Service exists → port exists → selector → pods
// → pod phase → endpoints ready (scaled-to-0 neutral) → containerPort →
// targetPort↔containerPort for that port. port=0 checks every port.
func ServiceChain(ctx context.Context, kube Kube, namespace, name string, port int32) ChainResult {
	svc, err := kube.Service(ctx, namespace, name)
	if err != nil {
		st := Step{
			Name:   fmt.Sprintf("Backend Service %s/%s", namespace, name),
			Hop:    HopBackend,
			Detail: fmt.Sprintf("service lookup failed: %v", err),
			Status: StatusUnknown,
		}
		if apierrors.IsNotFound(err) {
			st.Status = StatusFail
			st.Reason = ReasonBackendServiceMissing
			st.Detail = fmt.Sprintf("Service %s/%s does not exist.", namespace, name)
			st.Hint = "Create the Service or fix the backend reference (name and namespace)."
		}
		return ChainResult{Steps: []Step{st}, cut: cutServiceMissing}
	}
	steps := []Step{{
		Name:   fmt.Sprintf("Backend Service %s/%s [type=%s]", namespace, name, svc.Spec.Type),
		OK:     true,
		Status: StatusOK,
		Hop:    HopBackend,
		Detail: fmt.Sprintf("selector=%s; ports=[%s]", selectorLabel(svc), strings.Join(servicePortSummary(svc), ", ")),
	}}
	if port != 0 {
		found := false
		for _, sp := range svc.Spec.Ports {
			if sp.Port == port {
				found = true
			}
		}
		if !found {
			steps = append(steps, Step{
				Name:   fmt.Sprintf("Backend port %d", port),
				Status: StatusFail,
				Reason: ReasonBackendPortMissing,
				Hop:    HopBackend,
				Detail: fmt.Sprintf("port %d is not a port of Service %s/%s (ports: %s).", port, namespace, name, strings.Join(portLabels(svc), " ")),
				Hint:   "Point the route at one of the Service ports, or add the port to the Service.",
			})
			return ChainResult{Service: svc, Steps: steps, cut: cutPortMissing}
		}
	}
	if len(svc.Spec.Selector) == 0 {
		// Manual endpoints or ExternalName: only readiness can be checked.
		res := ChainResult{Service: svc, cut: cutNoSelector}
		slices, err := kube.EndpointSlices(ctx, namespace, name)
		eps := summarizeEndpoints(slices)
		res.Ready, res.NotReady, res.readyPods, res.EndpointsErr = eps.ready, eps.notReady, eps.readyPods, err
		w := NoSelectorWarning()
		steps = append(steps, Step{
			Name:   "Service selector",
			Status: StatusWarn,
			Reason: w.Reason,
			Hop:    HopPods,
			Detail: w.Message + "; pods and targetPort cannot be checked.",
			Hint:   w.Hint,
		})
		steps = append(steps, endpointsStep(namespace, name, eps, err, false, ""))
		res.Steps = steps
		res.Passed = err == nil && eps.ready > 0
		return res
	}
	tail := serviceTail(ctx, kube, namespace, svc, tailOptions{port: port})
	tail.Steps = append(steps, tail.Steps...)
	return tail
}

// serviceTail is the chain below a resolved Service (shared by Run and
// ServiceChain). Step names and order are ConnectivityCheck's: selector →
// [consistency] → pod phase → endpoints ready → containerPort declaration →
// targetPort match(es).
func serviceTail(ctx context.Context, kube Kube, namespace string, svc *corev1.Service, opts tailOptions) ChainResult {
	res := ChainResult{Service: svc, Passed: true}
	add := func(s Step) { res.Steps = append(res.Steps, s) }
	svcName := svc.Name

	// Selector → Pod matching
	selector := labels.Set(svc.Spec.Selector).AsSelector()
	pods, err := kube.Pods(ctx, namespace, selector)
	if err != nil {
		add(Step{
			Name:   "Service selector → Pod match",
			OK:     false,
			Status: StatusUnknown,
			Hop:    HopPods,
			Detail: fmt.Sprintf("failed to list pods: %v", err),
			Hint:   "Check RBAC permissions (pods get/list/watch).",
		})
		res.Passed = false
		res.cut = cutPodListFailed
		return res
	}
	sort.SliceStable(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	res.Pods = pods

	// An intentionally scaled-to-zero target is not a failure: no pods and
	// no endpoints are expected. Lookup errors are treated as "unknown"
	// (normal chain semantics apply).
	res.ScaledToZero = scaledToZero(ctx, kube, namespace, svc, opts.scaleTarget)
	scaleName := opts.scaleTarget
	if scaleName == "" {
		scaleName = svcName
	}
	if len(pods) == 0 && res.ScaledToZero {
		add(Step{
			Name:   "Service selector → Pod match",
			OK:     true,
			Status: StatusOK,
			Hop:    HopPods,
			Detail: fmt.Sprintf("selector %v selects no pods: workload %q is scaled to 0.", svc.Spec.Selector, scaleName),
		})
		add(scaledToZeroStep(scaleName))
		res.cut = cutScaledToZero
		return res
	}
	if len(pods) == 0 {
		add(Step{
			Name:   "Service selector → Pod match",
			OK:     false,
			Status: StatusFail,
			Reason: ReasonNoBackend,
			Hop:    HopPods,
			Detail: fmt.Sprintf("selector %v selects no pods.", svc.Spec.Selector),
			Hint:   "Compare pod labels with the Service selector (most common mistake: deployment label differs from service selector).",
		})
		res.Passed = false
		res.cut = cutNoPods
		return res
	}
	podNames := make([]string, 0, len(pods))
	for _, p := range pods {
		podNames = append(podNames, p.Name)
	}
	add(Step{
		Name:   "Service selector → Pod match",
		OK:     true,
		Status: StatusOK,
		Hop:    HopPods,
		Detail: fmt.Sprintf("%d pods selected: %v", len(pods), podNames),
	})

	// Target ↔ service consistency: the selected pods must belong to the
	// target workload. Catches stale or mismatched serviceName (e.g. a
	// target of one gateway paired with another gateway's service).
	if t := opts.consistencyTarget; t != "" {
		matched := 0
		for i := range pods {
			if podMatchesTarget(&pods[i], t) {
				matched++
			}
		}
		ok := matched > 0
		hint := ""
		if !ok {
			hint = fmt.Sprintf("Service %q selects pods of another workload. Target and service look mismatched — pick the service that fronts %q.", svcName, t)
		}
		add(Step{
			Name:   "Target ↔ service consistency",
			OK:     ok,
			Status: statusOf(ok),
			Hop:    HopPods,
			Detail: fmt.Sprintf("%d/%d selected pods belong to target %q", matched, len(pods), t),
			Hint:   hint,
		})
		if !ok {
			res.Passed = false
		}
	}

	// Endpoint readiness (D-035) is resolved before the pod phase so pods
	// can be marked; its step is reported after "Pod phase".
	slices, epErr := kube.EndpointSlices(ctx, namespace, svcName)
	eps := summarizeEndpoints(slices)
	res.Ready, res.NotReady, res.readyPods, res.EndpointsErr = eps.ready, eps.notReady, eps.readyPods, epErr

	// Pod phase + containerPort declarations
	running := 0
	declared := make([]string, 0)
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase == corev1.PodRunning {
			running++
		}
		for _, c := range p.Spec.Containers {
			for _, cp := range c.Ports {
				declared = append(declared, fmt.Sprintf("%s:%d", p.Name, cp.ContainerPort))
			}
		}
	}
	if running == 0 {
		add(Step{
			Name:   "Pod phase",
			OK:     false,
			Status: StatusFail,
			Hop:    HopPods,
			Detail: fmt.Sprintf("none of the %d selected pods is Running (phases: %v).", len(pods), podPhases(pods)),
			Hint:   "Inspect pod events with kubectl describe pod (CrashLoopBackOff, Pending, ImagePullBackOff...).",
		})
		res.Passed = false
	} else {
		add(Step{
			Name:   "Pod phase",
			OK:     true,
			Status: StatusOK,
			Hop:    HopPods,
			Detail: fmt.Sprintf("%d/%d pods Running.", running, len(pods)),
		})
	}

	// Endpoints ready: selector match is not a traffic guarantee —
	// NotReady pods are removed from the service endpoints (D-035).
	epStep := endpointsStep(namespace, svcName, eps, epErr, res.ScaledToZero, scaleName)
	add(epStep)
	if !epStep.OK {
		res.Passed = false
	}

	if len(declared) == 0 {
		add(Step{
			Name: "containerPort declaration",
			OK:   false,
			// Informational for the trace: a numeric targetPort works
			// without a declared containerPort (D-037 nuance).
			Status: StatusWarn,
			Hop:    HopPort,
			Detail: "no containerPort declared in pod specs.",
			Hint:   "containerPort is required for Service targetPort matching; add containerPorts to the deployment.",
		})
		res.Passed = false
	} else {
		add(Step{
			Name:   "containerPort declaration",
			OK:     true,
			Status: StatusOK,
			Hop:    HopPort,
			Detail: fmt.Sprintf("declared ports: %v", declared),
		})
	}

	// targetPort ↔ containerPort consistency
	target := opts.scaleTarget
	if target == "" {
		target = svcName
	}
	for _, sp := range svc.Spec.Ports {
		if opts.port != 0 && sp.Port != opts.port {
			continue
		}
		id, lbl, matched := resolveTargetPort(sp, pods)
		pc := PortCheck{Port: sp, NodeID: id, Label: lbl, Matched: matched}
		st := Step{
			Name:   fmt.Sprintf("Service port %d → targetPort %s match", sp.Port, sp.TargetPort.String()),
			OK:     matched,
			Status: StatusOK,
			Hop:    HopPort,
			Detail: fmt.Sprintf("targetPort %s resolves to %s — match.", sp.TargetPort.String(), strings.TrimPrefix(id, "cport:")),
		}
		if !matched {
			w := targetPortWarningFor(sp, target, pods)
			pc.Warning = &w
			st.Detail = fmt.Sprintf("targetPort %s does not resolve: no container declares port %s",
				sp.TargetPort.String(), strings.TrimPrefix(id, "cport:"))
			st.Hint = w.Hint
			st.Reason = w.Reason
			// An undeclared numeric port still works if the process
			// listens; an unresolved port name is a real outage.
			st.Status = StatusWarn
			if w.Reason == ReasonNamedPortUnresolved {
				st.Status = StatusFail
			}
			res.Passed = false
		}
		res.Ports = append(res.Ports, pc)
		add(st)
	}
	return res
}

// endpointsStep builds the "Endpoints ready" step.
func endpointsStep(namespace, svcName string, eps endpointSummary, epErr error, scaled bool, scaleName string) Step {
	switch {
	case scaled && eps.ready == 0:
		return scaledToZeroStep(scaleName)
	case epErr != nil:
		return Step{
			Name:   "Endpoints ready",
			OK:     false,
			Status: StatusUnknown,
			Hop:    HopEndpoints,
			Detail: fmt.Sprintf("failed to list EndpointSlices: %v", epErr),
			Hint:   "Check RBAC permissions (discovery.k8s.io endpointslices get/list/watch).",
		}
	case eps.ready == 0:
		return Step{
			Name:   "Endpoints ready",
			OK:     false,
			Status: StatusFail,
			Reason: ReasonNoReadyEndpoints,
			Hop:    HopEndpoints,
			Detail: fmt.Sprintf("service %s has no ready endpoints (%d ready / %d notReady).", svcName, eps.ready, eps.notReady),
			Hint: fmt.Sprintf("Pods are selected but not Ready: check the readiness probe and pod conditions "+
				"(kubectl describe pod; kubectl get endpointslices -n %s -l %s=%s).", namespace, discoveryv1.LabelServiceName, svcName),
		}
	}
	return Step{
		Name:   "Endpoints ready",
		OK:     true,
		Status: StatusOK,
		Hop:    HopEndpoints,
		Detail: fmt.Sprintf("%d ready / %d notReady endpoints.", eps.ready, eps.notReady),
	}
}

// scaledToZero decides whether an empty backend is intentional: by target
// name (ConnectivityCheck) or, without one, by every workload selected by
// the Service having desired replicas 0 (WorkloadLister).
func scaledToZero(ctx context.Context, kube Kube, namespace string, svc *corev1.Service, target string) bool {
	if target != "" {
		desired, found, err := kube.DesiredReplicas(ctx, namespace, target)
		return err == nil && found && desired == 0
	}
	wl, ok := kube.(WorkloadLister)
	if !ok {
		return false
	}
	desired, err := wl.SelectedWorkloads(ctx, namespace, svc.Spec.Selector)
	if err != nil || len(desired) == 0 {
		return false
	}
	for _, d := range desired {
		if d != 0 {
			return false
		}
	}
	return true
}

// servicePortSummary renders "port=80→targetPort=8080" per port.
func servicePortSummary(svc *corev1.Service) []string {
	out := make([]string, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		out = append(out, fmt.Sprintf("port=%d→targetPort=%s", p.Port, p.TargetPort.String()))
	}
	return out
}

// CountEndpoints returns the de-duplicated ready / notReady endpoint counts
// of a service's EndpointSlices (D-035 semantics).
func CountEndpoints(slices []discoveryv1.EndpointSlice) (ready, notReady int) {
	s := summarizeEndpoints(slices)
	return s.ready, s.notReady
}
