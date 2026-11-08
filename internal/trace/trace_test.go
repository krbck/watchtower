package trace

import (
	"context"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/krbck/watchtower/internal/integrations/istio"
)

func TestHomelabPortfolioPassesToPods(t *testing.T) {
	resp := homelab().trace(t, "Portfolio.example.com.", "/")
	if resp.State != StatePassed || len(resp.Branches) != 1 {
		t.Fatalf("state=%s branches=%d steps=%+v", resp.State, len(resp.Branches), resp.Branches)
	}
	b := resp.Branches[0]
	if b.Provider != ProviderIstio || b.Entry != "gw:default/nginx-gateway" {
		t.Fatalf("branch = %s %s", b.Provider, b.Entry)
	}
	if b.Steps[0].Hop != HopOutside {
		t.Fatalf("first step must be the out-of-cluster scope note: %+v", b.Steps[0])
	}
	for _, s := range b.Steps {
		if s.Status == StatusFail {
			t.Errorf("unexpected failing step: %+v", s)
		}
	}
	addr := b.Steps[2]
	if addr.Hop != HopAddress || !strings.Contains(addr.Detail, "203.0.113.150") || !strings.Contains(addr.Detail, "NodePort 32365") {
		t.Errorf("address step = %+v", addr)
	}
	want := []string{
		"trace:host:portfolio.example.com", "trace:addr:istio-system/istio-ingressgateway",
		"svc:istio-system/istio-ingressgateway", "dep:istio-system/istio-ingressgateway",
		"gw:default/nginx-gateway", "vs:production/portfolio", "svc:production/portfolio", "dep:production/portfolio",
	}
	if strings.Join(b.Highlight.Nodes, ",") != strings.Join(want, ",") {
		t.Errorf("highlight nodes:\n got  %v\n want %v", b.Highlight.Nodes, want)
	}
	// Mapper edges in traffic direction are part of the highlight.
	edges := map[string]bool{}
	for _, e := range b.Highlight.Edges {
		edges[e.From+"→"+e.To] = true
	}
	for _, e := range []string{
		"svc:istio-system/istio-ingressgateway→dep:istio-system/istio-ingressgateway",
		"dep:istio-system/istio-ingressgateway→gw:default/nginx-gateway",
		"gw:default/nginx-gateway→vs:production/portfolio",
		"vs:production/portfolio→svc:production/portfolio",
		"svc:production/portfolio→dep:production/portfolio",
	} {
		if !edges[e] {
			t.Errorf("highlight edge %s missing: %v", e, b.Highlight.Edges)
		}
	}
	if resp.GraphVersion != 7 || resp.Request.Host != "portfolio.example.com" || resp.Request.Port != 80 {
		t.Errorf("request echo = %+v v=%d", resp.Request, resp.GraphVersion)
	}
	if len(resp.Topology.Nodes) != len(want) || len(resp.Topology.Edges) != len(want)-1 {
		t.Errorf("topology = %d nodes / %d edges", len(resp.Topology.Nodes), len(resp.Topology.Edges))
	}
}

func TestHomelabHostNotOnGateway(t *testing.T) {
	for _, host := range []string{"website.example.com", "kdu-automation.example.com"} {
		resp := homelab().trace(t, host, "/")
		if resp.State != StateFailed || len(resp.Branches) != 1 {
			t.Fatalf("%s: state=%s branches=%d", host, resp.State, len(resp.Branches))
		}
		var last Step
		for _, st := range resp.Branches[0].Steps {
			if st.Reason == ReasonHostNotOnGateway {
				last = st
			}
		}
		if last.Status != StatusFail {
			t.Fatalf("%s: hostNotOnGateway step missing: %+v", host, resp.Branches[0].Steps)
		}
		// The host list lives in Items, not in the sentence.
		if len(last.Items) == 0 || !sort.StringsAreSorted(last.Items) || strings.Contains(last.Detail, last.Items[0]) {
			t.Errorf("%s: items=%v detail=%q", host, last.Items, last.Detail)
		}
		// The claiming VirtualService is part of the path (qa minor).
		vs := "vs:production/" + strings.Split(host, ".")[0]
		if host == "website.example.com" {
			vs = "vs:production/v1-website"
		}
		if !contains(resp.Branches[0].Highlight.Nodes, vs) {
			t.Errorf("%s: claiming VS %s missing from highlight %v", host, vs, resp.Branches[0].Highlight.Nodes)
		}
		if lastStep(resp.Branches[0]).Status != StatusSkipped {
			t.Errorf("%s: VS step must be skipped (not applied): %+v", host, lastStep(resp.Branches[0]))
		}
		if !strings.Contains(last.Detail, "Istio gateway default/nginx-gateway does not route this host") ||
			strings.Contains(strings.ToLower(last.Detail), "down") {
			t.Errorf("%s: wording = %q", host, last.Detail)
		}
	}
}

func TestHomelabAPIPrefixAndNoRouteMatch(t *testing.T) {
	resp := homelab().trace(t, "api.example.com", "/api/x")
	if resp.State != StatePassed {
		t.Fatalf("/api/x: %s %+v", resp.State, resp.Branches[0].Steps)
	}
	if !contains(resp.Branches[0].Highlight.Nodes, "dep:production/mobile-api") {
		t.Errorf("must reach mobile-api: %v", resp.Branches[0].Highlight.Nodes)
	}
	// D-040: the host is routed, only "/" matches no rule → Warning.
	resp = homelab().trace(t, "api.example.com", "/")
	last := lastStep(resp.Branches[0])
	if resp.State != StateWarning || resp.Branches[0].State != StateWarning ||
		last.Reason != ReasonPathNotRouted || last.Status != StatusWarn || !strings.Contains(last.Hint, "prefix /api") {
		t.Fatalf("/: state=%s last=%+v", resp.State, last)
	}
}

func TestHomelabGatewayHostWithoutVirtualService(t *testing.T) {
	resp := homelab().trace(t, "n8n.example.com", "")
	last := lastStep(resp.Branches[0])
	if last.Reason != ReasonNoRouteMatch || last.Hop != HopRoute || last.Status != StatusFail || resp.State != StateFailed {
		t.Fatalf("n8n: %s %+v", resp.State, last)
	}
}

func TestRandomHostNotClaimed(t *testing.T) {
	resp := homelab().trace(t, "nothing.example.org", "/")
	if resp.State != StateNotFound || len(resp.Branches) != 0 {
		t.Fatalf("state=%s branches=%d", resp.State, len(resp.Branches))
	}
	if r := reasonsOf(resp.Steps); r[ReasonHostNotClaimed] != StatusFail {
		t.Fatalf("request steps = %+v", resp.Steps)
	}
	if resp.Steps[0].Hop != HopOutside {
		t.Error("request-level steps start with the scope note")
	}
}

func TestIstioInactiveReported(t *testing.T) {
	f := homelab()
	f.snap = nil
	resp := f.trace(t, "portfolio.example.com", "/")
	if r := reasonsOf(resp.Steps); r[ReasonProviderInactive] != StatusSkipped || resp.State != StateNotFound {
		t.Fatalf("state=%s steps=%+v", resp.State, resp.Steps)
	}
}

// Determinism: same input → byte-identical JSON.
func TestTraceDeterministic(t *testing.T) {
	f := homelab()
	a := mustJSON(t, f.trace(t, "portfolio.example.com", "/"))
	for i := 0; i < 5; i++ {
		if b := mustJSON(t, f.trace(t, "portfolio.example.com", "/")); a != b {
			t.Fatalf("non-deterministic output:\n%s\n%s", a, b)
		}
	}
}

func TestIstioHostMatchTable(t *testing.T) {
	cases := []struct {
		pattern, host string
		ok            bool
	}{
		{"a.example.com", "a.example.com", true},
		{"A.Example.com.", "a.example.com", true},
		{"*.example.com", "a.example.com", true},
		{"*.example.com", "a.b.example.com", true}, // Istio: multi-label
		{"*.example.com", "example.com", false},    // no apex
		{"*", "anything.io", true},
		{"b.example.com", "a.example.com", false},
		{"*.other.com", "a.example.com", false},
	}
	for _, c := range cases {
		if ok, _ := istioHostMatch(c.pattern, c.host); ok != c.ok {
			t.Errorf("istioHostMatch(%q, %q) = %v, want %v", c.pattern, c.host, ok, c.ok)
		}
	}
	_, exact := istioHostMatch("a.example.com", "a.example.com")
	_, wild := istioHostMatch("*.example.com", "a.example.com")
	_, any := istioHostMatch("*", "a.example.com")
	if !(exact > wild && wild > any) {
		t.Errorf("specificity order broken: exact=%d wild=%d any=%d", exact, wild, any)
	}
}

// Namespace-qualified server hosts: "./host" only lets VirtualServices of
// the Gateway namespace bind; "production/host" only production.
func TestIstioServerHostNamespaces(t *testing.T) {
	for _, c := range []struct {
		serverHost string
		wantReason string
	}{
		{"./portfolio.example.com", ReasonVSNamespaceNotAllowed},
		{"util/portfolio.example.com", ReasonVSNamespaceNotAllowed},
		{"production/portfolio.example.com", ""},
		{"*/portfolio.example.com", ""},
	} {
		f := homelab()
		f.snap.Gateways[0].Servers[0].Hosts = []string{c.serverHost}
		resp := f.trace(t, "portfolio.example.com", "/")
		got := reasonsOf(resp.Branches[0].Steps)
		if c.wantReason != "" && got[c.wantReason] != StatusFail {
			t.Errorf("%s: want %s, got %+v", c.serverHost, c.wantReason, resp.Branches[0].Steps)
		}
		if c.wantReason == "" && resp.State != StatePassed {
			t.Errorf("%s: want Passed, got %s %+v", c.serverHost, resp.State, got)
		}
	}
}

// Exact VS host beats a wildcard VS; two VS on the same host conflict.
func TestIstioExactBeatsWildcardAndConflict(t *testing.T) {
	f := homelab()
	f.snap.Gateways[0].Servers[0].Hosts = []string{"*.example.com"}
	f.snap.VirtualServices = append(f.snap.VirtualServices, istio.VirtualService{
		Namespace: "production", Name: "wild", Hosts: []string{"*.example.com"}, Gateways: []string{"default/nginx-gateway"},
		HTTP: []istio.HTTPRule{{Route: []istio.Destination{{Host: "v1-website", Port: 80}}}},
	})
	resp := f.trace(t, "portfolio.example.com", "/")
	if !contains(resp.Branches[0].Highlight.Nodes, "vs:production/portfolio") || contains(resp.Branches[0].Highlight.Nodes, "vs:production/wild") {
		t.Fatalf("exact host must win: %v", resp.Branches[0].Highlight.Nodes)
	}
	// Any other host falls to the wildcard VS (multi-label too).
	resp = f.trace(t, "x.y.example.com", "/")
	if resp.State != StatePassed || !contains(resp.Branches[0].Highlight.Nodes, "vs:production/wild") {
		t.Fatalf("wildcard VS: %s %v", resp.State, resp.Branches[0].Highlight.Nodes)
	}
	// Duplicate exact host → hostConflict warning.
	f.snap.VirtualServices = append(f.snap.VirtualServices, istio.VirtualService{
		Namespace: "util", Name: "portfolio-dup", Hosts: []string{"portfolio.example.com"}, Gateways: []string{"default/nginx-gateway"},
		HTTP: []istio.HTTPRule{{Route: []istio.Destination{{Host: "immich-server", Port: 2283}}}},
	})
	resp = f.trace(t, "portfolio.example.com", "/")
	if reasonsOf(resp.Branches[0].Steps)[ReasonHostConflict] != StatusWarn {
		t.Fatalf("hostConflict expected: %+v", resp.Branches[0].Steps)
	}
}

// Istio http[] is first-match in declaration order (not longest prefix).
func TestIstioFirstMatchTable(t *testing.T) {
	rules := []istio.HTTPRule{
		{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "exact", Value: "/exact"}}}, Route: []istio.Destination{{Host: "portfolio", Port: 80}}},
		{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "prefix", Value: "/a"}}}, Route: []istio.Destination{{Host: "mobile-api", Port: 80}}},
		{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "prefix", Value: "/a/b"}}}, Route: []istio.Destination{{Host: "v1-website", Port: 80}}},
		{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "regex", Value: "/r[0-9]+"}}}, Route: []istio.Destination{{Host: "v1-website", Port: 80}}},
	}
	vs := &istio.VirtualService{Namespace: "production", Name: "t", HTTP: rules}
	gw := &istio.Gateway{Namespace: "default", Name: "g"}
	srv := &istio.Server{Port: 80}
	for _, c := range []struct {
		path string
		idx  int
	}{
		{"/exact", 0}, {"/exact/x", -1}, {"/a/b/c", 1}, {"/abc", 1}, {"/r12", 3}, {"/r12x", -1}, {"/", -1},
	} {
		_, _, idx := selectHTTPRule(vs, gw, srv, Request{Host: "h", Path: c.path, Port: 80, Scheme: "http"})
		if idx != c.idx {
			t.Errorf("path %s → rule %d, want %d", c.path, idx, c.idx)
		}
	}
}

// Header/method conditions cannot be predicted: listed as conditional,
// the first unconditional match is followed, branch is Partial.
func TestIstioConditionalMatch(t *testing.T) {
	f := homelab()
	vs := &f.snap.VirtualServices[2] // portfolio
	vs.HTTP = append([]istio.HTTPRule{{
		Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "prefix", Value: "/"}, Conditional: []string{"headers"}}},
		Route: []istio.Destination{{Host: "mobile-api", Port: 80}},
	}}, vs.HTTP...)
	resp := f.trace(t, "portfolio.example.com", "/")
	b := resp.Branches[0]
	if reasonsOf(b.Steps)[ReasonConditionalMatch] != StatusWarn || b.State != StatePartial || resp.State != StatePartial {
		t.Fatalf("state=%s steps=%+v", b.State, b.Steps)
	}
	if !contains(b.Highlight.Nodes, "svc:production/portfolio") || contains(b.Highlight.Nodes, "svc:production/mobile-api") {
		t.Fatalf("must follow the unconditional rule: %v", b.Highlight.Nodes)
	}
}

// Weighted destinations become one sub-path (group) per backend.
func TestIstioWeightedDestinations(t *testing.T) {
	f := homelab()
	f.snap.VirtualServices[2].HTTP = []istio.HTTPRule{{Route: []istio.Destination{
		{Host: "portfolio", Port: 80, Weight: 80}, {Host: "v1-website", Port: 80, Weight: 20, Subset: "canary"},
	}}}
	resp := f.trace(t, "portfolio.example.com", "/")
	b := resp.Branches[0]
	groups := map[string]bool{}
	for _, s := range b.Steps {
		if s.Group != "" {
			groups[s.Group] = true
		}
	}
	if len(groups) != 2 || reasonsOf(b.Steps)[ReasonDestinationRuleSubset] != StatusSkipped {
		t.Fatalf("groups=%v steps=%+v", groups, b.Steps)
	}
	edges := map[string]bool{}
	for _, e := range b.Highlight.Edges {
		edges[e.From+"→"+e.To] = true
	}
	if !edges["vs:production/portfolio→svc:production/portfolio"] || !edges["vs:production/portfolio→svc:production/v1-website"] {
		t.Fatalf("both sub-paths fork at the VS: %v", b.Highlight.Edges)
	}
}

func TestGatewayPortNotExposedAndSelectorNoPods(t *testing.T) {
	f := homelab()
	f.snap.Gateways[0].Servers = append(f.snap.Gateways[0].Servers, istio.Server{Port: 8000, Protocol: "HTTP", Hosts: []string{"portfolio.example.com"}})
	req, _ := ParseRequest("portfolio.example.com", "/", "8000", "http")
	resp, err := Trace(context.Background(), f.source(), req, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if reasonsOf(resp.Branches[0].Steps)[ReasonGatewayPortNotExposed] != StatusFail {
		t.Fatalf("port 8000 not on the LB service: %+v", resp.Branches[0].Steps)
	}

	f = homelab()
	f.snap.Gateways[0].Selector = map[string]string{"istio": "nope"}
	resp = f.trace(t, "portfolio.example.com", "/")
	r := reasonsOf(resp.Branches[0].Steps)
	if r[ReasonGatewaySelectorNoPods] != StatusFail || r[ReasonNoExternalAddress] != StatusFail {
		t.Fatalf("selector without pods: %+v", resp.Branches[0].Steps)
	}
}

// --- Ingress provider ---------------------------------------------------------

func pt(p networkingv1.PathType) *networkingv1.PathType { return &p }

func ingress(ns, name string, class *string, rules ...networkingv1.IngressRule) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       networkingv1.IngressSpec{IngressClassName: class, Rules: rules},
	}
}

func rule(host string, paths ...networkingv1.HTTPIngressPath) networkingv1.IngressRule {
	return networkingv1.IngressRule{Host: host, IngressRuleValue: networkingv1.IngressRuleValue{
		HTTP: &networkingv1.HTTPIngressRuleValue{Paths: paths}}}
}

func path(p string, t networkingv1.PathType, svc string, port int32) networkingv1.HTTPIngressPath {
	return networkingv1.HTTPIngressPath{Path: p, PathType: pt(t), Backend: networkingv1.IngressBackend{
		Service: &networkingv1.IngressServiceBackend{Name: svc, Port: networkingv1.ServiceBackendPort{Number: port}}}}
}

func class(name, controller string, def bool) *networkingv1.IngressClass {
	c := &networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: networkingv1.IngressClassSpec{Controller: controller}}
	if def {
		c.Annotations = map[string]string{"ingressclass.kubernetes.io/is-default-class": "true"}
	}
	return c
}

func strp(s string) *string { return &s }

// traefikCluster: k3s-style traefik in kube-system behind ServiceLB.
func traefikCluster() *fixture {
	f := newFixture().ns("kube-system", "web")
	lb := f.app("kube-system", "traefik", map[string]string{"app.kubernetes.io/name": "traefik"}, 80, 8000, corev1.ServiceTypeLoadBalancer)
	lb.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.150"}, {IP: "203.0.113.151"}}
	f.app("web", "site", map[string]string{"app": "site"}, 80, 8080, corev1.ServiceTypeClusterIP)
	f.app("web", "api", map[string]string{"app": "api"}, 80, 3000, corev1.ServiceTypeClusterIP)
	f.add(class("traefik", "traefik.io/ingress-controller", true))
	return f
}

func TestIngressTraefikPassesAndPathPrecedence(t *testing.T) {
	f := traefikCluster().add(ingress("web", "site", strp("traefik"), rule("site.example.com",
		path("/", networkingv1.PathTypePrefix, "site", 80),
		path("/api", networkingv1.PathTypePrefix, "api", 80),
		path("/api/exact", networkingv1.PathTypeExact, "site", 80),
	)))
	cases := []struct {
		path, wantSvc string
	}{
		{"/", "svc:web/site"},
		{"/apix", "svc:web/site"}, // segment-wise prefix: /api does not match /apix
		{"/api", "svc:web/api"},
		{"/api/v1", "svc:web/api"},
		{"/api/exact", "svc:web/site"}, // Exact beats the longer Prefix
	}
	for _, c := range cases {
		resp := f.trace(t, "site.example.com", c.path)
		if resp.State != StatePassed {
			t.Fatalf("%s: %s %+v", c.path, resp.State, resp.Branches[0].Steps)
		}
		b := resp.Branches[0]
		if b.Entry != "ingc:traefik" || !contains(b.Highlight.Nodes, c.wantSvc) {
			t.Errorf("%s: entry=%s nodes=%v want %s", c.path, b.Entry, b.Highlight.Nodes, c.wantSvc)
		}
		if !contains(b.Highlight.Nodes, "dep:kube-system/traefik") {
			t.Errorf("%s: controller in kube-system must be resolved (no system ns filter): %v", c.path, b.Highlight.Nodes)
		}
	}
}

func TestIngressNoRouteMatchAndDefaultBackend(t *testing.T) {
	ing := ingress("web", "site", strp("traefik"), rule("site.example.com", path("/app", networkingv1.PathTypePrefix, "site", 80)))
	f := traefikCluster().add(ing)
	resp := f.trace(t, "site.example.com", "/other")
	if last := lastStep(resp.Branches[0]); last.Reason != ReasonPathNotRouted || resp.State != StateWarning || !strings.Contains(last.Hint, "prefix /app") {
		t.Fatalf("pathNotRouted expected (D-040): %s %+v", resp.State, resp.Branches[0].Steps)
	}
	ing.Spec.DefaultBackend = &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "api", Port: networkingv1.ServiceBackendPort{Number: 80}}}
	resp = f.trace(t, "site.example.com", "/other")
	if resp.State != StatePassed || !contains(resp.Branches[0].Highlight.Nodes, "svc:web/api") {
		t.Fatalf("defaultBackend: %s %v", resp.State, resp.Branches[0].Highlight.Nodes)
	}
}

func TestIngressImplementationSpecificWarns(t *testing.T) {
	f := traefikCluster().add(ingress("web", "site", strp("traefik"), rule("site.example.com", path("/", networkingv1.PathTypeImplementationSpecific, "site", 80))))
	resp := f.trace(t, "site.example.com", "/x")
	var route Step
	for _, s := range resp.Branches[0].Steps {
		if s.Hop == HopRoute {
			route = s
		}
	}
	if route.Status != StatusWarn || resp.State != StatePassed {
		t.Fatalf("ImplementationSpecific must warn but pass: %s %+v", resp.State, route)
	}
}

// Ingress wildcard hosts cover exactly one label.
func TestIngressWildcardSingleLabel(t *testing.T) {
	f := traefikCluster().add(ingress("web", "wild", strp("traefik"), rule("*.example.com", path("/", networkingv1.PathTypePrefix, "site", 80))))
	if resp := f.trace(t, "a.example.com", "/"); resp.State != StatePassed {
		t.Fatalf("a.example.com: %s", resp.State)
	}
	for _, h := range []string{"a.b.example.com", "example.com"} {
		if resp := f.trace(t, h, "/"); resp.State != StateNotFound {
			t.Errorf("%s must not match *.example.com: %s", h, resp.State)
		}
	}
}

func TestIngressClassNotFoundAndControllerUnknown(t *testing.T) {
	// Mirrors nginx/nginx: class traefik does not exist, stale status IPs
	// that no LB Service fully carries.
	f := homelab()
	ing := ingress("default", "nginx", strp("traefik"), rule("nginx.local", path("/", networkingv1.PathTypeImplementationSpecific, "portfolio", 80)))
	ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "203.0.113.150"}, {IP: "203.0.113.151"}, {IP: "203.0.113.152"}}
	f.add(ing)
	resp := f.trace(t, "nginx.local", "/")
	r := reasonsOf(resp.Branches[0].Steps)
	if r[ReasonClassNotFound] != StatusFail || r[ReasonControllerUnknown] != StatusFail || resp.State != StateFailed {
		t.Fatalf("state=%s steps=%+v", resp.State, resp.Branches[0].Steps)
	}
	if resp.Branches[0].Entry != "ingc:traefik" {
		t.Errorf("entry = %s", resp.Branches[0].Entry)
	}
}

// k3s ServiceLB: every LB Service carries the node IPs, so the status
// address alone is ambiguous; IP+port decides.
func TestIngressServiceLBSharedIPs(t *testing.T) {
	build := func(otherPort int32) *fixture {
		f := newFixture().ns("kube-system", "web", "other")
		a := f.app("kube-system", "ctl", map[string]string{"app": "ctl"}, 80, 8000, corev1.ServiceTypeLoadBalancer)
		b := f.app("other", "db", map[string]string{"app": "db"}, otherPort, 5432, corev1.ServiceTypeLoadBalancer)
		ips := []corev1.LoadBalancerIngress{{IP: "10.1.0.1"}, {IP: "10.1.0.2"}}
		a.Status.LoadBalancer.Ingress, b.Status.LoadBalancer.Ingress = ips, ips
		f.app("web", "site", map[string]string{"app": "site"}, 80, 8080, corev1.ServiceTypeClusterIP)
		ing := ingress("web", "site", nil, rule("site.example.com", path("/", networkingv1.PathTypePrefix, "site", 80)))
		ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "10.1.0.1"}}
		f.add(ing)
		return f
	}
	resp := build(80).trace(t, "site.example.com", "/")
	b := resp.Branches[0]
	if reasonsOf(b.Steps)[ReasonAmbiguous] != StatusWarn {
		t.Fatalf("same IP and port on two LBs must be ambiguous: %+v", b.Steps)
	}
	var amb Step
	for _, s := range b.Steps {
		if s.Reason == ReasonAmbiguous {
			amb = s
		}
	}
	if !strings.Contains(amb.Detail, "kube-system/ctl") || !strings.Contains(amb.Detail, "other/db") {
		t.Errorf("candidate list missing: %q", amb.Detail)
	}
	resp = build(5432).trace(t, "site.example.com", "/")
	if !contains(resp.Branches[0].Highlight.Nodes, "dep:kube-system/ctl") || reasonsOf(resp.Branches[0].Steps)[ReasonAmbiguous] != "" {
		t.Fatalf("IP+port must pick the controller: %+v", resp.Branches[0].Steps)
	}
}

func TestIngressCatchAllIsWeak(t *testing.T) {
	// Mirrors default/nodejs-minimal: no host, class nginx missing.
	f := homelab().add(ingress("default", "nodejs-minimal", strp("nginx"), rule("", path("/", networkingv1.PathTypePrefix, "portfolio", 80))))
	// A host with a specific claim ignores the catch-all.
	if resp := f.trace(t, "portfolio.example.com", "/"); len(resp.Branches) != 1 || resp.Branches[0].Provider != ProviderIstio {
		t.Fatalf("catch-all must not add a branch next to a specific claim: %d", len(resp.Branches))
	}
	// An otherwise unclaimed host is answered by the catch-all.
	resp := f.trace(t, "random.example.org", "/")
	if len(resp.Branches) != 1 || resp.Branches[0].Provider != ProviderIngress {
		t.Fatalf("catch-all branch expected: %+v", resp.Branches)
	}
	r := reasonsOf(resp.Branches[0].Steps)
	if r[ReasonClassNotFound] != StatusFail || r[ReasonNoExternalAddress] != StatusFail || r[ReasonControllerUnknown] != StatusFail {
		t.Fatalf("steps = %+v", resp.Branches[0].Steps)
	}
}

func TestMultipleEntrypoints(t *testing.T) {
	f := homelab()
	f.add(class("traefik", "traefik.io/ingress-controller", false))
	f.app("kube-system", "traefik", map[string]string{"app.kubernetes.io/name": "traefik"}, 80, 8000, corev1.ServiceTypeLoadBalancer)
	f.add(ingress("production", "portfolio", strp("traefik"), rule("portfolio.example.com", path("/", networkingv1.PathTypePrefix, "portfolio", 80))))
	resp := f.trace(t, "portfolio.example.com", "/")
	if len(resp.Branches) != 2 || resp.Branches[0].Provider != ProviderIngress || resp.Branches[1].Provider != ProviderIstio {
		t.Fatalf("branches = %+v", resp.Branches)
	}
	if reasonsOf(resp.Steps)[ReasonMultipleEntrypoints] != StatusWarn {
		t.Fatalf("multipleEntrypoints expected: %+v", resp.Steps)
	}
}

func TestParseRequest(t *testing.T) {
	cases := []struct {
		host, path, port, scheme string
		want                     Request
		err                      bool
	}{
		{"Example.COM.", "", "", "", Request{"example.com", "/", 80, "http"}, false},
		{"example.com:8443", "/x?y=1", "", "https", Request{"example.com", "/x", 8443, "https"}, false},
		{"example.com", "", "443", "", Request{"example.com", "/", 443, "https"}, false},
		{"https://example.com/a/b", "", "", "", Request{"example.com", "/a/b", 443, "https"}, false},
		{"http://example.com/a", "", "", "", Request{"example.com", "/a", 80, "http"}, false},
		{"https://example.com/a", "", "", "http", Request{"example.com", "/a", 80, "http"}, false}, // explicit param wins
		{"HTTPS://example.com:8443", "", "", "", Request{"example.com", "/", 8443, "https"}, false},
		{"example.com", "/<img src=x onerror=alert(1)>", "", "", Request{}, true},
		{"api.example.com/<img src=x>", "", "", "", Request{}, true},
		{"example.com", "/a\"b", "", "", Request{}, true},
		{"example.com", "/a'b", "", "", Request{}, true},
		{"example.com", "/a`b", "", "", Request{}, true},
		{"example.com", "/a\x01b", "", "", Request{}, true},
		{"example.com", "/" + strings.Repeat("a", MaxPathLen), "", "", Request{}, true},
		{"example.com", "a", "", "https", Request{"example.com", "/a", 443, "https"}, false},
		{"", "", "", "", Request{}, true},
		{"bad host", "", "", "", Request{}, true},
		{"*.example.com", "", "", "", Request{}, true},
		{"example.com", "", "70000", "", Request{}, true},
		{"example.com", "", "", "ftp", Request{}, true},
		{"example.com:80", "", "81", "", Request{}, true},
	}
	for _, c := range cases {
		got, err := ParseRequest(c.host, c.path, c.port, c.scheme)
		if (err != nil) != c.err || (!c.err && got != c.want) {
			t.Errorf("ParseRequest(%q,%q,%q,%q) = %+v, %v; want %+v err=%v", c.host, c.path, c.port, c.scheme, got, err, c.want, c.err)
		}
	}
}

func TestHostsListing(t *testing.T) {
	f := homelab().add(ingress("web", "a", nil, rule("b.example.com"), rule(""), rule("*.wild.example.com")))
	f.snap.Gateways[0].Servers[0].Hosts = append(f.snap.Gateways[0].Servers[0].Hosts, "*.example.com", "*")
	hosts, err := Hosts(context.Background(), f.source())
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, h := range hosts {
		names = append(names, h.Host)
	}
	for _, want := range []string{"b.example.com", "n8n.example.com", "website.example.com", "portfolio.example.com"} {
		if !contains(names, want) {
			t.Errorf("%s missing from %v", want, names)
		}
	}
	for _, n := range names {
		if strings.HasPrefix(n, "*") {
			t.Errorf("wildcard host %q must be left out", n)
		}
	}
	if contains(names, "") || contains(names, "*") {
		t.Errorf("catch-all hosts must be left out: %v", names)
	}
}

// A scaled-to-zero backend stays non-failing (D-035) but is a visible
// warning in a host trace (requests reach no pod).
func TestScaledToZeroBackendWarns(t *testing.T) {
	f := homelab()
	for _, o := range f.objs {
		if d, ok := o.(*appsv1.Deployment); ok && d.Name == "portfolio" {
			zero := int32(0)
			d.Spec.Replicas = &zero
		}
	}
	var kept []client.Object
	for _, o := range f.objs {
		if p, ok := o.(*corev1.Pod); ok && p.Namespace == "production" && strings.HasPrefix(p.Name, "portfolio-") {
			continue
		}
		if sl, ok := o.(*discoveryv1.EndpointSlice); ok && sl.Name == "portfolio-sl" {
			sl.Endpoints = nil
		}
		kept = append(kept, o)
	}
	f.objs = kept
	resp := f.trace(t, "portfolio.example.com", "/")
	if resp.State != StatePassed || reasonsOf(resp.Branches[0].Steps)[ReasonNoReadyEndpoints] != StatusWarn {
		t.Fatalf("state=%s steps=%+v", resp.State, resp.Branches[0].Steps)
	}
}

// TLS passthrough (PASSTHROUGH / AUTO_PASSTHROUGH) routes via tls[] by SNI
// whatever the server protocol and needs no credentialName; a terminating
// HTTPS server routes via http[] and warns without credentialName (qa F2).
func TestIstioTLSModes(t *testing.T) {
	https := func(f *fixture, proto, mode string, cred bool) {
		f.snap.Gateways[0].Servers = append(f.snap.Gateways[0].Servers, istio.Server{
			Port: 443, Protocol: proto, TLSMode: mode, HasCredential: cred, Hosts: []string{"portfolio.example.com"},
		})
		vs := &f.snap.VirtualServices[2]
		vs.TLS = []istio.TLSRule{{SNIHosts: []string{"portfolio.example.com"}, Route: []istio.Destination{{Host: "v1-website", Port: 80}}}}
	}
	trace443 := func(f *fixture) Branch {
		req, _ := ParseRequest("https://portfolio.example.com", "/", "", "")
		resp, err := Trace(context.Background(), f.source(), req, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Branches[0]
	}
	stepAt := func(b Branch, hop string) Step {
		for _, s := range b.Steps {
			if s.Hop == hop {
				return s
			}
		}
		return Step{}
	}
	for _, c := range []struct{ proto, mode string }{{"HTTPS", "PASSTHROUGH"}, {"TLS", "PASSTHROUGH"}, {"TLS", "AUTO_PASSTHROUGH"}, {"HTTPS", "auto_passthrough"}} {
		f := homelab()
		https(f, c.proto, c.mode, false)
		b := trace443(f)
		if l := stepAt(b, HopListener); l.Status != StatusOK || strings.Contains(l.Detail, "credentialName is not set") {
			t.Errorf("%s/%s listener must not ask for a credential: %+v", c.proto, c.mode, l)
		}
		if r := stepAt(b, HopRoute); !strings.Contains(r.Name, "TLS passthrough") || r.Status != StatusOK {
			t.Errorf("%s/%s must route via tls[]: %+v", c.proto, c.mode, r)
		}
		if !contains(b.Highlight.Nodes, "svc:production/v1-website") {
			t.Errorf("%s/%s must follow the tls[] destination: %v", c.proto, c.mode, b.Highlight.Nodes)
		}
	}
	for _, c := range []struct {
		mode string
		cred bool
		want string
	}{{"SIMPLE", false, StatusWarn}, {"MUTUAL", true, StatusOK}, {"", true, StatusOK}} {
		f := homelab()
		https(f, "HTTPS", c.mode, c.cred)
		b := trace443(f)
		if l := stepAt(b, HopListener); l.Status != c.want {
			t.Errorf("HTTPS/%s cred=%v listener = %+v, want %s", c.mode, c.cred, l, c.want)
		}
		if !contains(b.Highlight.Nodes, "svc:production/portfolio") {
			t.Errorf("terminating HTTPS/%s must route via http[]: %v", c.mode, b.Highlight.Nodes)
		}
	}
}

// A ReplicaSet-owned pod whose Deployment does not exist maps to "pod:"
// exactly like the mapper, so highlight IDs match the Overview (qa minor).
func TestOrphanReplicaSetPodMapsToPod(t *testing.T) {
	f := homelab()
	var kept []client.Object
	for _, o := range f.objs {
		if d, ok := o.(*appsv1.Deployment); ok && d.Name == "mobile-api" {
			continue
		}
		kept = append(kept, o)
	}
	f.objs = kept
	resp := f.trace(t, "api.example.com", "/api/x")
	nodes := resp.Branches[0].Highlight.Nodes
	if !contains(nodes, "pod:production/mobile-api-abc-x1") || contains(nodes, "dep:production/mobile-api") {
		t.Fatalf("orphan pod must map to pod:: %v", nodes)
	}
}
