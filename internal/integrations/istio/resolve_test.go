package istio

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestResolveHost(t *testing.T) {
	nsExists := func(ns string) bool { return ns == "prod" || ns == "default" }
	cases := []struct {
		host             string
		wantNs, wantName string
		wantForm         HostForm
	}{
		{"reviews", "shop", "reviews", HostShort},
		{"Reviews.", "shop", "reviews", HostShort},
		{"reviews.prod.svc.cluster.local", "prod", "reviews", HostFQDN},
		{"reviews.prod.svc.corp.example", "prod", "reviews", HostFQDN}, // custom cluster domain
		{"reviews.prod.svc", "prod", "reviews", HostPartial},
		{"reviews.prod", "prod", "reviews", HostPartial},
		{"example.com", "", "", HostExternal}, // "com" is not a namespace
		{"api.example.com", "", "", HostExternal},
		{"*.example.com", "", "", HostExternal},
		{"", "", "", HostExternal},
	}
	for _, c := range cases {
		ns, name, form := ResolveHost(c.host, "shop", nsExists)
		if ns != c.wantNs || name != c.wantName || form != c.wantForm {
			t.Errorf("ResolveHost(%q) = (%q,%q,%d), want (%q,%q,%d)", c.host, ns, name, form, c.wantNs, c.wantName, c.wantForm)
		}
	}
}

func TestResolveGatewayRef(t *testing.T) {
	cases := []struct {
		ref, ns, name string
		ok            bool
	}{
		{"default/nginx-gateway", "default", "nginx-gateway", true},
		{"nginx-gateway", "production", "nginx-gateway", true}, // bare = VS namespace
		{"mesh", "", "", false},
		{"", "", "", false},
		{"/x", "", "", false},
		{"x/", "", "", false},
	}
	for _, c := range cases {
		ns, name, ok := ResolveGatewayRef(c.ref, "production")
		if ns != c.ns || name != c.name || ok != c.ok {
			t.Errorf("ResolveGatewayRef(%q) = (%q,%q,%v), want (%q,%q,%v)", c.ref, ns, name, ok, c.ns, c.name, c.ok)
		}
	}
}

func TestHostExposed(t *testing.T) {
	cases := []struct {
		vsHost, vsNs, server, gwNs string
		want                       bool
	}{
		{"api.example.com", "production", "api.example.com", "default", true},
		{"API.example.com", "production", "api.example.com", "default", true},
		{"website.example.net", "production", "api.example.com", "default", false},
		{"a.example.com", "p", "*.example.com", "default", true},
		{"example.com", "p", "*.example.com", "default", false}, // wildcard never matches the apex
		{"x.y.example.com", "p", "*.example.com", "default", true},
		{"anything.io", "p", "*", "default", true},
		{"*.example.com", "p", "a.example.com", "default", true}, // VS wildcard covers the server host
		{"*", "p", "a.example.com", "default", true},
		// namespace-qualified server hosts
		{"a.com", "p", "*/a.com", "default", true},
		{"a.com", "p", "p/a.com", "default", true},
		{"a.com", "q", "p/a.com", "default", false},
		{"a.com", "default", "./a.com", "default", true},
		{"a.com", "p", "./a.com", "default", false},
		{"a.com", "p", "p/*", "default", true},
	}
	for _, c := range cases {
		if got := HostExposed(c.vsHost, c.vsNs, c.server, c.gwNs); got != c.want {
			t.Errorf("HostExposed(%q,%q,%q,%q) = %v, want %v", c.vsHost, c.vsNs, c.server, c.gwNs, got, c.want)
		}
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeAuto, "auto": ModeAuto, "TRUE": ModeEnabled, "false": ModeDisabled} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q,%v want %q", in, got, err, want)
		}
	}
	if _, err := ParseMode("maybe"); err == nil {
		t.Error("ParseMode must reject unknown values")
	}
}

func TestParseVirtualServiceAndGateway(t *testing.T) {
	vs := ParseVirtualService(&unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"namespace": "production", "name": "express-website"},
		"spec": map[string]interface{}{
			"hosts":    []interface{}{"website.example.net"},
			"gateways": []interface{}{"default/nginx-gateway"},
			"http": []interface{}{
				map[string]interface{}{
					"match": []interface{}{map[string]interface{}{"uri": map[string]interface{}{"prefix": "/api"}}},
					"route": []interface{}{map[string]interface{}{"destination": map[string]interface{}{
						"host": "express-website", "port": map[string]interface{}{"number": int64(80)}}}},
				},
				map[string]interface{}{ // no match = everything
					"route": []interface{}{map[string]interface{}{"destination": map[string]interface{}{"host": "fallback"}}},
				},
			},
			"tcp": []interface{}{map[string]interface{}{
				"match": []interface{}{map[string]interface{}{"port": int64(5432)}},
				"route": []interface{}{map[string]interface{}{"destination": map[string]interface{}{"host": "pg"}}},
			}},
		},
	}})
	if vs.Key() != "production/express-website" || len(vs.Hosts) != 1 || vs.Gateways[0] != "default/nginx-gateway" {
		t.Fatalf("unexpected VS: %+v", vs)
	}
	want := []Route{
		{Protocol: "http", Match: "/api", Host: "express-website", Port: 80},
		{Protocol: "http", Match: "/", Host: "fallback"},
		{Protocol: "tcp", Match: ":5432", Host: "pg"},
	}
	if len(vs.Routes) != len(want) {
		t.Fatalf("routes = %+v", vs.Routes)
	}
	for i := range want {
		if vs.Routes[i] != want[i] {
			t.Errorf("route %d = %+v, want %+v", i, vs.Routes[i], want[i])
		}
	}

	gw := ParseGateway(&unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"namespace": "default", "name": "nginx-gateway"},
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{"istio": "ingressgateway"},
			"servers": []interface{}{map[string]interface{}{
				"hosts": []interface{}{"api.example.com", "immich.example.com"},
				"port":  map[string]interface{}{"name": "http", "number": int64(80), "protocol": "HTTP"},
			}},
		},
	}})
	if gw.Selector["istio"] != "ingressgateway" || len(gw.Servers) != 1 || gw.Servers[0].Port != 80 ||
		gw.Servers[0].Protocol != "HTTP" || len(gw.Servers[0].Hosts) != 2 {
		t.Fatalf("unexpected gateway: %+v", gw)
	}
	if !GatewayExposes(&gw, "api.example.com", "production") || GatewayExposes(&gw, "website.example.com", "production") {
		t.Error("GatewayExposes mismatch")
	}
}
