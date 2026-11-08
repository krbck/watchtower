package trace

import (
	"context"
	"encoding/json"
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
)

// fixture accumulates cluster objects for a test.
type fixture struct {
	objs []client.Object
	snap *istio.Snapshot
}

func newFixture() *fixture { return &fixture{} }

func (f *fixture) ns(names ...string) *fixture {
	for _, n := range names {
		f.objs = append(f.objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}})
	}
	return f
}

// app adds a Deployment with one ready pod (owned via ReplicaSet), a
// Service (port → targetPort) and its EndpointSlice.
func (f *fixture) app(ns, name string, labels map[string]string, port, targetPort int32, svcType corev1.ServiceType) *corev1.Service {
	one := int32(1)
	f.objs = append(f.objs, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}},
		},
	})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-abc-x1", Namespace: ns, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: name + "-abc", APIVersion: "apps/v1", UID: "u"}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Ports: []corev1.ContainerPort{{ContainerPort: targetPort}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		}},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: labels,
			Ports:    []corev1.ServicePort{{Name: "http", Port: port, TargetPort: intstr.FromInt32(targetPort)}},
		},
	}
	ready := true
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: name + "-sl", Namespace: ns, Labels: map[string]string{discoveryv1.LabelServiceName: name}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"10.0.0.1"},
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			TargetRef:  &corev1.ObjectReference{Kind: "Pod", Namespace: ns, Name: pod.Name},
		}},
	}
	f.objs = append(f.objs, pod, svc, slice)
	return svc
}

func (f *fixture) add(objs ...client.Object) *fixture {
	f.objs = append(f.objs, objs...)
	return f
}

func (f *fixture) source() Source {
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = appsv1.AddToScheme(sch)
	_ = discoveryv1.AddToScheme(sch)
	_ = networkingv1.AddToScheme(sch)
	c := crfake.NewClientBuilder().WithScheme(sch).WithObjects(f.objs...).Build()
	snap := f.snap
	return CachedSource{Reader: c, Istio: func() *istio.Snapshot { return snap }}
}

func (f *fixture) trace(t *testing.T, host, path string) *Response {
	t.Helper()
	req, err := ParseRequest(host, path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := Trace(context.Background(), f.source(), req, Options{GraphVersion: 7})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// homelab mirrors the user's cluster (D-038 live acceptance): an Istio
// ingress gateway behind a k3s ServiceLB LoadBalancer, Gateway
// default/nginx-gateway (port 80, explicit host list) and VirtualServices
// in production/util — some of whose hosts are NOT on the gateway.
func homelab() *fixture {
	f := newFixture().ns("default", "istio-system", "production", "util", "kube-system")
	igw := f.app("istio-system", "istio-ingressgateway", map[string]string{"app": "istio-ingressgateway", "istio": "ingressgateway"}, 80, 8080, corev1.ServiceTypeLoadBalancer)
	igw.Spec.Ports[0].NodePort = 32365
	igw.Spec.Ports = append(igw.Spec.Ports, corev1.ServicePort{Name: "https", Port: 443, TargetPort: intstr.FromInt32(8443), NodePort: 31668})
	igw.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.150"}, {IP: "203.0.113.152"}, {IP: "203.0.113.153"}}
	f.app("production", "portfolio", map[string]string{"app": "portfolio"}, 80, 80, corev1.ServiceTypeClusterIP)
	f.app("production", "mobile-api", map[string]string{"app": "mobile-api"}, 80, 3000, corev1.ServiceTypeClusterIP)
	f.app("production", "v1-website", map[string]string{"app": "v1-website"}, 80, 80, corev1.ServiceTypeClusterIP)
	f.app("util", "immich-server", map[string]string{"app": "immich"}, 2283, 2283, corev1.ServiceTypeClusterIP)

	all := []istio.HTTPRule{{Match: []istio.HTTPMatch{{}}}}
	route := func(rules []istio.HTTPRule, host string, port uint32) []istio.HTTPRule {
		out := make([]istio.HTTPRule, len(rules))
		for i, r := range rules {
			r.Route = []istio.Destination{{Host: host, Port: port}}
			out[i] = r
		}
		return out
	}
	prefix := func(p string) []istio.HTTPRule {
		return []istio.HTTPRule{{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "prefix", Value: p}}}}}
	}
	gwRef := []string{"default/nginx-gateway"}
	f.snap = &istio.Snapshot{
		Gateways: []istio.Gateway{{
			Namespace: "default", Name: "nginx-gateway",
			Selector: map[string]string{"istio": "ingressgateway"},
			Servers: []istio.Server{{Port: 80, PortName: "http", Protocol: "HTTP", Hosts: []string{
				"n8n.example.com", "portfolio.example.com", "api.example.com", "immich.example.com",
			}}},
		}},
		VirtualServices: []istio.VirtualService{
			{Namespace: "production", Name: "kdu-automation", Hosts: []string{"kdu-automation.example.com"}, Gateways: gwRef, HTTP: route(prefix("/"), "kdu-automation", 3000)},
			{Namespace: "production", Name: "mobile-api", Hosts: []string{"api.example.com"}, Gateways: gwRef, HTTP: route(prefix("/api"), "mobile-api", 80)},
			{Namespace: "production", Name: "portfolio", Hosts: []string{"portfolio.example.com"}, Gateways: gwRef, HTTP: route(all, "portfolio", 80)},
			{Namespace: "production", Name: "v1-website", Hosts: []string{"website.example.com"}, Gateways: gwRef, HTTP: route(all, "v1-website", 80)},
			{Namespace: "util", Name: "immich", Hosts: []string{"immich.example.com"}, Gateways: gwRef, HTTP: route(prefix("/"), "immich-server", 2283)},
		},
	}
	return f
}

func reasonsOf(steps []Step) map[string]string {
	out := map[string]string{}
	for _, s := range steps {
		if s.Reason != "" {
			out[s.Reason] = s.Status
		}
	}
	return out
}

func lastStep(b Branch) Step { return b.Steps[len(b.Steps)-1] }

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
