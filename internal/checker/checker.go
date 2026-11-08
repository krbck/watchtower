// Package checker implements the connectivity chain engine.
//
// Phase 1 scope: Service → Pod → (EndpointSlice readiness) → Port chain.
// Next steps: Ingress path, Istio VirtualService (port, rewritePath,
// stripPath), HTTP probe (200/300/500/404/503/502) — see D-010/D-011.
package checker

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Kube is the read surface the chain engine needs
// (narrow interface for testability).
type Kube interface {
	Service(ctx context.Context, namespace, name string) (*corev1.Service, error)
	Services(ctx context.Context, namespace string) ([]corev1.Service, error)
	Pods(ctx context.Context, namespace string, selector labels.Selector) ([]corev1.Pod, error)
	// EndpointSlices returns the slices backing a service (label
	// kubernetes.io/service-name), D-035.
	EndpointSlices(ctx context.Context, namespace, service string) ([]discoveryv1.EndpointSlice, error)
	// DesiredReplicas returns the desired replica count of the workload
	// named name (Deployment, StatefulSet or DaemonSet, in that order);
	// found=false when no such workload exists.
	DesiredReplicas(ctx context.Context, namespace, name string) (desired int32, found bool, err error)
}

// ClientsetKube adapts a client-go clientset to the Kube interface.
type ClientsetKube struct{ C kubernetes.Interface }

// KubeFromConfig derives a Kube from a rest.Config (in-cluster or external).
func KubeFromConfig(cfg *rest.Config) (Kube, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to build clientset: %w", err)
	}
	return ClientsetKube{C: cs}, nil
}

func (k ClientsetKube) Service(ctx context.Context, namespace, name string) (*corev1.Service, error) {
	return k.C.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (k ClientsetKube) Services(ctx context.Context, namespace string) ([]corev1.Service, error) {
	sl, err := k.C.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return sl.Items, nil
}

func (k ClientsetKube) Pods(ctx context.Context, namespace string, selector labels.Selector) ([]corev1.Pod, error) {
	pl, err := k.C.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	return pl.Items, nil
}

func (k ClientsetKube) EndpointSlices(ctx context.Context, namespace, service string) ([]discoveryv1.EndpointSlice, error) {
	sl, err := k.C.DiscoveryV1().EndpointSlices(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=" + service,
	})
	if err != nil {
		return nil, err
	}
	return sl.Items, nil
}

func (k ClientsetKube) DesiredReplicas(ctx context.Context, namespace, name string) (int32, bool, error) {
	apps := k.C.AppsV1()
	if d, err := apps.Deployments(namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		return replicasOrDefault(d.Spec.Replicas), true, nil
	} else if !apierrors.IsNotFound(err) {
		return 0, false, err
	}
	if s, err := apps.StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		return replicasOrDefault(s.Spec.Replicas), true, nil
	} else if !apierrors.IsNotFound(err) {
		return 0, false, err
	}
	if ds, err := apps.DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		return ds.Status.DesiredNumberScheduled, true, nil
	} else if !apierrors.IsNotFound(err) {
		return 0, false, err
	}
	return 0, false, nil
}

// replicasOrDefault returns spec.replicas, defaulting to 1 (API default).
func replicasOrDefault(r *int32) int32 {
	if r == nil {
		return 1
	}
	return *r
}

// Input holds the check parameters.
type Input struct {
	Target      string
	ServiceName string
	Port        int32
}

// Step is a single step of the connectivity chain.
type Step struct {
	Name   string
	OK     bool
	Detail string
	Hint   string // remediation suggestion (on failure)
	// Status refines OK for the host trace (D-038): ok | fail | warn |
	// unknown | skipped. A warn step keeps OK=false where ConnectivityCheck
	// historically failed (e.g. an undeclared numeric targetPort).
	Status string
	// Reason is the machine-readable code (shared P1 vocabulary).
	Reason string
	// Hop names the traffic hop the step belongs to (backend, pods,
	// endpoints, port, ...); the trace maps it to a graph node.
	Hop string
}

// TopologyNode is a node in the connectivity graph; the UI renders it.
type TopologyNode struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"` // client | service | pod | port | app | namespace | deployment | statefulset | daemonset | ingress | ingressclass | gateway | virtualservice | host | address
	Label string `json:"label"`
	// Namespace is optional (cluster-scoped nodes leave it empty).
	Namespace string `json:"namespace,omitempty"`
	OK        *bool  `json:"ok,omitempty"`
	Detail    string `json:"detail,omitempty"`
	// Meta carries structured fields rendered by the UI card (status, node,
	// replicas...).
	Meta map[string]string `json:"meta,omitempty"`
	// Ports is the structured port list rendered as a dedicated block
	// (service: port→targetPort/nodePort; workload: containerPort).
	Ports []PortInfo `json:"ports,omitempty"`
	// Warnings explains configuration issues found on this node (D-037).
	Warnings []NodeWarning `json:"warnings,omitempty"`
}

// NodeWarning is a single explained issue attached to a topology node:
// a stable machine-readable Reason, a human Message and a fix Hint (D-037).
type NodeWarning struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	Hint    string `json:"hint,omitempty"`
	// Items is a long list moved out of Message; shown on demand.
	Items []string `json:"items,omitempty"`
}

// PortInfo describes a port exposed by a service or declared by a container.
type PortInfo struct {
	Port       int32  `json:"port"`
	TargetPort string `json:"targetPort,omitempty"`
	NodePort   int32  `json:"nodePort,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
	Matched    bool   `json:"matched,omitempty"`
	// Reason/Hint explain an unmatched port (D-037): Reason is a
	// machine-readable code, Hint the human explanation plus the fix.
	Reason string `json:"reason,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

// TopologyEdge is a directed edge in the connectivity graph.
type TopologyEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label,omitempty"`
	OK    *bool  `json:"ok,omitempty"`
	// Config marks a configuration relation (e.g. IngressClass → Ingress),
	// not a traffic hop: drawn thin/dashed and ignored by the namespace
	// root computation (D-038).
	Config bool `json:"config,omitempty"`
}

// Topology is the traffic-flow graph derived from a chain run.
type Topology struct {
	Nodes []TopologyNode `json:"nodes,omitempty"`
	Edges []TopologyEdge `json:"edges,omitempty"`
}

// Result is the chain output.
type Result struct {
	Passed   bool
	Steps    []Step
	Topology Topology
}

func boolPtr(b bool) *bool { return &b }

// Run executes the Service → Pod → Port chain.
// Steps are ordered top-down; when a critical step fails the chain is cut
// at that point (everything below is meaningless without the layer above).
// The topology graph mirrors the steps: client → service → pods → ports → app.
//
// Service resolution (explicit, by target name, or discovery) is specific to
// ConnectivityCheck; everything below the Service is the shared service tail
// (serviceTail, also behind ServiceChain for the host trace, D-038).
func Run(ctx context.Context, kube Kube, namespace string, in Input) Result {
	steps := make([]Step, 0)
	add := func(s Step) { steps = append(steps, s) }

	var topo Topology
	nodeIDs := map[string]bool{}
	addNode := func(n TopologyNode) {
		if !nodeIDs[n.ID] {
			nodeIDs[n.ID] = true
			topo.Nodes = append(topo.Nodes, n)
		}
	}
	addEdge := func(e TopologyEdge) { topo.Edges = append(topo.Edges, e) }
	edgeIDs := map[string]bool{}
	addEdgeOnce := func(e TopologyEdge) {
		id := e.From + "→" + e.To
		if !edgeIDs[id] {
			edgeIDs[id] = true
			addEdge(e)
		}
	}

	// Graph anchors: traffic source and the target application.
	addNode(TopologyNode{ID: "client", Kind: "client", Label: "client / traffic"})
	addNode(TopologyNode{ID: "app", Kind: "app", Label: in.Target})

	svcName := in.ServiceName
	discovered := false

	// (1) Service resolution. When no explicit serviceName is given we first
	// try a service named like the target, then discover any service whose
	// selector fronts the target's pods (e.g. target redis-0 → service redis).
	var svc *corev1.Service
	var err error
	if svcName != "" {
		svc, err = kube.Service(ctx, namespace, svcName)
	} else {
		svcName = in.Target
		svc, err = kube.Service(ctx, namespace, svcName)
		if err != nil {
			if found := discoverService(ctx, kube, namespace, in.Target); found != nil {
				svc = found
				svcName = found.Name
				discovered = true
				err = nil
			}
		}
	}
	if err != nil {
		add(Step{
			Name:   fmt.Sprintf("Service: %s (ns=%s)", svcName, namespace),
			OK:     false,
			Status: StatusFail,
			Reason: ReasonBackendServiceMissing,
			Hop:    HopBackend,
			Detail: fmt.Sprintf("service not found: %v", err),
			Hint:   fmt.Sprintf("No Service in namespace %s selects the pods of target %q. Create a Service or set serviceName explicitly.", namespace, in.Target),
		})
		addNode(TopologyNode{ID: "svc", Kind: "service", Label: svcName, OK: boolPtr(false), Detail: "not found"})
		addEdge(TopologyEdge{From: "client", To: "svc", Label: "request", OK: boolPtr(false)})
		addEdge(TopologyEdge{From: "svc", To: "app", Label: "unreachable", OK: boolPtr(false)})
		return Result{Passed: false, Steps: steps, Topology: topo}
	}
	if discovered {
		add(Step{
			Name:   "Service discovery",
			OK:     true,
			Status: StatusOK,
			Hop:    HopBackend,
			Detail: fmt.Sprintf("service %q discovered: its selector fronts target %q", svcName, in.Target),
		})
	}

	ports := servicePortSummary(svc)
	add(Step{
		Name:   fmt.Sprintf("Service: %s (ns=%s) [type=%s]", svcName, namespace, svc.Spec.Type),
		OK:     true,
		Status: StatusOK,
		Hop:    HopBackend,
		Detail: fmt.Sprintf("selector=%v; ports=[%s]", svc.Spec.Selector, strings.Join(ports, ", ")),
	})
	addNode(TopologyNode{
		ID:     "svc",
		Kind:   "service",
		Label:  fmt.Sprintf("%s\n%s", svcName, strings.Join(portLabels(svc), " ")),
		OK:     boolPtr(true),
		Detail: fmt.Sprintf("type=%s selector=%v", svc.Spec.Type, svc.Spec.Selector),
	})
	addEdge(TopologyEdge{From: "client", To: "svc", Label: "request", OK: boolPtr(true)})

	// (2..4) Shared service tail: selector → pods → (target consistency) →
	// pod phase → endpoints ready → containerPort → targetPort.
	opts := tailOptions{scaleTarget: in.Target}
	if in.ServiceName != "" && in.ServiceName != in.Target {
		opts.consistencyTarget = in.Target
	}
	ch := serviceTail(ctx, kube, namespace, svc, opts)
	steps = append(steps, ch.Steps...)
	pass := ch.Passed

	switch ch.cut {
	case cutPodListFailed:
		addNode(TopologyNode{ID: "pod:?", Kind: "pod", Label: "listing failed", OK: boolPtr(false)})
		addEdge(TopologyEdge{From: "svc", To: "pod:?", Label: "selector", OK: boolPtr(false)})
		addEdge(TopologyEdge{From: "pod:?", To: "app", Label: "unknown", OK: boolPtr(false)})
		return Result{Passed: false, Steps: steps, Topology: topo}
	case cutScaledToZero:
		setNodeMeta(&topo, "svc", "endpoints", "scaled to 0")
		// Neutral (OK unset) graph: nothing is broken, nothing runs.
		addNode(TopologyNode{ID: "pod:none", Kind: "pod", Label: "scaled to 0", Detail: "no pods expected"})
		addEdge(TopologyEdge{From: "svc", To: "pod:none", Label: selectorLabel(svc)})
		addEdge(TopologyEdge{From: "pod:none", To: "app", Label: "scaled to 0"})
		return Result{Passed: true, Steps: steps, Topology: topo}
	case cutNoPods:
		addNode(TopologyNode{ID: "pod:none", Kind: "pod", Label: "no pods matched", OK: boolPtr(false)})
		addEdge(TopologyEdge{From: "svc", To: "pod:none", Label: selectorLabel(svc), OK: boolPtr(false)})
		addEdge(TopologyEdge{From: "pod:none", To: "app", Label: "unreachable", OK: boolPtr(false)})
		return Result{Passed: false, Steps: steps, Topology: topo}
	}

	if ch.EndpointsErr == nil {
		label := fmt.Sprintf("%d ready / %d notReady", ch.Ready, ch.NotReady)
		if ch.ScaledToZero && ch.Ready == 0 {
			label += " · scaled to 0"
		}
		setNodeMeta(&topo, "svc", "endpoints", label)
	}

	// Graph: pods, their readiness and the declared containerPorts.
	for i := range ch.Pods {
		p := &ch.Pods[i]
		podRunning := p.Status.Phase == corev1.PodRunning
		podReady := ch.PodReady(p.Name)
		detail := string(p.Status.Phase)
		if ch.EndpointsErr == nil && !podReady {
			detail += " · not a ready endpoint"
		}
		addNode(TopologyNode{ID: "pod:" + p.Name, Kind: "pod", Label: p.Name, OK: boolPtr(podRunning && podReady), Detail: detail})
		addEdge(TopologyEdge{From: "svc", To: "pod:" + p.Name, Label: "selector", OK: boolPtr(podReady)})
		addEdge(TopologyEdge{From: "pod:" + p.Name, To: "app", Label: "runs", OK: boolPtr(podRunning)})
		for _, c := range p.Spec.Containers {
			for _, cp := range c.Ports {
				addNode(TopologyNode{
					ID:     fmt.Sprintf("cport:%d", cp.ContainerPort),
					Kind:   "port",
					Label:  fmt.Sprintf(":%d", cp.ContainerPort),
					OK:     boolPtr(true),
					Detail: fmt.Sprintf("containerPort declared by %s", c.Name),
				})
				addEdgeOnce(TopologyEdge{From: "pod:" + p.Name, To: fmt.Sprintf("cport:%d", cp.ContainerPort), Label: "containerPort", OK: boolPtr(true)})
				addEdgeOnce(TopologyEdge{From: fmt.Sprintf("cport:%d", cp.ContainerPort), To: "app", Label: "binds", OK: boolPtr(true)})
			}
		}
	}

	// Graph: targetPort resolution per service port. The port node exists
	// only if declared by a container; an undeclared targetPort gets a red
	// "ghost" node.
	for _, pc := range ch.Ports {
		addNode(TopologyNode{
			ID:     pc.NodeID,
			Kind:   "port",
			Label:  ":" + strings.TrimPrefix(pc.NodeID, "cport:"),
			OK:     boolPtr(pc.Matched),
			Detail: "referenced by Service",
		})
		addEdge(TopologyEdge{From: "svc", To: pc.NodeID, Label: pc.Label, OK: boolPtr(pc.Matched)})
		if pc.Warning != nil {
			// Explain the issue on the service node and on the ghost port
			// node (D-037).
			addNodeWarning(&topo, "svc", *pc.Warning)
			addNodeWarning(&topo, pc.NodeID, *pc.Warning)
		}
	}

	// (5) Optional explicit target port check — the port the app listens on
	// must be defined either as a Service port or as a targetPort.
	if in.Port != 0 {
		found := false
		where := ""
		for _, sp := range svc.Spec.Ports {
			if sp.Port == in.Port {
				found = true
				where = "service port"
			}
			if int32(sp.TargetPort.IntValue()) == in.Port {
				found = true
				where = "targetPort"
			}
		}
		st := Step{
			Name:   fmt.Sprintf("Target port %d defined in Service", in.Port),
			OK:     found,
			Status: statusOf(found),
			Hop:    HopPort,
			Detail: fmt.Sprintf("service ports: %s", strings.Join(ports, ", ")),
			Hint:   "The port the app listens on must be defined as a Service port or targetPort.",
		}
		if found {
			st.Detail += fmt.Sprintf(" — matched as %s", where)
		} else {
			st.Reason = ReasonBackendPortMissing
			pass = false
		}
		add(st)
	}

	// The app node reflects the overall outcome.
	setNodeOK(&topo, "app", pass)

	return Result{Passed: pass, Steps: steps, Topology: topo}
}

// resolveTargetPort resolves a ServicePort's targetPort against the pods'
// containerPorts. Returns the graph node id, a display label and whether
// the targetPort resolves to a declared containerPort.
func resolveTargetPort(sp corev1.ServicePort, pods []corev1.Pod) (id, label string, matched bool) {
	if sp.TargetPort.Type == 1 { // named port
		for i := range pods {
			for _, c := range pods[i].Spec.Containers {
				for _, cp := range c.Ports {
					if cp.Name == sp.TargetPort.StrVal {
						return fmt.Sprintf("cport:%d", cp.ContainerPort),
							fmt.Sprintf("%d → %s (%d)", sp.Port, sp.TargetPort.StrVal, cp.ContainerPort), true
					}
				}
			}
		}
		return "cport:" + sp.TargetPort.StrVal,
			fmt.Sprintf("%d → %s", sp.Port, sp.TargetPort.StrVal), false
	}
	tp := sp.TargetPort.IntValue()
	if tp == 0 {
		tp = int(sp.Port) // targetPort defaults to port when omitted
	}
	for i := range pods {
		for _, c := range pods[i].Spec.Containers {
			for _, cp := range c.Ports {
				if int(cp.ContainerPort) == tp {
					return fmt.Sprintf("cport:%d", tp),
						fmt.Sprintf("%d → %d", sp.Port, tp), true
				}
			}
		}
	}
	return fmt.Sprintf("cport:%d", tp), fmt.Sprintf("%d → %d", sp.Port, tp), false
}

// scaledToZeroStep is the informative (non-failing) endpoints step for an
// intentionally scaled-to-zero target workload.
func scaledToZeroStep(target string) Step {
	return Step{
		Name:   "Endpoints ready",
		OK:     true,
		Status: StatusOK,
		Hop:    HopEndpoints,
		Detail: "workload scaled to 0 — no endpoints expected",
		Hint:   fmt.Sprintf("If %q should receive traffic, scale it up (kubectl scale --replicas=1 ...).", target),
	}
}

// endpointSummary is the readiness view of a service's EndpointSlices.
type endpointSummary struct {
	ready, notReady int
	readyPods       map[string]bool // pod name → ready endpoint
}

// summarizeEndpoints counts endpoints by readiness, de-duplicating across
// slices (dual-stack services publish one slice per IP family). A nil Ready
// condition means ready (API contract).
func summarizeEndpoints(slices []discoveryv1.EndpointSlice) endpointSummary {
	out := endpointSummary{readyPods: map[string]bool{}}
	seen := map[string]bool{}
	for i := range slices {
		for _, ep := range slices[i].Endpoints {
			id := ""
			if ep.TargetRef != nil && ep.TargetRef.Name != "" {
				id = ep.TargetRef.Kind + "/" + ep.TargetRef.Name
			} else if len(ep.Addresses) > 0 {
				id = "addr/" + ep.Addresses[0]
			}
			if id != "" {
				if seen[id] {
					continue
				}
				seen[id] = true
			}
			ready := ep.Conditions.Ready == nil || *ep.Conditions.Ready
			if ready {
				out.ready++
				if ep.TargetRef != nil && ep.TargetRef.Kind == "Pod" {
					out.readyPods[ep.TargetRef.Name] = true
				}
			} else {
				out.notReady++
			}
		}
	}
	return out
}

// setNodeMeta sets a meta field on a node.
func setNodeMeta(topo *Topology, id, key, value string) {
	for i := range topo.Nodes {
		if topo.Nodes[i].ID == id {
			if topo.Nodes[i].Meta == nil {
				topo.Nodes[i].Meta = map[string]string{}
			}
			topo.Nodes[i].Meta[key] = value
		}
	}
}

// setNodeOK sets the OK flag of a node only when it has not been set yet
// (first declaration wins).
// addNodeWarning appends an explained warning to the node with the given ID.
func addNodeWarning(topo *Topology, id string, w NodeWarning) {
	for i := range topo.Nodes {
		if topo.Nodes[i].ID != id {
			continue
		}
		for _, have := range topo.Nodes[i].Warnings {
			if reflect.DeepEqual(have, w) {
				return
			}
		}
		topo.Nodes[i].Warnings = append(topo.Nodes[i].Warnings, w)
		return
	}
}

// targetPortWarningFor builds the explained warning for an unresolved
// targetPort; the target pods are reported as one backend named after the
// check target (they share a pod template).
func targetPortWarningFor(sp corev1.ServicePort, target string, pods []corev1.Pod) NodeWarning {
	backend := BackendPorts{Name: target}
	for i := range pods {
		for _, c := range pods[i].Spec.Containers {
			for _, cp := range c.Ports {
				backend.Ports = append(backend.Ports, DeclaredPort{Name: cp.Name, Port: cp.ContainerPort})
			}
		}
	}
	if sp.TargetPort.Type == 1 { // named port
		return TargetPortWarning(true, sp.TargetPort.StrVal, 0, []BackendPorts{backend})
	}
	num := int32(sp.TargetPort.IntValue())
	if num == 0 {
		num = sp.Port
	}
	return TargetPortWarning(false, "", num, []BackendPorts{backend})
}

func setNodeOK(topo *Topology, id string, ok bool) {
	for i := range topo.Nodes {
		if topo.Nodes[i].ID == id && topo.Nodes[i].OK == nil {
			topo.Nodes[i].OK = &ok
		}
	}
}

func portLabels(svc *corev1.Service) []string {
	out := make([]string, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		out = append(out, fmt.Sprintf(":%d", p.Port))
	}
	return out
}

func selectorLabel(svc *corev1.Service) string {
	pairs := make([]string, 0, len(svc.Spec.Selector))
	for k, v := range svc.Spec.Selector {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

// discoverService finds a service whose selector fronts pods that belong to
// the target workload.
func discoverService(ctx context.Context, kube Kube, namespace, target string) *corev1.Service {
	svcs, err := kube.Services(ctx, namespace)
	if err != nil {
		return nil
	}
	for i := range svcs {
		svc := &svcs[i]
		if len(svc.Spec.Selector) == 0 {
			continue
		}
		pods, err := kube.Pods(ctx, namespace, labels.Set(svc.Spec.Selector).AsSelector())
		if err != nil {
			continue
		}
		for j := range pods {
			if podMatchesTarget(&pods[j], target) {
				return svc
			}
		}
	}
	return nil
}

// PodMatchesTarget reports whether a pod belongs to the target workload
// (same rules as Service discovery; used by the upstream API, B-001).
func PodMatchesTarget(pod *corev1.Pod, target string) bool { return podMatchesTarget(pod, target) }

// podMatchesTarget reports whether a pod belongs to the given target
// workload (owner reference, name prefix or common app labels).
func podMatchesTarget(pod *corev1.Pod, target string) bool {
	if pod.Name == target {
		return true
	}
	for _, ref := range pod.OwnerReferences {
		switch ref.Kind {
		case "ReplicaSet":
			if strings.HasPrefix(ref.Name, target+"-") {
				return true
			}
		case "StatefulSet", "DaemonSet", "Job":
			if ref.Name == target {
				return true
			}
		}
	}
	if strings.HasPrefix(pod.Name, target+"-") {
		return true
	}
	if pod.Labels["app"] == target || pod.Labels["app.kubernetes.io/name"] == target {
		return true
	}
	return false
}

func podPhases(pods []corev1.Pod) []string {
	phases := make([]string, 0, len(pods))
	for i := range pods {
		phases = append(phases, string(pods[i].Status.Phase))
	}
	return phases
}
