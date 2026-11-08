package trace

import (
	"fmt"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/krbck/watchtower/internal/integrations/istio"
)

// D-040: a routed host whose path matches no rule is a Warning with the
// served paths in the hint; the cases that stay Failed keep their reason.

func TestPathNotRoutedAPIRoot(t *testing.T) {
	resp := homelab().trace(t, "api.example.com", "/")
	b := resp.Branches[0]
	last := lastStep(b)
	if resp.State != StateWarning || b.State != StateWarning {
		t.Fatalf("state=%s branch=%s", resp.State, b.State)
	}
	if last.Reason != ReasonPathNotRouted || last.Status != StatusWarn || last.OK || last.Ref != "vs:production/mobile-api" || last.Hop != HopRoute {
		t.Fatalf("last step = %+v", last)
	}
	if !strings.HasPrefix(last.Detail, "Route configuration is correct; no rule matches this path (the request would get a 404 from the gateway).") {
		t.Errorf("detail = %q", last.Detail)
	}
	if !strings.Contains(last.Hint, "prefix /api") || len(last.Items) != 0 {
		t.Errorf("hint = %q items=%v", last.Hint, last.Items)
	}
	// The VS is on the path, the backend is not (the request ends at 404).
	if !contains(b.Highlight.Nodes, "vs:production/mobile-api") || contains(b.Highlight.Nodes, "svc:production/mobile-api") {
		t.Errorf("highlight = %v", b.Highlight.Nodes)
	}
	for _, e := range resp.Topology.Edges {
		if e.OK != nil && !*e.OK {
			t.Errorf("a warning must not paint edges red: %+v", e)
		}
	}
	if resp := homelab().trace(t, "api.example.com", "/api/x"); resp.State != StatePassed {
		t.Fatalf("/api/x: %s", resp.State)
	}
}

func TestPathNotRoutedKeepsFailedCases(t *testing.T) {
	f := homelab()
	// n8n: host on the gateway, no VirtualService at all.
	if r := f.trace(t, "n8n.example.com", "/"); r.State != StateFailed || lastStep(r.Branches[0]).Reason != ReasonNoRouteMatch {
		t.Fatalf("n8n: %s %+v", r.State, lastStep(r.Branches[0]))
	}
	// Host not on the gateway.
	if r := f.trace(t, "website.example.com", "/"); r.State != StateFailed || reasonsOf(r.Branches[0].Steps)[ReasonHostNotOnGateway] != StatusFail {
		t.Fatalf("website: %s", r.State)
	}
	// A VS without any http[] section has no route for the host.
	f = homelab()
	f.snap.VirtualServices[1].HTTP = nil
	if r := f.trace(t, "api.example.com", "/"); r.State != StateFailed || lastStep(r.Branches[0]).Reason != ReasonNoRouteMatch {
		t.Fatalf("no http[]: %s %+v", r.State, lastStep(r.Branches[0]))
	}
	// Rules that only apply to another port: not a path miss.
	f = homelab()
	f.snap.VirtualServices[1].HTTP[0].Match[0].Port = 8080
	if r := f.trace(t, "api.example.com", "/api/x"); r.State != StateFailed || lastStep(r.Branches[0]).Reason != ReasonNoRouteMatch {
		t.Fatalf("port-only rules: %s %+v", r.State, lastStep(r.Branches[0]))
	}
	// A "*" VS host is a weak claim (D-038a): stays noRouteMatch.
	f = homelab()
	f.snap.Gateways[0].Servers[0].Hosts = append(f.snap.Gateways[0].Servers[0].Hosts, "*")
	f.snap.VirtualServices = append(f.snap.VirtualServices, istio.VirtualService{
		Namespace: "production", Name: "catchall", Hosts: []string{"*"}, Gateways: []string{"default/nginx-gateway"},
		HTTP: []istio.HTTPRule{{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "prefix", Value: "/only"}}}, Route: []istio.Destination{{Host: "portfolio", Port: 80}}}},
	})
	if r := f.trace(t, "other.example.org", "/"); r.State != StateFailed || lastStep(r.Branches[0]).Reason != ReasonNoRouteMatch {
		t.Fatalf("weak claim: %s %+v", r.State, lastStep(r.Branches[0]))
	}
}

// Exact / prefix / regex uri matches: hits pass, misses warn and list the
// rule in declaration order.
func TestPathNotRoutedMatchKinds(t *testing.T) {
	f := homelab()
	f.snap.VirtualServices[1].HTTP = []istio.HTTPRule{
		{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "exact", Value: "/health"}}}, Route: []istio.Destination{{Host: "mobile-api", Port: 80}}},
		{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "regex", Value: "/v[0-9]+/.*"}}}, Route: []istio.Destination{{Host: "mobile-api", Port: 80}}},
		{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "prefix", Value: "/api"}, IgnoreURICase: true}}, Route: []istio.Destination{{Host: "mobile-api", Port: 80}}},
	}
	for _, c := range []struct {
		path, state string
	}{
		{"/health", StatePassed}, {"/health/x", StateWarning},
		{"/v2/users", StatePassed}, {"/vx/users", StateWarning},
		{"/API/x", StatePassed}, {"/", StateWarning},
	} {
		r := f.trace(t, "api.example.com", c.path)
		if r.State != c.state {
			t.Errorf("%s: state %s, want %s (%+v)", c.path, r.State, c.state, lastStep(r.Branches[0]))
		}
		if c.state == StateWarning {
			h := lastStep(r.Branches[0]).Hint
			want := "exact /health, regex /v[0-9]+/.*, prefix /api (case-insensitive)"
			if !strings.Contains(h, want) {
				t.Errorf("%s: hint %q, want %q", c.path, h, want)
			}
		}
	}
}

// More served paths than fit in the sentence move to Items (D-039).
func TestPathNotRoutedLongListUsesItems(t *testing.T) {
	f := homelab()
	var rules []istio.HTTPRule
	for i := 0; i < 7; i++ {
		rules = append(rules, istio.HTTPRule{
			Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "prefix", Value: fmt.Sprintf("/p%d", i)}}},
			Route: []istio.Destination{{Host: "mobile-api", Port: 80}},
		})
	}
	f.snap.VirtualServices[1].HTTP = rules
	last := lastStep(f.trace(t, "api.example.com", "/").Branches[0])
	if len(last.Items) != 7 || last.Items[0] != "prefix /p0" || strings.Contains(last.Hint, "/p0") || !strings.Contains(last.Hint, "7 paths") {
		t.Fatalf("hint=%q items=%v", last.Hint, last.Items)
	}
}

func TestIngressPathNotRouted(t *testing.T) {
	ing := ingress("web", "site", strp("traefik"), rule("site.example.com",
		path("/app", networkingv1.PathTypePrefix, "site", 80),
		path("/login", networkingv1.PathTypeExact, "site", 80),
	))
	f := traefikCluster().add(ing)
	r := f.trace(t, "site.example.com", "/other")
	last := lastStep(r.Branches[0])
	if r.State != StateWarning || last.Reason != ReasonPathNotRouted || last.Ref != "ingress:web/site" ||
		!strings.Contains(last.Hint, "exact /login, prefix /app") || !strings.Contains(last.Hint, "defaultBackend") {
		t.Fatalf("state=%s last=%+v", r.State, last)
	}
	if r := f.trace(t, "site.example.com", "/app/x"); r.State != StatePassed {
		t.Fatalf("/app/x: %s", r.State)
	}
	// A catch-all (no host) rule is weak: a path miss there stays Failed.
	f = traefikCluster().add(ingress("web", "any", strp("traefik"), rule("", path("/app", networkingv1.PathTypePrefix, "site", 80))))
	if r := f.trace(t, "random.example.org", "/x"); r.State != StateFailed || lastStep(r.Branches[0]).Reason != ReasonNoRouteMatch {
		t.Fatalf("catch-all: %s %+v", r.State, lastStep(r.Branches[0]))
	}
}

func TestOverallStateOrdering(t *testing.T) {
	br := func(states ...string) []Branch {
		out := make([]Branch, len(states))
		for i, s := range states {
			out[i].State = s
		}
		return out
	}
	for _, c := range []struct {
		in   []string
		want string
	}{
		{nil, StateNotFound},
		{[]string{StatePassed}, StatePassed},
		{[]string{StatePassed, StatePassed}, StatePassed},
		{[]string{StateWarning}, StateWarning},
		{[]string{StatePassed, StateWarning}, StateWarning},
		{[]string{StatePartial, StateWarning}, StateWarning},
		{[]string{StateFailed, StateWarning}, StatePartial},
		{[]string{StateFailed, StatePassed}, StatePartial},
		{[]string{StatePartial}, StatePartial},
		{[]string{StatePassed, StatePartial}, StatePartial},
		{[]string{StateFailed}, StateFailed},
		{[]string{StateFailed, StateFailed}, StateFailed},
	} {
		if got := overallState(br(c.in...)); got != c.want {
			t.Errorf("%v → %s, want %s", c.in, got, c.want)
		}
	}
	// Branch level: fail beats the path warning, the path warning beats
	// partial findings.
	b := newBranch(ProviderIstio, "gw:x/y")
	b.add(Step{Status: StatusUnknown, Hop: HopAddress})
	b.add(Step{Status: StatusWarn, Reason: ReasonPathNotRouted, Hop: HopRoute})
	if b.state() != StateWarning {
		t.Errorf("branch state = %s", b.state())
	}
	b.add(Step{Status: StatusFail, Reason: ReasonNoReadyEndpoints})
	if b.state() != StateFailed {
		t.Errorf("branch state = %s", b.state())
	}
}

func TestPathNotRoutedDeterministic(t *testing.T) {
	f := homelab()
	a := mustJSON(t, f.trace(t, "api.example.com", "/"))
	for i := 0; i < 5; i++ {
		if b := mustJSON(t, f.trace(t, "api.example.com", "/")); a != b {
			t.Fatalf("non-deterministic:\n%s\n%s", a, b)
		}
	}
}
