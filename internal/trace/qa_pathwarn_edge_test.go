package trace

import (
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/krbck/watchtower/internal/integrations/istio"
)

// QA D-040: path-miss edge cases beyond pathwarn_test.go — mixed rules
// (port/authority/gateway/headers), tls-only VS, wildcard hosts, multiple
// VS per host, Ingress Exact vs Prefix, defaultBackend, wildcard Ingress.

func mobileRoute() []istio.Destination { return []istio.Destination{{Host: "mobile-api", Port: 80}} }

func TestQAPathWarnMixedIstioRules(t *testing.T) {
	// Rule A applies to another port, rule B applies to this port with
	// another path: the path misses on the applicable rule → Warning, and
	// the hint lists only what applies to this request (not port 8080's).
	f := homelab()
	f.snap.VirtualServices[1].HTTP = []istio.HTTPRule{
		{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "prefix", Value: "/"}, Port: 8080}}, Route: mobileRoute()},
		{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "prefix", Value: "/api"}}}, Route: mobileRoute()},
	}
	r := f.trace(t, "api.example.com", "/")
	last := lastStep(r.Branches[0])
	if r.State != StateWarning || last.Reason != ReasonPathNotRouted {
		t.Fatalf("mixed port: %s %+v", r.State, last)
	}
	if !strings.Contains(last.Hint, "prefix /api") || strings.Contains(last.Hint, "prefix /,") || strings.Contains(last.Hint, "serve: prefix /.") {
		t.Errorf("hint lists non-applicable rule: %q", last.Hint)
	}

	// Authority-only mismatch (path would match): no route for this host → Failed.
	f = homelab()
	f.snap.VirtualServices[1].HTTP = []istio.HTTPRule{
		{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "prefix", Value: "/"}, Authority: istio.StringMatch{Kind: "exact", Value: "other.example.com"}}}, Route: mobileRoute()},
	}
	if r := f.trace(t, "api.example.com", "/"); r.State != StateFailed || lastStep(r.Branches[0]).Reason != ReasonNoRouteMatch {
		t.Fatalf("authority-only: %s %+v", r.State, lastStep(r.Branches[0]))
	}

	// Gateway-scoped match for another gateway only → Failed.
	f = homelab()
	f.snap.VirtualServices[1].HTTP = []istio.HTTPRule{
		{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "prefix", Value: "/"}, Gateways: []string{"default/other-gw"}}}, Route: mobileRoute()},
	}
	if r := f.trace(t, "api.example.com", "/"); r.State != StateFailed || lastStep(r.Branches[0]).Reason != ReasonNoRouteMatch {
		t.Fatalf("other gateway only: %s %+v", r.State, lastStep(r.Branches[0]))
	}
}

func TestQAPathWarnConditionalRules(t *testing.T) {
	// A header-conditioned rule matching the path is followed as a
	// candidate (Partial + conditionalMatch), never turned into Warning.
	f := homelab()
	f.snap.VirtualServices[1].HTTP = []istio.HTTPRule{
		{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "prefix", Value: "/"}, Conditional: []string{"headers"}}}, Route: mobileRoute()},
	}
	r := f.trace(t, "api.example.com", "/")
	rs := reasonsOf(r.Branches[0].Steps)
	if r.State == StateWarning || rs[ReasonPathNotRouted] != "" || rs[ReasonConditionalMatch] == "" {
		t.Fatalf("conditional-on-path: state=%s reasons=%v", r.State, rs)
	}
	if r.State != StatePartial {
		t.Errorf("conditional-on-path: state=%s, want Partial", r.State)
	}

	// Method-conditioned rule on /admin only; request / → path miss →
	// Warning with the condition shown in the hint.
	f = homelab()
	f.snap.VirtualServices[1].HTTP = []istio.HTTPRule{
		{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "prefix", Value: "/admin"}, Conditional: []string{"method"}}}, Route: mobileRoute()},
	}
	r = f.trace(t, "api.example.com", "/")
	last := lastStep(r.Branches[0])
	if r.State != StateWarning || last.Reason != ReasonPathNotRouted || !strings.Contains(last.Hint, "prefix /admin (+method)") {
		t.Fatalf("conditional elsewhere: %s %+v", r.State, last)
	}

	// Pure header rule (no uri) → applies to any path → conditional, not Warning.
	f = homelab()
	f.snap.VirtualServices[1].HTTP = []istio.HTTPRule{
		{Match: []istio.HTTPMatch{{Conditional: []string{"headers"}}}, Route: mobileRoute()},
	}
	if r := f.trace(t, "api.example.com", "/zzz"); r.State == StateWarning {
		t.Fatalf("header-only rule became Warning: %+v", lastStep(r.Branches[0]))
	}
}

func TestQAPathWarnTLSOnlyVS(t *testing.T) {
	// A VS with only a tls[] section bound to an HTTP server has no http
	// route for the host → Failed noRouteMatch (not Warning).
	f := homelab()
	f.snap.VirtualServices[1].HTTP = nil
	f.snap.VirtualServices[1].TLS = []istio.TLSRule{{}}
	if r := f.trace(t, "api.example.com", "/"); r.State != StateFailed || lastStep(r.Branches[0]).Reason != ReasonNoRouteMatch {
		t.Fatalf("tls-only VS: %s %+v", r.State, lastStep(r.Branches[0]))
	}
}

func TestQAPathWarnWildcardAndMultipleVS(t *testing.T) {
	// Wildcard VS host "*.example.com" is specific (not "*") → Warning on miss.
	f := homelab()
	f.snap.Gateways[0].Servers[0].Hosts = append(f.snap.Gateways[0].Servers[0].Hosts, "*.example.com")
	f.snap.VirtualServices = append(f.snap.VirtualServices, istio.VirtualService{
		Namespace: "production", Name: "wild", Hosts: []string{"*.example.com"}, Gateways: []string{"default/nginx-gateway"},
		HTTP: []istio.HTTPRule{{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "prefix", Value: "/w"}}}, Route: mobileRoute()}},
	})
	r := f.trace(t, "zzz.example.com", "/")
	if r.State != StateWarning || lastStep(r.Branches[0]).Ref != "vs:production/wild" {
		t.Fatalf("wildcard VS: %s %+v", r.State, lastStep(r.Branches[0]))
	}
	if r := f.trace(t, "zzz.example.com", "/w/1"); r.State != StatePassed {
		t.Fatalf("wildcard VS hit: %s", r.State)
	}
	// Exact VS (api) misses "/" while the wildcard VS has only /w: the
	// exact one wins (Envoy most-specific virtual host) → Warning on api's VS.
	r = f.trace(t, "api.example.com", "/")
	last := lastStep(r.Branches[0])
	if r.State != StateWarning || last.Ref != "vs:production/mobile-api" {
		t.Fatalf("exact over wildcard: %s %+v", r.State, last)
	}

	// Weak "*" server host but a specific VS host → strong claim, Warning.
	f = homelab()
	f.snap.Gateways[0].Servers[0].Hosts = []string{"*"}
	if r := f.trace(t, "api.example.com", "/"); r.State != StateWarning {
		t.Fatalf("* server + specific VS: %s %+v", r.State, lastStep(r.Branches[0]))
	}

	// Two VS with the same exact host: hostConflict warn + first by key is followed.
	f = homelab()
	f.snap.VirtualServices = append(f.snap.VirtualServices, istio.VirtualService{
		Namespace: "production", Name: "api-dup", Hosts: []string{"api.example.com"}, Gateways: []string{"default/nginx-gateway"},
		HTTP: []istio.HTTPRule{{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "prefix", Value: "/dup"}}}, Route: mobileRoute()}},
	})
	r = f.trace(t, "api.example.com", "/")
	rs := reasonsOf(r.Branches[0].Steps)
	if rs[ReasonHostConflict] != StatusWarn || r.State != StateWarning {
		t.Fatalf("duplicate VS: state=%s reasons=%v", r.State, rs)
	}
	a := mustJSON(t, r)
	if b := mustJSON(t, f.trace(t, "api.example.com", "/")); a != b {
		t.Fatal("duplicate VS: non-deterministic")
	}
}

func TestQAIngressPathEdges(t *testing.T) {
	// Exact /login does not cover /login/x → Warning; Prefix /app covers /app/x.
	f := traefikCluster().add(ingress("web", "site", strp("traefik"), rule("site.example.com",
		path("/login", networkingv1.PathTypeExact, "site", 80),
		path("/app", networkingv1.PathTypePrefix, "site", 80),
	)))
	for _, c := range []struct{ p, want string }{
		{"/login", StatePassed}, {"/login/x", StateWarning}, {"/app/x", StatePassed}, {"/apps", StateWarning},
	} {
		if r := f.trace(t, "site.example.com", c.p); r.State != c.want {
			t.Errorf("%s: %s, want %s", c.p, r.State, c.want)
		}
	}

	// defaultBackend present → the miss is served by it: no warning.
	ing := ingress("web", "site", strp("traefik"), rule("site.example.com", path("/app", networkingv1.PathTypePrefix, "site", 80)))
	ing.Spec.DefaultBackend = &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "api", Port: networkingv1.ServiceBackendPort{Number: 80}}}
	f = traefikCluster().add(ing)
	r := f.trace(t, "site.example.com", "/other")
	if r.State != StatePassed || reasonsOf(r.Branches[0].Steps)[ReasonPathNotRouted] != "" {
		t.Fatalf("defaultBackend: %s %v", r.State, reasonsOf(r.Branches[0].Steps))
	}

	// Wildcard host rule (single label) is specific → Warning on miss.
	f = traefikCluster().add(ingress("web", "wild", strp("traefik"), rule("*.example.com", path("/app", networkingv1.PathTypePrefix, "site", 80))))
	if r := f.trace(t, "x.example.com", "/"); r.State != StateWarning {
		t.Fatalf("wildcard ingress: %s %+v", r.State, lastStep(r.Branches[0]))
	}
}

func TestQAMixedProviderTopState(t *testing.T) {
	// Istio Warning branch + Ingress Failed branch for one host → Partial
	// (doc: Failed + Warning mix is Partial); Istio Warning + Ingress Passed → Warning.
	f := homelab()
	f.add(class("traefik", "traefik.io/ingress-controller", true))
	f.add(ingress("production", "api-ing", strp("nope"), rule("api.example.com", path("/", networkingv1.PathTypePrefix, "mobile-api", 80))))
	r := f.trace(t, "api.example.com", "/")
	states := map[string]string{}
	for _, b := range r.Branches {
		states[b.Provider] = b.State
	}
	if states[ProviderIstio] != StateWarning || states[ProviderIngress] != StateFailed || r.State != StatePartial {
		t.Fatalf("mix: top=%s branches=%v", r.State, states)
	}
}

func TestQAKeepBranchesToKeepsWarning(t *testing.T) {
	// Upstream (B-001) must keep a Warning branch that passes the route
	// pointing at the Service, and report the recomputed state as Warning.
	resp := homelab().trace(t, "api.example.com", "/")
	kept := KeepBranchesTo(resp, "svc:production/mobile-api", "vs:production/mobile-api")
	if len(kept.Branches) != 1 || kept.State != StateWarning || kept.Branches[0].State != StateWarning {
		t.Fatalf("kept=%d state=%s", len(kept.Branches), kept.State)
	}
	if other := KeepBranchesTo(resp, "svc:production/portfolio", "vs:production/portfolio"); other.State != StateNotFound {
		t.Fatalf("unrelated service kept the branch: %s", other.State)
	}
}
