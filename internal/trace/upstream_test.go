package trace

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

// B-001: the routes that reach a Service, traced back from the Service.

func TestRoutesToMobileAPI(t *testing.T) {
	f := homelab()
	routes, err := RoutesTo(context.Background(), f.source(), "production", "mobile-api")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("routes = %+v", routes)
	}
	r := routes[0]
	if r.Host != "api.example.com" || r.Path != "/api" || r.Scheme != "http" || r.Port != 80 ||
		r.Provider != ProviderIstio || r.Route != "vs:production/mobile-api" || r.Entry != "gw:default/nginx-gateway" || !r.Traceable() {
		t.Fatalf("route = %+v", r)
	}

	// The trace for the route passes and its branch ends at the Service.
	resp := f.trace(t, r.Host, r.Path)
	kept := KeepBranchesTo(resp, "svc:production/mobile-api", r.Route)
	if kept.State != StatePassed || len(kept.Branches) != 1 {
		t.Fatalf("kept = %s %+v", kept.State, kept.Branches)
	}
	for _, id := range []string{"svc:istio-system/istio-ingressgateway", "dep:istio-system/istio-ingressgateway",
		"gw:default/nginx-gateway", "vs:production/mobile-api", "svc:production/mobile-api"} {
		if !contains(kept.Branches[0].Highlight.Nodes, id) {
			t.Errorf("upstream chain misses %s: %v", id, kept.Branches[0].Highlight.Nodes)
		}
	}
}

func TestRoutesToNoRoutes(t *testing.T) {
	f := homelab()
	f.app("production", "redis", map[string]string{"app": "redis"}, 6379, 6379, corev1.ServiceTypeClusterIP)
	routes, err := RoutesTo(context.Background(), f.source(), "production", "redis")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 0 {
		t.Fatalf("redis has no route, got %+v", routes)
	}
}

func TestRoutesToIngressBackend(t *testing.T) {
	f := traefikCluster().add(
		ingress("web", "site", strp("traefik"),
			rule("site.example.com",
				path("/", networkingv1.PathTypePrefix, "site", 80),
				path("/api", networkingv1.PathTypePrefix, "api", 80)),
			rule("", path("/", networkingv1.PathTypePrefix, "api", 80))),
		// Another namespace's Ingress pointing at a same-named Service
		// must not count (Ingress backends are namespace-local).
		ingress("other", "x", strp("traefik"), rule("x.example.com", path("/", networkingv1.PathTypePrefix, "api", 80))),
	)
	routes, err := RoutesTo(context.Background(), f.source(), "web", "api")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 {
		t.Fatalf("routes = %+v", routes)
	}
	// Sorted by host: the catch-all ("") first, with a note and no trace.
	if routes[0].Host != "" || routes[0].Traceable() || routes[0].Provider != ProviderIngress {
		t.Errorf("catch-all route = %+v", routes[0])
	}
	r := routes[1]
	if r.Host != "site.example.com" || r.Path != "/api" || r.Route != "ingress:web/site" || r.Entry != "ingc:traefik" || !r.Traceable() {
		t.Fatalf("ingress route = %+v", r)
	}
	resp := f.trace(t, r.Host, r.Path)
	kept := KeepBranchesTo(resp, "svc:web/api", r.Route)
	if kept.State != StatePassed || len(kept.Branches) != 1 || kept.Branches[0].Entry != "ingc:traefik" {
		t.Fatalf("kept = %s %+v", kept.State, kept.Branches)
	}
}

// A route that is broken before the backend (host not on the gateway) is
// kept through its route node; a branch reaching another Service is not.
func TestKeepBranchesToBrokenRouteAndNoMutation(t *testing.T) {
	f := homelab()
	routes, err := RoutesTo(context.Background(), f.source(), "production", "v1-website")
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes = %+v err=%v", routes, err)
	}
	resp := f.trace(t, routes[0].Host, routes[0].Path)
	nBranches, nNodes := len(resp.Branches), len(resp.Topology.Nodes)
	kept := KeepBranchesTo(resp, "svc:production/v1-website", routes[0].Route)
	if len(kept.Branches) != 1 || kept.State != StateFailed {
		t.Fatalf("broken route must be kept: %s %+v", kept.State, kept.Branches)
	}
	// Unrelated service: nothing kept, state NotFound, original untouched.
	other := KeepBranchesTo(resp, "svc:production/portfolio", "vs:production/portfolio")
	if len(other.Branches) != 0 || other.State != StateNotFound || len(other.Topology.Nodes) != 0 {
		t.Fatalf("unrelated = %s %+v", other.State, other.Branches)
	}
	if len(resp.Branches) != nBranches || len(resp.Topology.Nodes) != nNodes {
		t.Fatal("KeepBranchesTo modified the (cached) response")
	}
}

func TestServicesForTarget(t *testing.T) {
	f := homelab()
	// A second Service selecting the mobile-api pods.
	extra := f.app("production", "mobile-api-metrics", map[string]string{"app": "mobile-api"}, 9090, 9090, corev1.ServiceTypeClusterIP)
	_ = extra
	got, err := ServicesForTarget(context.Background(), f.source(), "production", "mobile-api")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 || got[0] != "mobile-api" || !contains(got, "mobile-api-metrics") {
		t.Fatalf("services = %v", got)
	}
	none, err := ServicesForTarget(context.Background(), f.source(), "production", "nothing")
	if err != nil || len(none) != 0 {
		t.Fatalf("unknown target = %v err=%v", none, err)
	}
}
