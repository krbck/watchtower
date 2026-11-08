package trace

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/krbck/watchtower/internal/checker"
	"github.com/krbck/watchtower/internal/graphid"
	"github.com/krbck/watchtower/internal/integrations/istio"
)

// Upstream (B-001): which north-south routes send traffic to a Service?
// RoutesTo lists every route claim (Istio VirtualService bound to a
// gateway, Ingress rule) whose destination is the Service, as traceable
// host/path queries; the API runs the host trace for each and keeps only the
// branches that end at the Service (KeepBranchesTo).

// UpstreamRoute is one route claim pointing at a Service.
type UpstreamRoute struct {
	Host     string `json:"host"`
	Path     string `json:"path"`
	Scheme   string `json:"scheme"`
	Port     int32  `json:"port"`
	Provider string `json:"provider"`
	// Route is the graph ID of the claiming object (vs:ns/name,
	// ingress:ns/name).
	Route string `json:"route"`
	// Entry is the expected entry point (gw:ns/name for Istio, ingc:<class>
	// for Ingress when known); the trace branch entry wins when present.
	Entry string `json:"entry,omitempty"`
	// Note is set when the claim cannot be traced as a host query
	// (wildcard host, catch-all rule, tcp route); such routes carry no trace.
	Note string `json:"note,omitempty"`
}

// Traceable reports whether the route can be run through Trace.
func (r UpstreamRoute) Traceable() bool { return r.Note == "" }

// ServicesForTarget resolves the Services fronting a target workload: a
// Service named like the target first, then every Service whose selector
// matches pods of the target (checker discovery rules, D-018).
func ServicesForTarget(ctx context.Context, src Source, namespace, target string) ([]string, error) {
	svcs, err := src.Services(ctx)
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	pods, err := src.Pods(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	var nsPods []*corev1.Pod
	for i := range pods {
		if pods[i].Namespace == namespace && checker.PodMatchesTarget(&pods[i], target) {
			nsPods = append(nsPods, &pods[i])
		}
	}
	seen := map[string]bool{}
	var out []string
	sort.SliceStable(svcs, func(i, j int) bool { return svcs[i].Name < svcs[j].Name })
	for i := range svcs {
		if svcs[i].Namespace == namespace && svcs[i].Name == target {
			seen[target] = true
			out = append(out, target)
		}
	}
	for i := range svcs {
		svc := &svcs[i]
		if svc.Namespace != namespace || seen[svc.Name] || len(svc.Spec.Selector) == 0 {
			continue
		}
		sel := labels.SelectorFromSet(svc.Spec.Selector)
		for _, p := range nsPods {
			if sel.Matches(labels.Set(p.Labels)) {
				seen[svc.Name] = true
				out = append(out, svc.Name)
				break
			}
		}
	}
	return out, nil
}

// RoutesTo lists the route claims whose destination is Service ns/name,
// sorted and de-duplicated.
func RoutesTo(ctx context.Context, src Source, namespace, name string) ([]UpstreamRoute, error) {
	nss, err := src.Namespaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	nsExists := map[string]bool{}
	for i := range nss {
		nsExists[nss[i].Name] = true
	}
	var out []UpstreamRoute
	if snap := src.IstioSnapshot(); snap != nil {
		out = append(out, istioRoutesTo(snap, nsExists, namespace, name)...)
	}
	ings, err := src.Ingresses(ctx)
	if err != nil {
		return nil, fmt.Errorf("list ingresses: %w", err)
	}
	for i := range ings {
		ing := &ings[i]
		if ing.Namespace != namespace {
			continue
		}
		route := graphid.Ingress(ing.Namespace, ing.Name)
		entry := ""
		if ing.Spec.IngressClassName != nil && *ing.Spec.IngressClassName != "" {
			entry = graphid.IngressClass(*ing.Spec.IngressClassName)
		}
		for _, r := range ing.Spec.Rules {
			if r.HTTP == nil {
				continue
			}
			for _, p := range r.HTTP.Paths {
				if p.Backend.Service == nil || p.Backend.Service.Name != name {
					continue
				}
				u := UpstreamRoute{Host: normHost(r.Host), Path: normPath(p.Path), Scheme: "http", Port: 80,
					Provider: ProviderIngress, Route: route, Entry: entry}
				if u.Host == "" {
					u.Note = "Ingress rule without host (catch-all): it applies to every host the controller receives, so there is no single host to trace."
				} else if strings.HasPrefix(u.Host, "*") {
					u.Note = "Wildcard host " + u.Host + ": trace a concrete host under it in Host mode."
				}
				out = append(out, u)
			}
		}
		if be := ing.Spec.DefaultBackend; be != nil && be.Service != nil && be.Service.Name == name {
			out = append(out, UpstreamRoute{Host: "*", Path: "/", Scheme: "http", Port: 80, Provider: ProviderIngress, Route: route, Entry: entry,
				Note: "Ingress defaultBackend: takes requests no rule matches, for any host."})
		}
	}
	return dedupeRoutes(out), nil
}

// istioRoutesTo collects VirtualService rules (bound to a gateway) whose
// destination resolves to the Service. Partially qualified destinations
// ("name.ns") count too: the trace then reports why Istio does not route
// them.
func istioRoutesTo(snap *istio.Snapshot, nsExists map[string]bool, namespace, name string) []UpstreamRoute {
	gws := map[string]*istio.Gateway{}
	for i := range snap.Gateways {
		gws[snap.Gateways[i].Key()] = &snap.Gateways[i]
	}
	targets := func(vs *istio.VirtualService, dests []istio.Destination) bool {
		for _, d := range dests {
			ns, n, form := istio.ResolveHost(d.Host, vs.Namespace, func(s string) bool { return nsExists[s] })
			if form != istio.HostExternal && ns == namespace && n == name {
				return true
			}
		}
		return false
	}
	var out []UpstreamRoute
	for i := range snap.VirtualServices {
		vs := &snap.VirtualServices[i]
		bound := boundGateways(vs)
		if len(bound) == 0 {
			continue // mesh-only: not reachable north-south
		}
		route := graphid.VirtualService(vs.Namespace, vs.Name)
		emit := func(host, path, wantScheme string, wantPort uint32, gwKeys []string) {
			for _, gk := range gwKeys {
				ns, n, _ := strings.Cut(gk, "/")
				u := UpstreamRoute{Host: normHost(host), Path: path, Provider: ProviderIstio, Route: route, Entry: graphid.IstioGateway(ns, n)}
				u.Scheme, u.Port = serverFor(gws[gk], host, vs.Namespace, wantScheme, wantPort)
				switch {
				case u.Host == "" || u.Host == "*":
					u.Note = "VirtualService host \"*\" matches every host: trace a concrete host in Host mode."
				case strings.HasPrefix(u.Host, "*"):
					u.Note = "Wildcard host " + u.Host + ": trace a concrete host under it in Host mode."
				}
				out = append(out, u)
			}
		}
		for _, rule := range vs.HTTP {
			if rule.Redirect || rule.DirectResponse || !targets(vs, rule.Route) {
				continue
			}
			for _, pm := range httpRulePaths(rule, vs, bound) {
				for _, h := range vs.Hosts {
					emit(h, pm.path, "http", pm.port, pm.gateways)
				}
			}
		}
		for _, rule := range vs.TLS {
			if !targets(vs, rule.Route) {
				continue
			}
			hosts := rule.SNIHosts
			if len(hosts) == 0 {
				hosts = vs.Hosts
			}
			for _, h := range hosts {
				emit(h, "/", "https", rule.Port, bound)
			}
		}
		for _, r := range vs.Routes {
			if r.Protocol != "tcp" || !targets(vs, []istio.Destination{{Host: r.Host}}) {
				continue
			}
			for _, h := range vs.Hosts {
				for _, gk := range bound {
					ns, n, _ := strings.Cut(gk, "/")
					out = append(out, UpstreamRoute{Host: normHost(h), Path: "/", Scheme: "tcp", Provider: ProviderIstio, Route: route,
						Entry: graphid.IstioGateway(ns, n), Note: "tcp[] route (" + r.Match + "): host traces cover http and TLS (SNI) only."})
				}
			}
		}
	}
	return out
}

type rulePath struct {
	path     string
	port     uint32
	gateways []string
}

// httpRulePaths derives one traceable path per match entry: uri prefix or
// exact value; no uri match (or no match block) → "/". A regex uri cannot be
// turned into a concrete path; "/" is traced and the trace shows whether the
// rule takes it. Match-level gateways narrow the bound gateways.
func httpRulePaths(rule istio.HTTPRule, vs *istio.VirtualService, bound []string) []rulePath {
	if len(rule.Match) == 0 {
		return []rulePath{{path: "/", gateways: bound}}
	}
	var out []rulePath
	for _, m := range rule.Match {
		p := "/"
		switch m.URI.Kind {
		case "prefix", "exact":
			p = normPath(m.URI.Value)
		}
		gwKeys := bound
		if len(m.Gateways) > 0 {
			gwKeys = nil
			for _, ref := range m.Gateways {
				if ns, n, ok := istio.ResolveGatewayRef(ref, vs.Namespace); ok {
					gwKeys = append(gwKeys, ns+"/"+n)
				}
			}
		}
		out = append(out, rulePath{path: p, port: m.Port, gateways: gwKeys})
	}
	return out
}

// serverFor picks the scheme/port to trace a VirtualService host on a
// gateway: the server that exposes the host (wanted port first, then a
// plain HTTP server, then any). Without one (host not on the gateway,
// gateway missing) the default http:80 is traced and the trace explains.
func serverFor(gw *istio.Gateway, host, vsNs, wantScheme string, wantPort uint32) (string, int32) {
	scheme, port := wantScheme, int32(80)
	if wantScheme == "https" {
		port = 443
	}
	if wantPort != 0 {
		port = int32(wantPort)
	}
	if gw == nil {
		return scheme, port
	}
	var best *istio.Server
	score := -1
	for i := range gw.Servers {
		s := &gw.Servers[i]
		exposed := false
		for _, sh := range s.Hosts {
			if istio.HostExposed(host, vsNs, sh, gw.Namespace) {
				exposed = true
			}
		}
		if !exposed {
			continue
		}
		proto := strings.ToUpper(s.Protocol)
		tls := proto == "HTTPS" || proto == "TLS"
		sc := 0
		if wantPort != 0 && s.Port == wantPort {
			sc += 4
		}
		if tls == (wantScheme == "https") {
			sc += 2
		}
		if sc > score {
			best, score = s, sc
		}
	}
	if best == nil {
		return scheme, port
	}
	proto := strings.ToUpper(best.Protocol)
	if proto == "HTTPS" || proto == "TLS" {
		scheme = "https"
	} else {
		scheme = "http"
	}
	return scheme, int32(best.Port)
}

func normHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

func normPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func dedupeRoutes(in []UpstreamRoute) []UpstreamRoute {
	seen := map[string]bool{}
	out := make([]UpstreamRoute, 0, len(in))
	for _, r := range in {
		k := fmt.Sprintf("%s|%s|%s|%d|%s|%s|%s", r.Host, r.Path, r.Scheme, r.Port, r.Provider, r.Route, r.Entry)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Route != b.Route {
			return a.Route < b.Route
		}
		return a.Entry < b.Entry
	})
	return out
}

// KeepBranchesTo returns a copy of resp with only the branches relevant to
// Service svcID: branches whose path reaches the Service, plus failed or
// partial branches that pass through routeID (the claim pointing at the
// Service, broken before the backend — e.g. hostNotOnGateway). The topology
// is cut down to the kept paths and the state recomputed. resp itself (may
// be a shared cache entry) is never modified.
func KeepBranchesTo(resp *Response, svcID, routeID string) *Response {
	if resp == nil {
		return nil
	}
	out := *resp
	out.Branches = []Branch{}
	keepN := map[string]bool{}
	keepE := map[string]bool{}
	for _, b := range resp.Branches {
		has := func(id string) bool {
			for _, n := range b.Highlight.Nodes {
				if n == id {
					return true
				}
			}
			return false
		}
		if !has(svcID) && (routeID == "" || !has(routeID) || b.State == StatePassed) {
			continue
		}
		out.Branches = append(out.Branches, b)
		for _, n := range b.Highlight.Nodes {
			keepN[n] = true
		}
		for _, e := range b.Highlight.Edges {
			keepE[e.From+"→"+e.To] = true
		}
	}
	out.Topology = checker.Topology{Nodes: []checker.TopologyNode{}, Edges: []checker.TopologyEdge{}}
	for _, n := range resp.Topology.Nodes {
		if keepN[n.ID] {
			out.Topology.Nodes = append(out.Topology.Nodes, n)
		}
	}
	for _, e := range resp.Topology.Edges {
		if keepE[e.From+"→"+e.To] {
			out.Topology.Edges = append(out.Topology.Edges, e)
		}
	}
	if len(out.Branches) == 0 {
		out.State = StateNotFound
	} else {
		out.State = overallState(out.Branches)
	}
	return &out
}
