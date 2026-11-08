package topologymapper

import (
	"context"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/krbck/watchtower/internal/checker"
	"github.com/krbck/watchtower/internal/graphid"
	"github.com/krbck/watchtower/internal/integrations/istio"
)

// BuildOptions carries the optional inputs of a graph build.
type BuildOptions struct {
	// Istio is the Istio integration snapshot (nil = no Istio, D-037).
	Istio *istio.Snapshot
	// Skip lists watched types (typeName) whose informer is not synced;
	// they are treated as empty instead of blocking on a cached List.
	Skip map[string]bool
}

// Build constructs the whole-cluster graph from cached objects.
// faz-1 scope: namespaces, workloads (deployments, statefulsets, daemonsets),
// services, standalone pods, ingresses. Ports are rendered inside their owner
// card (service/workload/pod), and pods owned by a workload are folded into
// that workload card — keeps the map readable (D-017, D-035).
// Edges:
//   - namespace → chain root (D-032)
//   - service → workload (selector match; ok=false when the service has
//     EndpointSlices but zero ready endpoints, D-035)
//   - service → pod (selector match, standalone pods only)
//   - ingress → service (backend)
//   - Istio (D-037): workload → gateway ("serves"), gateway → virtualservice
//     ("binds"), virtualservice → service ("routes")
//
// Every list is sorted by namespace/name so the output is deterministic —
// the mapper fingerprints it to detect real changes (D-034).
func Build(ctx context.Context, c client.Client) (*Graph, error) {
	return BuildWith(ctx, c, BuildOptions{})
}

// BuildWith is Build with optional inputs (Istio snapshot, skipped types).
func BuildWith(ctx context.Context, c client.Client, opts BuildOptions) (*Graph, error) {
	b := newBuilder()
	list := func(name string, l client.ObjectList) error {
		if opts.Skip[name] {
			return nil
		}
		return c.List(ctx, l)
	}

	var nsList corev1.NamespaceList
	if err := list("Namespace", &nsList); err != nil {
		return nil, err
	}
	nsExists := make(map[string]bool, len(nsList.Items))
	for i := range nsList.Items {
		nsExists[nsList.Items[i].Name] = true
	}
	sortObjs(nsList.Items)
	for i := range nsList.Items {
		ns := &nsList.Items[i]
		if isSystemNamespace(ns.Name) {
			continue
		}
		b.addNode(checker.TopologyNode{
			ID:     "ns:" + ns.Name,
			Kind:   "namespace",
			Label:  ns.Name,
			Detail: string(ns.Status.Phase),
			Meta:   map[string]string{"status": string(ns.Status.Phase)},
		})
	}

	var podList corev1.PodList
	if err := list("Pod", &podList); err != nil {
		return nil, err
	}
	sortObjs(podList.Items)

	var depList appsv1.DeploymentList
	if err := list("Deployment", &depList); err != nil {
		return nil, err
	}
	var stsList appsv1.StatefulSetList
	if err := list("StatefulSet", &stsList); err != nil {
		return nil, err
	}
	var dsList appsv1.DaemonSetList
	if err := list("DaemonSet", &dsList); err != nil {
		return nil, err
	}
	workloads := collectWorkloads(depList.Items, stsList.Items, dsList.Items)
	byID := make(map[string]*workload, len(workloads))
	for i := range workloads {
		byID[workloads[i].id] = &workloads[i]
	}

	// Pods: workload-owned pods are folded into their workload card (count
	// only); standalone pods stay as nodes.
	type podCount struct{ running, total int }
	counts := map[string]*podCount{}
	standalone := make([]*corev1.Pod, 0)
	for i := range podList.Items {
		pod := &podList.Items[i]
		if isSystemNamespace(pod.Namespace) {
			continue
		}
		if ownerID := podOwnerID(pod); ownerID != "" && byID[ownerID] != nil {
			cnt := counts[ownerID]
			if cnt == nil {
				cnt = &podCount{}
				counts[ownerID] = cnt
			}
			cnt.total++
			if pod.Status.Phase == corev1.PodRunning {
				cnt.running++
			}
			// Live labels: controllers may label pods at runtime beyond
			// the template (e.g. longhorn), and Services select those.
			byID[ownerID].addLiveLabels(pod.Labels)
			continue
		}
		standalone = append(standalone, pod)
	}

	for i := range workloads {
		w := &workloads[i]
		meta := map[string]string{
			"replicas": w.replicas,
			"selector": selectorString(w.selector),
		}
		if cnt := counts[w.id]; cnt != nil {
			meta["pods"] = itoa(cnt.running) + "/" + itoa(cnt.total) + " running"
		}
		b.addNode(checker.TopologyNode{
			ID:        w.id,
			Kind:      w.kind,
			Label:     w.name,
			Namespace: w.namespace,
			Detail:    w.replicas,
			Meta:      meta,
			Ports:     workloadPorts(w.containers),
		})
	}

	var sliceList discoveryv1.EndpointSliceList
	if err := list("EndpointSlice", &sliceList); err != nil {
		return nil, err
	}
	endpoints := endpointStatsByService(sliceList.Items)

	var svcList corev1.ServiceList
	if err := list("Service", &svcList); err != nil {
		return nil, err
	}
	sortObjs(svcList.Items)
	// noTraffic marks services that select backends but have zero ready
	// endpoints: their "selects" edges are flagged ok=false (D-035).
	noTraffic := map[string]bool{}
	for i := range svcList.Items {
		svc := &svcList.Items[i]
		if isSystemNamespace(svc.Namespace) {
			continue
		}
		svcID := b.svcID(svc.Namespace, svc.Name)
		// Selection is computed once per service and reused by the
		// scaled-to-zero check, the port matching and the edges.
		sel := selectBackends(svc, workloads, standalone)
		meta := map[string]string{
			"type":     string(svc.Spec.Type),
			"selector": selectorString(svc.Spec.Selector),
		}
		// Selector-less services (manually managed endpoints) are shown
		// but never flagged.
		if st := endpoints[svc.Namespace+"/"+svc.Name]; st != nil {
			meta["endpoints"] = itoa(st.ready) + " ready / " + itoa(st.notReady) + " notReady"
			if len(svc.Spec.Selector) > 0 && st.ready == 0 {
				// Intentionally scaled-to-zero backends are not a failure:
				// stay neutral and say so on the card.
				if sel.allScaledToZero() {
					meta["endpoints"] += " · scaled to 0"
				} else {
					noTraffic[svcID] = true
				}
			}
		}
		if ext := externalAddresses(svc); ext != "" {
			meta["external"] = ext
		}
		ports, warnings := servicePorts(svc, sel.backendPorts())
		b.addNode(checker.TopologyNode{
			ID:        svcID,
			Kind:      "service",
			Label:     svc.Name,
			Namespace: svc.Namespace,
			Detail:    "type=" + string(svc.Spec.Type),
			Meta:      meta,
			Ports:     ports,
			Warnings:  warnings,
		})

		// Service → workload: the service selector selects the same pods as
		// the workload's pod-template selector.
		for _, w := range sel.workloads {
			b.addEdge(selectsEdge(svcID, w.id, noTraffic[svcID]))
		}
	}

	for _, pod := range standalone {
		podID := b.podID(pod.Namespace, pod.Name)
		running := pod.Status.Phase == corev1.PodRunning
		ok := running
		b.addNode(checker.TopologyNode{
			ID:        podID,
			Kind:      "pod",
			Label:     pod.Name,
			Namespace: pod.Namespace,
			OK:        &ok,
			Detail:    string(pod.Status.Phase),
			Meta: map[string]string{
				"status": string(pod.Status.Phase),
				"node":   pod.Spec.NodeName,
			},
			Ports: workloadPorts(pod.Spec.Containers),
		})

		for j := range svcList.Items {
			svc := &svcList.Items[j]
			if svc.Namespace != pod.Namespace || isSystemNamespace(svc.Namespace) {
				continue
			}
			if selectorMatches(svc.Spec.Selector, pod.Labels) {
				svcID := b.svcID(svc.Namespace, svc.Name)
				b.addEdge(selectsEdge(svcID, podID, noTraffic[svcID]))
			}
		}
	}

	var classList networkingv1.IngressClassList
	if err := list("IngressClass", &classList); err != nil {
		return nil, err
	}
	sortObjs(classList.Items)
	// Cluster-scoped IngressClass nodes (D-038): rendered in the "cluster"
	// pseudo-cell; config edges class → ingress.
	addIngressClasses(b, classList.Items)

	var ingList networkingv1.IngressList
	if err := list("Ingress", &ingList); err != nil {
		return nil, err
	}
	sortObjs(ingList.Items)
	for i := range ingList.Items {
		ing := &ingList.Items[i]
		if isSystemNamespace(ing.Namespace) {
			continue
		}
		ingID := graphid.Ingress(ing.Namespace, ing.Name)
		node := checker.TopologyNode{
			ID:        ingID,
			Kind:      "ingress",
			Label:     ing.Name,
			Namespace: ing.Namespace,
			Detail:    ingressHostDetail(ing),
			Meta:      map[string]string{"host": ingressHostDetail(ing)},
		}
		linkIngressClass(b, &node, ing, classList.Items)
		b.addNode(node)
		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for _, p := range rule.HTTP.Paths {
				if p.Backend.Service == nil || p.Backend.Service.Name == "" {
					continue
				}
				svcName := p.Backend.Service.Name
				if b.has(b.svcID(ing.Namespace, svcName)) {
					b.addEdge(checker.TopologyEdge{
						From: ingID, To: b.svcID(ing.Namespace, svcName),
						Label: "routes → " + backendPortLabel(p.Backend.Service.Port),
					})
				}
			}
		}
	}

	// Istio routing objects (D-037) — from the integration snapshot, never
	// listed here (D-036).
	if opts.Istio != nil {
		svcByKey := make(map[string]*corev1.Service, len(svcList.Items))
		for i := range svcList.Items {
			svcByKey[svcList.Items[i].Namespace+"/"+svcList.Items[i].Name] = &svcList.Items[i]
		}
		addIstio(b, opts.Istio, istioInputs{
			services:   svcByKey,
			nsExists:   func(ns string) bool { return nsExists[ns] },
			workloads:  workloads,
			standalone: standalone,
		})
	}

	// Anchor chain roots to their namespace (D-032).
	b.addNamespaceEdges()

	return b.finish(), nil
}

// selectsEdge builds a service → backend edge; ok=false when the service has
// no ready endpoints (otherwise ok stays unset — no visual change).
func selectsEdge(from, to string, noTraffic bool) checker.TopologyEdge {
	e := checker.TopologyEdge{From: from, To: to, Label: "selects"}
	if noTraffic {
		f := false
		e.OK = &f
	}
	return e
}

// serviceSelectsWorkload reports whether a service selects a workload's
// pods. A Service selects pods by label, so the selector is matched against
// the pod-template labels (which may carry more keys than the workload's
// matchLabels) and against the labels of the workload's live pods
// (controllers may add labels at runtime); workloads without a selector
// never match.
func serviceSelectsWorkload(svc *corev1.Service, w *workload) bool {
	if w.namespace != svc.Namespace {
		return false
	}
	return w.selectedBy(svc.Spec.Selector)
}

// selectedBy reports whether a label selector selects the workload's pods
// (pod-template labels, falling back to matchLabels, or any live pod's
// labels). Shared by Service selection and Istio Gateway selection
// (D-037 deviation 1), so both see runtime-labelled pods alike.
func (w *workload) selectedBy(selector map[string]string) bool {
	if w.selector == nil {
		return false
	}
	labels := w.podLabels
	if labels == nil {
		labels = w.selector
	}
	if selectorMatches(selector, labels) {
		return true
	}
	for _, l := range w.liveLabels {
		if selectorMatches(selector, l) {
			return true
		}
	}
	return false
}

// addLiveLabels records a live pod's label set once: replicas usually share
// identical labels, so N pods collapse into one entry (perf, qa D-037).
func (w *workload) addLiveLabels(l map[string]string) {
	if len(l) == 0 {
		return
	}
	key := selectorString(l)
	if w.liveKeys == nil {
		w.liveKeys = map[string]bool{}
	}
	if w.liveKeys[key] {
		return
	}
	w.liveKeys[key] = true
	w.liveLabels = append(w.liveLabels, l)
}

// selection is the set of backends a service selects, computed once.
type selection struct {
	workloads []*workload
	pods      []*corev1.Pod // standalone pods
}

// selectBackends resolves the workloads and standalone pods a service
// selects.
func selectBackends(svc *corev1.Service, workloads []workload, standalone []*corev1.Pod) selection {
	var sel selection
	for i := range workloads {
		if serviceSelectsWorkload(svc, &workloads[i]) {
			sel.workloads = append(sel.workloads, &workloads[i])
		}
	}
	for _, pod := range standalone {
		if pod.Namespace == svc.Namespace && selectorMatches(svc.Spec.Selector, pod.Labels) {
			sel.pods = append(sel.pods, pod)
		}
	}
	return sel
}

// allScaledToZero reports whether at least one workload is selected and
// every selected backend has desired replicas == 0 (no standalone pod
// selected either).
func (s selection) allScaledToZero() bool {
	if len(s.pods) > 0 || len(s.workloads) == 0 {
		return false
	}
	for _, w := range s.workloads {
		if w.desired != 0 {
			return false
		}
	}
	return true
}

// backendPorts collects, per selected backend (workload template or
// standalone pod), the declared containerPorts so service ports can be
// matched and unmatched ones explained (D-037).
func (s selection) backendPorts() []checker.BackendPorts {
	var out []checker.BackendPorts
	collect := func(name string, containers []corev1.Container) {
		bp := checker.BackendPorts{Name: name}
		for _, ctr := range containers {
			for _, cp := range ctr.Ports {
				bp.Ports = append(bp.Ports, checker.DeclaredPort{Name: cp.Name, Port: cp.ContainerPort})
			}
		}
		out = append(out, bp)
	}
	for _, w := range s.workloads {
		collect(w.name, w.containers)
	}
	for _, pod := range s.pods {
		collect(pod.Name, pod.Spec.Containers)
	}
	return out
}

// workload is the common shape of deployments, statefulsets and daemonsets.
type workload struct {
	id, kind, namespace, name string
	selector                  map[string]string   // pod-template matchLabels (nil: no selector)
	podLabels                 map[string]string   // pod-template labels
	liveLabels                []map[string]string // distinct label sets of the workload's live pods
	liveKeys                  map[string]bool     // dedupe keys of liveLabels
	containers                []corev1.Container
	replicas                  string // "ready/desired ready"
	desired                   int32  // desired replicas (0 = intentionally scaled to zero)
}

// collectWorkloads flattens the workload kinds into one sorted list
// (deployments, then statefulsets, then daemonsets; each by namespace/name).
func collectWorkloads(deps []appsv1.Deployment, sts []appsv1.StatefulSet, dss []appsv1.DaemonSet) []workload {
	sortObjs(deps)
	sortObjs(sts)
	sortObjs(dss)
	out := make([]workload, 0, len(deps)+len(sts)+len(dss))
	for i := range deps {
		d := &deps[i]
		if isSystemNamespace(d.Namespace) {
			continue
		}
		out = append(out, workload{
			id: depID(d.Namespace, d.Name), kind: "deployment", namespace: d.Namespace, name: d.Name,
			selector:   matchLabels(d.Spec.Selector),
			podLabels:  d.Spec.Template.Labels,
			containers: d.Spec.Template.Spec.Containers,
			replicas:   readyOf(d.Status.AvailableReplicas, d.Spec.Replicas),
			desired:    desiredOf(d.Spec.Replicas),
		})
	}
	for i := range sts {
		s := &sts[i]
		if isSystemNamespace(s.Namespace) {
			continue
		}
		out = append(out, workload{
			id: stsID(s.Namespace, s.Name), kind: "statefulset", namespace: s.Namespace, name: s.Name,
			selector:   matchLabels(s.Spec.Selector),
			podLabels:  s.Spec.Template.Labels,
			containers: s.Spec.Template.Spec.Containers,
			replicas:   readyOf(s.Status.ReadyReplicas, s.Spec.Replicas),
			desired:    desiredOf(s.Spec.Replicas),
		})
	}
	for i := range dss {
		d := &dss[i]
		if isSystemNamespace(d.Namespace) {
			continue
		}
		desired := d.Status.DesiredNumberScheduled
		out = append(out, workload{
			id: dsID(d.Namespace, d.Name), kind: "daemonset", namespace: d.Namespace, name: d.Name,
			selector:   matchLabels(d.Spec.Selector),
			podLabels:  d.Spec.Template.Labels,
			containers: d.Spec.Template.Spec.Containers,
			replicas:   readyOf(d.Status.NumberReady, &desired),
			desired:    desired,
		})
	}
	return out
}

func matchLabels(sel *metav1.LabelSelector) map[string]string {
	if sel == nil {
		return nil
	}
	if sel.MatchLabels == nil {
		return map[string]string{}
	}
	return sel.MatchLabels
}

// desiredOf returns spec.replicas, defaulting to 1 when unset (API default).
func desiredOf(replicas *int32) int32 {
	if replicas == nil {
		return 1
	}
	return *replicas
}

func readyOf(ready int32, desired *int32) string {
	d := int32(0)
	if desired != nil {
		d = *desired
	}
	return itoa(int(ready)) + "/" + itoa(int(d)) + " ready"
}

// podOwnerID resolves the workload node ID that owns a pod (graphid.PodOwner).
func podOwnerID(pod *corev1.Pod) string { return graphid.PodOwner(pod) }

// epStats counts a service's endpoints by readiness.
type epStats struct{ ready, notReady int }

// endpointStatsByService aggregates EndpointSlices (label
// kubernetes.io/service-name) into ready/notReady counts keyed "ns/service".
// Endpoints are de-duplicated across slices (dual-stack services publish one
// slice per IP family for the same pods).
func endpointStatsByService(slices []discoveryv1.EndpointSlice) map[string]*epStats {
	out := map[string]*epStats{}
	seen := map[string]bool{}
	for i := range slices {
		sl := &slices[i]
		svcName := sl.Labels[discoveryv1.LabelServiceName]
		if svcName == "" || isSystemNamespace(sl.Namespace) {
			continue
		}
		key := sl.Namespace + "/" + svcName
		st := out[key]
		if st == nil {
			st = &epStats{}
			out[key] = st
		}
		for _, ep := range sl.Endpoints {
			id := endpointIdentity(&ep)
			if id != "" {
				if seen[key+"|"+id] {
					continue
				}
				seen[key+"|"+id] = true
			}
			if endpointReady(&ep) {
				st.ready++
			} else {
				st.notReady++
			}
		}
	}
	return out
}

func endpointIdentity(ep *discoveryv1.Endpoint) string {
	if ep.TargetRef != nil && ep.TargetRef.Name != "" {
		return ep.TargetRef.Kind + "/" + ep.TargetRef.Namespace + "/" + ep.TargetRef.Name
	}
	if len(ep.Addresses) > 0 {
		return "addr/" + ep.Addresses[0]
	}
	return ""
}

// endpointReady follows the API contract: a nil Ready condition means ready.
func endpointReady(ep *discoveryv1.Endpoint) bool {
	return ep.Conditions.Ready == nil || *ep.Conditions.Ready
}

// sortObjs sorts a typed object list by namespace, then name.
func sortObjs[T any, P interface {
	*T
	metav1.Object
}](items []T) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := P(&items[i]), P(&items[j])
		if a.GetNamespace() != b.GetNamespace() {
			return a.GetNamespace() < b.GetNamespace()
		}
		return a.GetName() < b.GetName()
	})
}

// addNamespaceEdges anchors only the ROOT of each chain to its namespace:
// a resource that already has an incoming edge (e.g. a deployment selected by
// a service, or a service routed to by an ingress) is already anchored through
// that chain, so it gets no direct namespace edge (D-032).
func (b *builder) addNamespaceEdges() {
	hasIncoming := map[string]bool{}
	for _, e := range b.edges {
		// Config relations (IngressClass → Ingress) are not traffic: the
		// Ingress stays the chain root and keeps its namespace anchor.
		if e.Config {
			continue
		}
		hasIncoming[e.To] = true
	}
	for _, n := range b.nodes {
		if n.Kind == "namespace" || n.Namespace == "" || hasIncoming[n.ID] {
			continue
		}
		b.addEdge(checker.TopologyEdge{From: "ns:" + n.Namespace, To: n.ID})
	}
}

// builder keeps node/edge uniqueness during a single build.
type builder struct {
	nodes []checker.TopologyNode
	edges []checker.TopologyEdge
	seenN map[string]bool
	seenE map[string]bool
}

func newBuilder() *builder {
	return &builder{
		nodes: make([]checker.TopologyNode, 0, 256),
		edges: make([]checker.TopologyEdge, 0, 512),
		seenN: map[string]bool{},
		seenE: map[string]bool{},
	}
}

func (b *builder) addNode(n checker.TopologyNode) {
	if b.seenN[n.ID] {
		return
	}
	b.seenN[n.ID] = true
	b.nodes = append(b.nodes, n)
}

func (b *builder) addEdge(e checker.TopologyEdge) {
	id := e.From + "→" + e.To
	if b.seenE[id] {
		return
	}
	b.seenE[id] = true
	b.edges = append(b.edges, e)
}

func (b *builder) has(nodeID string) bool { return b.seenN[nodeID] }

func (b *builder) svcID(ns, name string) string { return graphid.Service(ns, name) }
func (b *builder) podID(ns, name string) string { return graphid.Pod(ns, name) }

func depID(ns, name string) string { return graphid.Deployment(ns, name) }
func stsID(ns, name string) string { return graphid.StatefulSet(ns, name) }
func dsID(ns, name string) string  { return graphid.DaemonSet(ns, name) }

func (b *builder) finish() *Graph { return &Graph{Nodes: b.nodes, Edges: b.edges} }

// isSystemNamespace hides kube-system and friends from the map (noise).
func isSystemNamespace(ns string) bool {
	switch ns {
	case "kube-system", "kube-public", "kube-node-lease", "local-path-storage",
		"watchtower":
		return true
	}
	return false
}

// selectorMatches reports whether every selector key/value is present in labels.
func selectorMatches(selector, labels_ map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels_[k] != v {
			return false
		}
	}
	return true
}

func selectorString(matchLabels map[string]string) string {
	if len(matchLabels) == 0 {
		return ""
	}
	var parts []string
	for k, v := range matchLabels {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// servicePorts builds the structured port list for a service, marking each
// targetPort as matched when a selected backend declares it. Unmatched
// ports carry an explained Reason/Hint, and each issue is also returned as
// a node warning (D-037). backends is nil when the selector selects nothing.
func servicePorts(svc *corev1.Service, backends []checker.BackendPorts) ([]checker.PortInfo, []checker.NodeWarning) {
	out := make([]checker.PortInfo, 0, len(svc.Spec.Ports))
	var warnings []checker.NodeWarning
	noSelector := len(svc.Spec.Selector) == 0
	noBackend := !noSelector && len(backends) == 0
	if noBackend && len(svc.Spec.Ports) > 0 {
		warnings = append(warnings, checker.NoBackendWarning(selectorString(svc.Spec.Selector), svc.Namespace))
	}
	for _, p := range svc.Spec.Ports {
		tp := p.TargetPort
		tpLabel := tp.String()
		if tp.Type == 0 && tp.IntValue() == 0 {
			tpLabel = itoa(int(p.Port))
		}
		named := tp.Type == 1
		num := int32(tp.IntValue())
		if !named && num == 0 {
			num = p.Port
		}
		matched := false
		for _, b := range backends {
			for _, cp := range b.Ports {
				if (named && cp.Name == tp.StrVal) || (!named && cp.Port == num) {
					matched = true
				}
			}
		}
		pi := checker.PortInfo{
			Port:       p.Port,
			TargetPort: tpLabel,
			NodePort:   p.NodePort,
			Protocol:   string(p.Protocol),
			Matched:    matched,
		}
		if !matched {
			var w checker.NodeWarning
			switch {
			case noSelector:
				// Informational only: not a node warning.
				w = checker.NoSelectorWarning()
			case noBackend:
				w = checker.NoBackendWarning(selectorString(svc.Spec.Selector), svc.Namespace)
			default:
				w = checker.TargetPortWarning(named, tp.StrVal, num, backends)
				warnings = append(warnings, w)
			}
			pi.Reason = w.Reason
			pi.Hint = checker.PortHint(w)
		}
		out = append(out, pi)
	}
	return out, warnings
}

// workloadPorts builds the containerPort list for a workload/pod.
func workloadPorts(containers []corev1.Container) []checker.PortInfo {
	out := make([]checker.PortInfo, 0)
	for _, ctr := range containers {
		for _, cp := range ctr.Ports {
			out = append(out, checker.PortInfo{
				Port:     cp.ContainerPort,
				Protocol: string(cp.Protocol),
			})
		}
	}
	return out
}

// externalAddresses renders a service's external entry points: load
// balancer ingress IPs/hostnames plus spec.externalIPs (deduplicated). A
// LoadBalancer without any address yet reports "pending".
func externalAddresses(svc *corev1.Service) string {
	seen := map[string]bool{}
	var out []string
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		add(ing.IP)
		add(ing.Hostname)
	}
	for _, ip := range svc.Spec.ExternalIPs {
		add(ip)
	}
	if len(out) == 0 {
		if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
			return "pending"
		}
		return ""
	}
	return strings.Join(out, ", ")
}

// backendPortLabel renders an Ingress backend port as its number or name.
func backendPortLabel(p networkingv1.ServiceBackendPort) string {
	if p.Number != 0 {
		return itoa(int(p.Number))
	}
	if p.Name != "" {
		return p.Name
	}
	return "?"
}

func ingressHostDetail(ing *networkingv1.Ingress) string {
	if len(ing.Spec.Rules) == 0 {
		return "no rules"
	}
	return ing.Spec.Rules[0].Host
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [12]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
