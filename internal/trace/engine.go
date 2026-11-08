package trace

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	"github.com/krbck/watchtower/internal/checker"
	"github.com/krbck/watchtower/internal/graphid"
	"github.com/krbck/watchtower/internal/integrations/istio"
)

// Options tunes a trace run.
type Options struct {
	// GraphVersion is echoed in the response (mapper graph version the
	// highlight IDs refer to).
	GraphVersion uint64
	// NodeLookup (optional) returns the mapper node for an ID so the trace
	// topology renders the same cards as the Overview.
	NodeLookup func(id string) *checker.TopologyNode
}

// world is the cluster state a trace runs against, loaded once per run and
// sorted so the output is deterministic.
type world struct {
	kube       checker.Kube
	namespaces map[string]bool
	services   []corev1.Service
	svcByKey   map[string]*corev1.Service
	pods       []corev1.Pod
	ingresses  []networkingv1.Ingress
	classes    []networkingv1.IngressClass
	// workloads holds the IDs of existing Deployments/StatefulSets/
	// DaemonSets (graphid scheme).
	workloads map[string]bool
	istio     *istio.Snapshot
}

func loadWorld(ctx context.Context, src Source) (*world, error) {
	w := &world{kube: src.Kube(), namespaces: map[string]bool{}, svcByKey: map[string]*corev1.Service{}, istio: src.IstioSnapshot()}
	nss, err := src.Namespaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	for i := range nss {
		w.namespaces[nss[i].Name] = true
	}
	if w.services, err = src.Services(ctx); err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	sort.SliceStable(w.services, func(i, j int) bool { return key(&w.services[i].ObjectMeta) < key(&w.services[j].ObjectMeta) })
	for i := range w.services {
		w.svcByKey[key(&w.services[i].ObjectMeta)] = &w.services[i]
	}
	if w.pods, err = src.Pods(ctx); err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	sort.SliceStable(w.pods, func(i, j int) bool { return key(&w.pods[i].ObjectMeta) < key(&w.pods[j].ObjectMeta) })
	if w.ingresses, err = src.Ingresses(ctx); err != nil {
		return nil, fmt.Errorf("list ingresses: %w", err)
	}
	sort.SliceStable(w.ingresses, func(i, j int) bool { return key(&w.ingresses[i].ObjectMeta) < key(&w.ingresses[j].ObjectMeta) })
	if w.classes, err = src.IngressClasses(ctx); err != nil {
		return nil, fmt.Errorf("list ingressclasses: %w", err)
	}
	sort.SliceStable(w.classes, func(i, j int) bool { return w.classes[i].Name < w.classes[j].Name })
	if w.workloads, err = src.WorkloadIDs(ctx); err != nil {
		return nil, fmt.Errorf("list workloads: %w", err)
	}
	return w, nil
}

// claim is one entry point that claims the host. Weak claims (catch-all
// rules: an Ingress rule without host, a "*" server host) only count when
// no specific claim exists — otherwise every host would show them.
type claim struct {
	provider string
	entry    string
	weak     bool
	run      func(ctx context.Context) *branchBuilder
}

// Trace runs the host trace. It never returns a nil response for a valid
// request; err is set only when the cluster state could not be read.
func Trace(ctx context.Context, src Source, req Request, opts Options) (*Response, error) {
	w, err := loadWorld(ctx, src)
	if err != nil {
		return nil, err
	}
	resp := &Response{Request: req, GraphVersion: opts.GraphVersion, Steps: []Step{scopeStep()}, Branches: []Branch{}}

	var claims []claim
	if w.istio == nil {
		resp.Steps = append(resp.Steps, Step{
			Name:   "Istio provider inactive",
			Hop:    HopClaim,
			Status: StatusSkipped,
			Reason: ReasonProviderInactive,
			Detail: "The Istio integration has no snapshot (not installed, disabled or not readable), so Istio Gateways and VirtualServices were not evaluated.",
			Hint:   "If Istio routes this host, check integrations.istio.enabled and the istio-reader ClusterRole.",
		})
	} else {
		claims = append(claims, istioClaims(w, req)...)
	}
	claims = append(claims, ingressClaims(w, req)...)

	strong := make([]claim, 0, len(claims))
	for _, c := range claims {
		if !c.weak {
			strong = append(strong, c)
		}
	}
	if len(strong) > 0 {
		claims = strong
	}
	sort.SliceStable(claims, func(i, j int) bool {
		if claims[i].provider != claims[j].provider {
			return claims[i].provider < claims[j].provider
		}
		return claims[i].entry < claims[j].entry
	})

	var builders []*branchBuilder
	for _, c := range claims {
		builders = append(builders, c.run(ctx))
	}
	for _, b := range builders {
		resp.Branches = append(resp.Branches, b.branch())
	}

	switch {
	case len(resp.Branches) == 0:
		resp.State = StateNotFound
		resp.Steps = append(resp.Steps, notClaimedStep(w, req))
	default:
		if len(resp.Branches) > 1 {
			entries := make([]string, 0, len(resp.Branches))
			for _, b := range resp.Branches {
				entries = append(entries, b.Provider+" "+b.Entry)
			}
			resp.Steps = append(resp.Steps, Step{
				Name:   fmt.Sprintf("%d entry points claim %s", len(resp.Branches), req.Host),
				Hop:    HopClaim,
				Status: StatusWarn,
				Reason: ReasonMultipleEntrypoints,
				Detail: "Claimed by: " + strings.Join(entries, "; ") + ". Which one receives traffic depends on DNS / the address clients use — each is traced as its own branch.",
				Hint:   "Keep a host on one entry point, or make sure DNS points at the one you intend.",
			})
		}
		resp.State = overallState(resp.Branches)
	}
	resp.Topology = buildTopology(builders, opts.NodeLookup)
	for i := range resp.Steps {
		resp.Steps[i].OK = resp.Steps[i].Status == StatusOK
	}
	return resp, nil
}

// scopeStep states what the trace cannot see (D-038).
func scopeStep() Step {
	return Step{
		Name:   "Outside the cluster: not visible",
		Hop:    HopOutside,
		Status: StatusUnknown,
		Detail: "DNS, CDN, tunnels (e.g. Cloudflare Tunnel), external load balancers and anything else in front of the cluster are not visible to Watchtower. The trace starts at the cluster's own entry points.",
		Hint:   "If the cluster path below is fine but the site is unreachable, check DNS records and the tunnel/CDN configuration.",
	}
}

// notClaimedStep explains a host no entry point claims, mentioning
// mesh-only VirtualServices that match it (not north-south reachable).
func notClaimedStep(w *world, req Request) Step {
	st := Step{
		Name:   "No entry point claims " + req.Host,
		Hop:    HopClaim,
		Status: StatusFail,
		Reason: ReasonHostNotClaimed,
		Detail: "No Istio Gateway / VirtualService and no Ingress rule in the cluster matches this host.",
		Hint:   "Add the host to an Ingress rule or to an Istio Gateway server plus a VirtualService bound to it. If the host is served outside this cluster, this is expected.",
	}
	if w.istio != nil {
		var meshOnly []string
		for i := range w.istio.VirtualServices {
			vs := &w.istio.VirtualServices[i]
			if len(boundGateways(vs)) > 0 {
				continue
			}
			for _, h := range vs.Hosts {
				if ok, _ := istioHostMatch(h, req.Host); ok {
					meshOnly = append(meshOnly, vs.Key())
					break
				}
			}
		}
		if len(meshOnly) > 0 {
			st.Detail += " VirtualService(s) " + strings.Join(meshOnly, ", ") + " match the host but are bound only to the mesh (no gateway), so they are not reachable from outside."
			st.Hint = "Bind the VirtualService to an ingress Gateway (spec.gateways) to expose it north-south."
		}
	}
	return st
}

// overallState folds branch states (D-040): all Passed → Passed, all
// Failed → Failed, no Failed but some Warning → Warning, otherwise Partial
// (a mix that includes a failing or uncertain branch).
func overallState(branches []Branch) string {
	passed, failed, warning := 0, 0, 0
	for _, b := range branches {
		switch b.State {
		case StatePassed:
			passed++
		case StateFailed:
			failed++
		case StateWarning:
			warning++
		}
	}
	switch {
	case len(branches) == 0:
		return StateNotFound
	case passed == len(branches):
		return StatePassed
	case failed == len(branches):
		return StateFailed
	case failed == 0 && warning > 0:
		return StateWarning
	}
	return StatePartial
}

// --- branch builder ----------------------------------------------------

// branchBuilder collects the steps of one branch and the graph path they
// walk. Each step with a Ref extends the path from the previous ref; a
// weighted destination forks the path at the route (fork/join).
type branchBuilder struct {
	provider, entry string
	steps           []Step
	last            string
	nodes           []string
	nodeSeen        map[string]bool
	nodeStatus      map[string]string
	edges           []EdgeRef
	edgeSeen        map[string]bool
	edgeBad         map[string]bool
	synth           map[string]checker.TopologyNode
}

func newBranch(provider, entry string) *branchBuilder {
	return &branchBuilder{
		provider: provider, entry: entry,
		nodeSeen: map[string]bool{}, nodeStatus: map[string]string{},
		edgeSeen: map[string]bool{}, edgeBad: map[string]bool{},
		synth: map[string]checker.TopologyNode{},
	}
}

// add appends a step and extends the path to its ref.
func (b *branchBuilder) add(s Step) {
	s.OK = s.Status == StatusOK
	b.steps = append(b.steps, s)
	if s.Ref == "" {
		return
	}
	b.nodeStatus[s.Ref] = worse(b.nodeStatus[s.Ref], s.Status)
	if b.nodeSeen[s.Ref] {
		// Revisiting a node (e.g. the Service's endpoints step after its
		// pods) annotates it but neither draws a back-edge nor moves the
		// cursor: the path stays in traffic direction. A failure there
		// still turns the edges into the node red.
		if s.Status == StatusFail {
			for _, e := range b.edges {
				if e.To == s.Ref {
					b.edgeBad[e.From+"→"+e.To] = true
				}
			}
		}
		return
	}
	b.nodeSeen[s.Ref] = true
	b.nodes = append(b.nodes, s.Ref)
	if b.last != "" {
		id := b.last + "→" + s.Ref
		if !b.edgeSeen[id] {
			b.edgeSeen[id] = true
			b.edges = append(b.edges, EdgeRef{From: b.last, To: s.Ref})
		}
		if s.Status == StatusFail {
			b.edgeBad[id] = true
		}
	}
	b.last = s.Ref
}

// at moves the path cursor (fork point for weighted destinations).
func (b *branchBuilder) at(ref string) { b.last = ref }

// define registers display data for a trace-only (or unmapped) node.
func (b *branchBuilder) define(n checker.TopologyNode) {
	if _, ok := b.synth[n.ID]; !ok {
		b.synth[n.ID] = n
	}
}

// state: any fail → Failed; a path that matches no rule (D-040
// pathNotRouted) → Warning; ambiguity/conditional/unknown → Partial; else
// Passed (other warnings allowed, e.g. a scaled-to-zero backend).
func (b *branchBuilder) state() string {
	partial, warning := false, false
	for _, s := range b.steps {
		switch {
		case s.Status == StatusFail:
			return StateFailed
		case s.Hop == HopOutside:
		case s.Reason == ReasonPathNotRouted:
			warning = true
		case s.Status == StatusUnknown,
			s.Reason == ReasonConditionalMatch, s.Reason == ReasonAmbiguous:
			partial = true
		}
	}
	switch {
	case warning:
		return StateWarning
	case partial:
		return StatePartial
	}
	return StatePassed
}

func (b *branchBuilder) branch() Branch {
	hl := Highlight{Nodes: append([]string{}, b.nodes...), Edges: append([]EdgeRef{}, b.edges...)}
	return Branch{Provider: b.provider, Entry: b.entry, State: b.state(), Steps: b.steps, Highlight: hl}
}

// worse orders statuses for node colouring: fail > warn > ok > others.
func worse(a, b string) string {
	rank := func(s string) int {
		switch s {
		case StatusFail:
			return 3
		case StatusWarn:
			return 2
		case StatusOK:
			return 1
		}
		return 0
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// buildTopology merges every branch path into one graph for the
// Connectivity view: mapper node data when available, synthesized
// otherwise; ok follows the worst step status at the node.
func buildTopology(builders []*branchBuilder, lookup func(string) *checker.TopologyNode) checker.Topology {
	topo := checker.Topology{Nodes: []checker.TopologyNode{}, Edges: []checker.TopologyEdge{}}
	seenN := map[string]int{}
	seenE := map[string]int{}
	for _, b := range builders {
		for _, id := range b.nodes {
			st := b.nodeStatus[id]
			if i, ok := seenN[id]; ok {
				setOK(&topo.Nodes[i], st)
				continue
			}
			var n checker.TopologyNode
			if lookup != nil && !strings.HasPrefix(id, graphid.TracePrefix) {
				if m := lookup(id); m != nil {
					n = *m
					n.OK = nil
				}
			}
			if n.ID == "" {
				if s, ok := b.synth[id]; ok {
					n = s
				} else {
					n = synthNode(id)
				}
			}
			setOK(&n, st)
			seenN[id] = len(topo.Nodes)
			topo.Nodes = append(topo.Nodes, n)
		}
		for _, e := range b.edges {
			id := e.From + "→" + e.To
			bad := b.edgeBad[id]
			if i, ok := seenE[id]; ok {
				if bad {
					f := false
					topo.Edges[i].OK = &f
				}
				continue
			}
			te := checker.TopologyEdge{From: e.From, To: e.To}
			if bad {
				f := false
				te.OK = &f
			}
			seenE[id] = len(topo.Edges)
			topo.Edges = append(topo.Edges, te)
		}
	}
	return topo
}

func setOK(n *checker.TopologyNode, status string) {
	switch status {
	case StatusFail:
		f := false
		n.OK = &f
	case StatusOK, StatusWarn:
		if n.OK == nil {
			t := true
			n.OK = &t
		}
	}
}

// synthNode derives display data from an ID ("svc:ns/name" → service).
func synthNode(id string) checker.TopologyNode {
	prefix, rest, _ := strings.Cut(id, ":")
	kind := map[string]string{
		"svc": "service", "dep": "deployment", "sts": "statefulset", "ds": "daemonset",
		"pod": "pod", "gw": "gateway", "vs": "virtualservice", "ingress": "ingress", "ingc": "ingressclass",
	}[prefix]
	if kind == "" {
		kind = "host"
	}
	n := checker.TopologyNode{ID: id, Kind: kind, Label: rest}
	if ns, name, ok := strings.Cut(rest, "/"); ok {
		n.Namespace, n.Label = ns, name
	}
	return n
}

func key(m interface {
	GetNamespace() string
	GetName() string
}) string {
	return m.GetNamespace() + "/" + m.GetName()
}
