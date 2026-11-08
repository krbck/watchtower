package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/krbck/watchtower/internal/integrations/istio"
	"github.com/krbck/watchtower/internal/trace"
)

// upstreamApp returns a Deployment + ready pod + Service + EndpointSlice.
func upstreamApp(ns, name string, port, targetPort int32, typ corev1.ServiceType) []client.Object {
	lbl := map[string]string{"app": name}
	one := int32(1)
	ready := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-abc-x1", Namespace: ns, Labels: lbl,
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: name + "-abc", APIVersion: "apps/v1", UID: "u"}}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Ports: []corev1.ContainerPort{{ContainerPort: targetPort}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.ServiceSpec{Type: typ, Selector: lbl,
			Ports: []corev1.ServicePort{{Name: "http", Port: port, TargetPort: intstr.FromInt32(targetPort)}}},
	}
	return []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: appsv1.DeploymentSpec{
			Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: lbl},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: lbl}}}},
		pod, svc,
		&discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Name: name + "-sl", Namespace: ns, Labels: map[string]string{discoveryv1.LabelServiceName: name}},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready},
				TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: ns, Name: pod.Name}}},
		},
	}
}

// upstreamSource: an Istio ingress gateway, VirtualService
// production/mobile-api (api.example.com, prefix /api), a Service
// without routes and an Ingress backend in "web".
func upstreamSource(t *testing.T) trace.Source {
	t.Helper()
	sch := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, discoveryv1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(sch); err != nil {
			t.Fatal(err)
		}
	}
	var objs []client.Object
	for _, n := range []string{"istio-system", "default", "production", "web"} {
		objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}})
	}
	igw := upstreamApp("istio-system", "istio-ingressgateway", 80, 8080, corev1.ServiceTypeLoadBalancer)
	for _, o := range igw {
		switch v := o.(type) {
		case *corev1.Pod:
			v.Labels["istio"] = "ingressgateway"
		case *corev1.Service:
			v.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.150"}}
		}
	}
	objs = append(objs, igw...)
	objs = append(objs, upstreamApp("production", "mobile-api", 80, 3000, corev1.ServiceTypeClusterIP)...)
	objs = append(objs, upstreamApp("production", "lonely", 80, 80, corev1.ServiceTypeClusterIP)...)
	objs = append(objs, upstreamApp("web", "shop", 80, 8080, corev1.ServiceTypeClusterIP)...)
	prefix := networkingv1.PathTypePrefix
	objs = append(objs, &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "web"},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "shop.example.com",
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
				Path: "/cart", PathType: &prefix,
				Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "shop", Port: networkingv1.ServiceBackendPort{Number: 80}}},
			}}}}}}},
	})
	c := crfake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
	snap := &istio.Snapshot{
		Gateways: []istio.Gateway{{Namespace: "default", Name: "nginx-gateway", Selector: map[string]string{"istio": "ingressgateway"},
			Servers: []istio.Server{{Port: 80, Protocol: "HTTP", Hosts: []string{"api.example.com"}}}}},
		VirtualServices: []istio.VirtualService{{Namespace: "production", Name: "mobile-api", Hosts: []string{"api.example.com"},
			Gateways: []string{"default/nginx-gateway"},
			HTTP: []istio.HTTPRule{{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "prefix", Value: "/api"}}},
				Route: []istio.Destination{{Host: "mobile-api", Port: 80}}}}}},
	}
	return trace.CachedSource{Reader: c, Istio: func() *istio.Snapshot { return snap }}
}

func getUpstream(t *testing.T, url string) (int, UpstreamResponse) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body UpstreamResponse
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

func TestUpstreamEndpoint(t *testing.T) {
	s := &Server{Trace: upstreamSource(t)}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	base := ts.URL + "/api/v1/upstream"

	for _, q := range []string{"", "?namespace=production", "?service=mobile-api", "?namespace=Bad_NS&service=x",
		"?namespace=production&service=%3Cimg%3E", "?namespace=production&target=a%20b", "?namespace=production&service=a.b"} {
		if code, _ := getUpstream(t, base+q); code != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", q, code)
		}
	}
	if code, _ := getUpstream(t, base+"?namespace=production&service=nope"); code != http.StatusNotFound {
		t.Errorf("unknown service: status %d, want 404", code)
	}

	code, body := getUpstream(t, base+"?namespace=production&service=mobile-api")
	if code != http.StatusOK || body.Service != "mobile-api" || len(body.Hosts) != 1 || len(body.Traces) != 1 {
		t.Fatalf("mobile-api: status=%d body=%+v", code, body)
	}
	h := body.Hosts[0]
	if h.Host != "api.example.com" || h.Path != "/api" || h.Provider != trace.ProviderIstio ||
		h.Entry != "gw:default/nginx-gateway" || h.Route != "vs:production/mobile-api" || h.Trace != 0 || !h.Reaches {
		t.Fatalf("host = %+v", h)
	}
	tr := body.Traces[0]
	if tr.State != trace.StatePassed || len(tr.Branches) != 1 || tr.Request.Path != "/api" {
		t.Fatalf("trace = %s %+v", tr.State, tr.Branches)
	}

	// Target workload instead of a Service: resolved via discovery.
	code, body = getUpstream(t, base+"?namespace=production&target=mobile-api-abc-x1")
	if code != http.StatusOK || body.Service != "mobile-api" || len(body.Hosts) != 1 {
		t.Fatalf("target: status=%d body=%+v", code, body)
	}

	// A Service without routes: empty lists, not an error.
	code, body = getUpstream(t, base+"?namespace=production&service=lonely")
	if code != http.StatusOK || len(body.Hosts) != 0 || len(body.Traces) != 0 || body.Hosts == nil {
		t.Fatalf("lonely: status=%d body=%+v", code, body)
	}

	// Ingress backend.
	code, body = getUpstream(t, base+"?namespace=web&service=shop")
	if code != http.StatusOK || len(body.Hosts) != 1 {
		t.Fatalf("shop: status=%d body=%+v", code, body)
	}
	if h := body.Hosts[0]; h.Host != "shop.example.com" || h.Path != "/cart" || h.Provider != trace.ProviderIngress || h.Route != "ingress:web/shop" || h.Trace != 0 {
		t.Fatalf("shop host = %+v", h)
	}
	if len(body.Traces[0].Branches) != 1 {
		t.Fatalf("shop trace branches = %+v", body.Traces[0].Branches)
	}
}

func TestUpstreamEndpointDisabled(t *testing.T) {
	ts := httptest.NewServer((&Server{}).routes())
	defer ts.Close()
	if code, _ := getUpstream(t, ts.URL+"/api/v1/upstream?namespace=a&service=b"); code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", code)
	}
}
