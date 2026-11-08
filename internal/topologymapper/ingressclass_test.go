package topologymapper

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/krbck/watchtower/internal/integrations/istio"
)

func mkIngressClass(name, controller string, isDefault bool) *networkingv1.IngressClass {
	c := &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       networkingv1.IngressClassSpec{Controller: controller},
	}
	if isDefault {
		c.Annotations = map[string]string{"ingressclass.kubernetes.io/is-default-class": "true"}
	}
	return c
}

func mkClassIngress(ns, name string, class *string, backend string) *networkingv1.Ingress {
	prefix := networkingv1.PathTypePrefix
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: networkingv1.IngressSpec{
			IngressClassName: class,
			Rules: []networkingv1.IngressRule{{
				Host: name + ".example.com",
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{
						Path: "/", PathType: &prefix,
						Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
							Name: backend, Port: networkingv1.ServiceBackendPort{Number: 80},
						}},
					}},
				}},
			}},
		},
	}
}

func strp(s string) *string { return &s }

// IngressClass nodes are cluster-scoped; class → ingress is a config edge
// that does not steal the Ingress's namespace anchor (D-032/D-038); a
// missing class is a red ghost node plus a classNotFound warning.
func TestIngressClassNodesAndConfigEdges(t *testing.T) {
	g := buildWithIstio(t, nil,
		mkNamespace("web"),
		mkIngressClass("traefik", "traefik.io/ingress-controller", true),
		mkService("web", "site", map[string]string{"app": "site"}, 80, 80),
		mkClassIngress("web", "a", strp("traefik"), "site"),
		mkClassIngress("web", "b", strp("nginx"), "site"),
		mkClassIngress("web", "c", nil, "site"), // default class
	)

	cls := g.findNode("ingc:traefik")
	if cls == nil || cls.Namespace != "" || cls.Kind != "ingressclass" || cls.Meta["default"] != "true" ||
		cls.Meta["workload"] != "app.kubernetes.io/name=traefik" {
		t.Fatalf("traefik class node = %+v", cls)
	}
	for _, ing := range []string{"ingress:web/a", "ingress:web/c"} {
		e := g.findEdge("ingc:traefik", ing)
		if e == nil || !e.Config {
			t.Fatalf("config edge ingc:traefik → %s missing: %+v", ing, e)
		}
		// D-032: the config edge is ignored by the root computation.
		if !g.hasEdge("ns:web", ing) {
			t.Errorf("%s must keep its namespace anchor", ing)
		}
	}
	ghost := g.findNode("ingc:nginx")
	if ghost == nil || ghost.OK == nil || *ghost.OK || len(ghost.Warnings) != 1 {
		t.Fatalf("ghost class node = %+v", ghost)
	}
	if e := g.findEdge("ingc:nginx", "ingress:web/b"); e == nil || !e.Config || e.OK == nil || *e.OK {
		t.Fatalf("ghost config edge = %+v", e)
	}
	if got := warningReasons(g.findNode("ingress:web/b")); len(got) != 1 || got[0] != ReasonClassNotFound {
		t.Fatalf("ingress b warnings = %v", got)
	}
	// The service routed to by the ingresses is not a root.
	if g.hasEdge("ns:web", "svc:web/site") {
		t.Error("service behind an ingress must not get a namespace edge")
	}
}

func TestIngressNoDefaultClassWarns(t *testing.T) {
	g := buildWithIstio(t, nil, mkNamespace("web"),
		mkService("web", "site", map[string]string{"app": "site"}, 80, 80),
		mkClassIngress("web", "c", nil, "site"))
	if got := warningReasons(g.findNode("ingress:web/c")); len(got) != 1 || got[0] != ReasonNoDefaultClass {
		t.Fatalf("warnings = %v", got)
	}
}

// Gateway "serves" uses the same pod-template + live-label logic as
// Service → workload (qa D-037 fix).
func TestGatewayServesByLivePodLabels(t *testing.T) {
	ds := mkDaemonSet("istio-system", "gw", 1, 1, 8080)
	pod := mkOwnedPod("istio-system", "gw-x", "gw", "DaemonSet", "gw", corev1.PodRunning)
	pod.Labels["istio"] = "ingressgateway" // runtime label, not in the template
	snap := &istio.Snapshot{Gateways: []istio.Gateway{{
		Namespace: "default", Name: "g", Selector: map[string]string{"istio": "ingressgateway"},
		Servers: []istio.Server{{Port: 80, Hosts: []string{"*"}}},
	}}}
	g := buildWithIstio(t, snap, mkNamespace("istio-system"), mkNamespace("default"), ds, pod)
	if !g.hasEdge("ds:istio-system/gw", "gw:default/g") {
		t.Fatal("gateway must be served by the workload via live pod labels")
	}
	if n := g.findNode("gw:default/g"); len(n.Warnings) != 0 {
		t.Fatalf("no gatewayNoWorkload expected: %+v", n.Warnings)
	}
}

// Replicas with identical labels collapse into one live label set.
func TestLiveLabelsDeduplicated(t *testing.T) {
	w := &workload{selector: map[string]string{"a": "b"}}
	for i := 0; i < 50; i++ {
		w.addLiveLabels(map[string]string{"a": "b", "x": "y"})
	}
	w.addLiveLabels(map[string]string{"a": "b", "x": "z"})
	w.addLiveLabels(nil)
	if len(w.liveLabels) != 2 {
		t.Fatalf("liveLabels = %d sets, want 2", len(w.liveLabels))
	}
}
