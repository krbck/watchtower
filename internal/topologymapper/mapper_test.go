package topologymapper

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	watchtowerv1alpha1 "github.com/krbck/watchtower/api/v1alpha1"
)

func fakeClient(objs ...client.Object) client.Client {
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = appsv1.AddToScheme(sch)
	_ = networkingv1.AddToScheme(sch)
	_ = discoveryv1.AddToScheme(sch)
	_ = watchtowerv1alpha1.AddToScheme(sch)
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
}

func mkNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func mkDeployment(ns, name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
						Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 80}},
					}},
				},
			},
		},
		Status: appsv1.DeploymentStatus{AvailableReplicas: replicas},
	}
}

func mkPodForDep(ns, depName, podName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
			Labels:    map[string]string{"app": depName},
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "ReplicaSet",
				Name: depName + "-abc123",
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func mkStandalonePod(ns, name string, labels map[string]string, ports []corev1.ContainerPort) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "app", Ports: ports}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func mkService(ns, name string, selector map[string]string, port, targetPort int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selector,
			Ports: []corev1.ServicePort{{
				Port:       port,
				TargetPort: intstr.FromInt(int(targetPort)),
			}},
		},
	}
}

func TestBuildBasics(t *testing.T) {
	c := fakeClient(
		mkNamespace("default"),
		mkNamespace("kube-system"), // must be filtered out
		mkDeployment("default", "web", 2),
		mkPodForDep("default", "web", "web-abc123-x1"),
		mkPodForDep("default", "web", "web-abc123-x2"),
		mkService("default", "web", map[string]string{"app": "web"}, 80, 80),
	)

	g, err := Build(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}

	if g.findNode("ns:default") == nil {
		t.Error("namespace node missing")
	}
	if g.findNode("ns:kube-system") != nil {
		t.Error("kube-system namespace must be excluded")
	}
	if g.findNode("dep:default/web") == nil {
		t.Error("deployment node missing")
	}
	if g.findNode("svc:default/web") == nil {
		t.Error("service node missing")
	}
	// Deployment-owned pods are folded into the deployment card (D-017).
	if g.findNode("pod:default/web-abc123-x1") != nil {
		t.Error("deployment-owned pod must not be a separate node")
	}
	// Deployment meta carries the pod count.
	if dn := g.findNode("dep:default/web"); dn != nil && dn.Meta["pods"] != "2/2 running" {
		t.Errorf("deployment meta pods = %q, want 2/2 running", dn.Meta["pods"])
	}
	// Service selects the deployment.
	if !g.hasEdge("svc:default/web", "dep:default/web") {
		t.Error("service→deployment edge missing")
	}
	// Only the chain root (service) is anchored to the namespace; the
	// deployment is already connected through the service (D-032).
	if !g.hasEdge("ns:default", "svc:default/web") {
		t.Error("namespace→service edge missing (chain root)")
	}
	if g.hasEdge("ns:default", "dep:default/web") {
		t.Error("connected deployment must not get a direct namespace edge")
	}
	// Ports are structured on the service card.
	if sn := g.findNode("svc:default/web"); sn != nil {
		if len(sn.Ports) != 1 || sn.Ports[0].Port != 80 || sn.Ports[0].TargetPort != "80" || !sn.Ports[0].Matched {
			t.Errorf("service ports = %+v, want [{80 80 matched}]", sn.Ports)
		}
	}
	if dn := g.findNode("dep:default/web"); dn != nil {
		if len(dn.Ports) != 1 || dn.Ports[0].Port != 80 {
			t.Errorf("deployment ports = %+v, want [80]", dn.Ports)
		}
	}
	// No standalone port nodes anymore.
	for _, n := range g.Nodes {
		if n.Kind == "port" {
			t.Errorf("port node must not exist in the overview map: %+v", n)
		}
	}
}

func TestStandalonePodWithPorts(t *testing.T) {
	c := fakeClient(
		mkNamespace("default"),
		mkStandalonePod("default", "debug-shell", map[string]string{"app": "debug"},
			[]corev1.ContainerPort{{ContainerPort: 8080}, {ContainerPort: 9090}}),
		mkService("default", "debug", map[string]string{"app": "debug"}, 80, 8080),
	)

	g, err := Build(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	pod := g.findNode("pod:default/debug-shell")
	if pod == nil {
		t.Fatal("standalone pod node missing")
	}
	if len(pod.Ports) != 2 || pod.Ports[0].Port != 8080 || pod.Ports[1].Port != 9090 {
		t.Errorf("pod ports = %+v, want [8080 9090]", pod.Ports)
	}
	if pod.Meta["node"] != "node-1" {
		t.Errorf("pod meta node = %q, want node-1", pod.Meta["node"])
	}
	if !g.hasEdge("svc:default/debug", "pod:default/debug-shell") {
		t.Error("service→standalone pod edge missing")
	}
}

func TestBuildIngress(t *testing.T) {
	c := fakeClient(
		mkNamespace("default"),
		mkService("default", "web", map[string]string{"app": "web"}, 80, 80),
		&networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: "web-ing", Namespace: "default"},
			Spec: networkingv1.IngressSpec{
				Rules: []networkingv1.IngressRule{{
					Host: "myapp.com",
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{{
								Path: "/",
								Backend: networkingv1.IngressBackend{
									Service: &networkingv1.IngressServiceBackend{
										Name: "web",
										Port: networkingv1.ServiceBackendPort{Number: 80},
									},
								},
							}},
						},
					},
				}},
			},
		},
	)

	g, err := Build(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if g.findNode("ingress:default/web-ing") == nil {
		t.Fatal("ingress node missing")
	}
	if !g.hasEdge("ingress:default/web-ing", "svc:default/web") {
		t.Error("ingress→service edge missing")
	}
	if n := g.findNode("ingress:default/web-ing"); n != nil && n.Detail != "myapp.com" {
		t.Errorf("ingress detail = %q, want myapp.com", n.Detail)
	}
}

func TestBuildSystemNamespaceFiltered(t *testing.T) {
	c := fakeClient(
		mkNamespace("kube-system"),
		mkPodForDep("kube-system", "coredns", "coredns-1"),
	)
	g, err := Build(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range g.Nodes {
		if n.Namespace == "kube-system" || n.ID == "ns:kube-system" {
			t.Errorf("system namespace leaked into graph: %+v", n)
		}
	}
}

func TestUnconnectedDeploymentAnchored(t *testing.T) {
	c := fakeClient(
		mkNamespace("default"),
		mkDeployment("default", "worker", 1), // no service selects it
	)
	g, err := Build(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if !g.hasEdge("ns:default", "dep:default/worker") {
		t.Error("chain-root deployment must be anchored to its namespace")
	}
}

func TestIngressIsChainRoot(t *testing.T) {
	c := fakeClient(
		mkNamespace("default"),
		mkDeployment("default", "web", 1),
		mkService("default", "web", map[string]string{"app": "web"}, 80, 80),
		&networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: "web-ing", Namespace: "default"},
			Spec: networkingv1.IngressSpec{
				Rules: []networkingv1.IngressRule{{
					Host: "myapp.com",
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{{
								Backend: networkingv1.IngressBackend{
									Service: &networkingv1.IngressServiceBackend{
										Name: "web",
										Port: networkingv1.ServiceBackendPort{Number: 80},
									},
								},
							}},
						},
					},
				}},
			},
		},
	)
	g, err := Build(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if !g.hasEdge("ns:default", "ingress:default/web-ing") {
		t.Error("ingress must be anchored to the namespace (chain root)")
	}
	if g.hasEdge("ns:default", "svc:default/web") {
		t.Error("service routed by an ingress must not get a direct namespace edge")
	}
	if g.hasEdge("ns:default", "dep:default/web") {
		t.Error("deployment must not get a direct namespace edge")
	}
}

func TestSelectorMismatchNoEdge(t *testing.T) {
	c := fakeClient(
		mkNamespace("default"),
		mkDeployment("default", "web", 1),
		mkService("default", "web", map[string]string{"app": "other"}, 80, 80),
	)
	g, err := Build(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if g.hasEdge("svc:default/web", "dep:default/web") {
		t.Error("selector mismatch must not create service→deployment edge")
	}
}
