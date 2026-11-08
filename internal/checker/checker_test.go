package checker

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

func mkPod(name string, phase corev1.PodPhase, podLabels map[string]string, ports []corev1.ContainerPort) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: podLabels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Ports: ports}},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func mkSvc(name string, selector map[string]string, port int32, targetPort int) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selector,
			Ports: []corev1.ServicePort{{
				Port:       port,
				TargetPort: intstr.FromInt(targetPort),
			}},
		},
	}
}

// mkSlice builds an EndpointSlice for a service; each pod becomes an endpoint
// with the given readiness.
func mkSlice(svc string, ready []string, notReady []string) *discoveryv1.EndpointSlice {
	sl := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc + "-abcde",
			Namespace: "default",
			Labels:    map[string]string{discoveryv1.LabelServiceName: svc},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	add := func(pod string, ok bool) {
		r := ok
		sl.Endpoints = append(sl.Endpoints, discoveryv1.Endpoint{
			Addresses:  []string{"10.0.0." + pod},
			Conditions: discoveryv1.EndpointConditions{Ready: &r},
			TargetRef:  &corev1.ObjectReference{Kind: "Pod", Namespace: "default", Name: pod},
		})
	}
	for _, p := range ready {
		add(p, true)
	}
	for _, p := range notReady {
		add(p, false)
	}
	return sl
}

func findStep(res Result, name string) (Step, int) {
	for i, s := range res.Steps {
		if s.Name == name {
			return s, i
		}
	}
	return Step{}, -1
}

func TestHappyPath(t *testing.T) {
	pod := mkPod("web-1", corev1.PodRunning, map[string]string{"app": "web"},
		[]corev1.ContainerPort{{ContainerPort: 8080}})
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)
	slice := mkSlice("web", []string{"web-1"}, nil)

	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod, slice)}, "default", Input{Target: "web"})

	if !res.Passed {
		t.Fatalf("expected Passed, got steps: %+v", res.Steps)
	}
	if len(res.Steps) != 6 {
		t.Fatalf("expected 6 steps (service, selector, pod phase, endpoints, containerPort, targetPort), got %d", len(res.Steps))
	}
	for _, s := range res.Steps {
		if !s.OK {
			t.Errorf("unexpected failed step: %+v", s)
		}
	}
}

func TestSelectorMismatch(t *testing.T) {
	pod := mkPod("web-1", corev1.PodRunning, map[string]string{"app": "frontend"},
		[]corev1.ContainerPort{{ContainerPort: 8080}})
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)

	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod)}, "default", Input{Target: "web"})

	if res.Passed {
		t.Fatal("chain must fail on selector mismatch")
	}
	// The chain must be cut at step 2: service OK, selector FAIL
	if len(res.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d: %+v", len(res.Steps), res.Steps)
	}
	if res.Steps[1].OK || res.Steps[1].Hint == "" {
		t.Errorf("selector step must be FAIL with a hint: %+v", res.Steps[1])
	}
}

func TestTargetPortMismatch(t *testing.T) {
	pod := mkPod("web-1", corev1.PodRunning, map[string]string{"app": "web"},
		[]corev1.ContainerPort{{ContainerPort: 9090}})
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)

	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod)}, "default", Input{Target: "web"})

	if res.Passed {
		t.Fatal("chain must fail on targetPort mismatch")
	}
	last := res.Steps[len(res.Steps)-1]
	if last.OK {
		t.Errorf("last step (targetPort) must be FAIL: %+v", last)
	}
	if last.Hint == "" {
		t.Error("targetPort failure must include a remediation hint")
	}
}

func TestServiceMissing(t *testing.T) {
	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset()}, "default", Input{Target: "ghost"})
	if res.Passed || len(res.Steps) != 1 {
		t.Fatalf("expected a single FAIL step when service is missing: %+v", res.Steps)
	}
	if res.Steps[0].OK {
		t.Error("first step must be FAIL")
	}
}

func TestPodNotRunning(t *testing.T) {
	pod := mkPod("web-1", corev1.PodPending, map[string]string{"app": "web"},
		[]corev1.ContainerPort{{ContainerPort: 8080}})
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)

	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod)}, "default", Input{Target: "web"})
	if res.Passed {
		t.Fatal("chain must fail when no pod is Running")
	}
}

func TestNamedTargetPort(t *testing.T) {
	pod := mkPod("web-1", corev1.PodRunning, map[string]string{"app": "web"},
		[]corev1.ContainerPort{{Name: "http", ContainerPort: 8080}})
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "web"},
			Ports: []corev1.ServicePort{{
				Port:       80,
				TargetPort: intstr.FromString("http"),
			}},
		},
	}

	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod, mkSlice("web", []string{"web-1"}, nil))}, "default", Input{Target: "web"})
	if !res.Passed {
		t.Fatalf("named targetPort must match, steps: %+v", res.Steps)
	}
}

func TestExplicitTargetPortMissing(t *testing.T) {
	pod := mkPod("web-1", corev1.PodRunning, map[string]string{"app": "web"},
		[]corev1.ContainerPort{{ContainerPort: 8080}})
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)

	slice := mkSlice("web", []string{"web-1"}, nil)
	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod, slice)}, "default", Input{Target: "web", Port: 8080})
	if !res.Passed {
		t.Fatalf("port 8080 must be part of the chain: %+v", res.Steps)
	}

	res = Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod, slice)}, "default", Input{Target: "web", Port: 9999})
	if res.Passed {
		t.Fatal("port 9999 not in Service → expect FAIL")
	}
}

func findNode(t *testing.T, res Result, id string) TopologyNode {
	t.Helper()
	for _, n := range res.Topology.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("node %q not found in topology: %+v", id, res.Topology.Nodes)
	return TopologyNode{}
}

func TestTopologyHappyPath(t *testing.T) {
	pod := mkPod("web-1", corev1.PodRunning, map[string]string{"app": "web"},
		[]corev1.ContainerPort{{ContainerPort: 8080}})
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)

	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod, mkSlice("web", []string{"web-1"}, nil))}, "default", Input{Target: "web"})

	// The client node is the traffic source: no check applies (OK stays nil).
	if n := findNode(t, res, "client"); n.OK != nil {
		t.Errorf("client node expected neutral (nil OK), got %+v", n)
	}
	for _, id := range []string{"svc", "pod:web-1", "cport:8080", "app"} {
		n := findNode(t, res, id)
		if n.OK == nil || !*n.OK {
			t.Errorf("node %s expected OK, got %+v", id, n)
		}
	}
	// The svc→cport edge must exist and be green.
	found := false
	for _, e := range res.Topology.Edges {
		if e.From == "svc" && e.To == "cport:8080" {
			found = true
			if e.OK == nil || !*e.OK {
				t.Errorf("svc→cport:8080 edge expected OK, got %+v", e)
			}
		}
	}
	if !found {
		t.Fatalf("svc→cport:8080 edge missing: %+v", res.Topology.Edges)
	}
}

func TestTopologyTargetPortMismatchGhostNode(t *testing.T) {
	// App listens on 9090 but the Service targets 8080.
	pod := mkPod("web-1", corev1.PodRunning, map[string]string{"app": "web"},
		[]corev1.ContainerPort{{ContainerPort: 9090}})
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)

	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod)}, "default", Input{Target: "web"})

	if res.Passed {
		t.Fatal("expected FAIL on targetPort mismatch")
	}
	// The referenced-but-undeclared port must appear as a red ghost node.
	ghost := findNode(t, res, "cport:8080")
	if ghost.OK == nil || *ghost.OK {
		t.Errorf("cport:8080 expected red ghost node, got %+v", ghost)
	}
	// The actually declared port must be green.
	declared := findNode(t, res, "cport:9090")
	if declared.OK == nil || !*declared.OK {
		t.Errorf("cport:9090 expected OK, got %+v", declared)
	}
}

func TestTopologySelectorMismatchGhostPod(t *testing.T) {
	pod := mkPod("web-1", corev1.PodRunning, map[string]string{"app": "frontend"},
		[]corev1.ContainerPort{{ContainerPort: 8080}})
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)

	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod)}, "default", Input{Target: "web"})

	ghost := findNode(t, res, "pod:none")
	if ghost.OK == nil || *ghost.OK {
		t.Errorf("pod:none expected red ghost node, got %+v", ghost)
	}
	if findNode(t, res, "svc").OK == nil || !*findNode(t, res, "svc").OK {
		t.Error("svc node expected OK (service itself exists)")
	}
}

func TestTopologyServiceMissing(t *testing.T) {
	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset()}, "default", Input{Target: "ghost"})
	if _, ok := func() (TopologyNode, bool) {
		for _, n := range res.Topology.Nodes {
			if n.ID == "svc" {
				return n, true
			}
		}
		return TopologyNode{}, false
	}(); !ok {
		t.Fatalf("svc node expected even when missing: %+v", res.Topology.Nodes)
	}
}

func TestTargetServiceMismatch(t *testing.T) {
	// Target is "ingress-gw" but the service selects "egress-gw" pods.
	pod := mkPod("egress-gw-1", corev1.PodRunning, map[string]string{"app": "egress-gw"},
		[]corev1.ContainerPort{{ContainerPort: 8080}})
	svc := mkSvc("ingress-gw-svc", map[string]string{"app": "egress-gw"}, 80, 8080)

	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod)}, "default",
		Input{Target: "ingress-gw", ServiceName: "ingress-gw-svc"})

	if res.Passed {
		t.Fatal("target/service mismatch must fail the chain")
	}
	found := false
	for _, s := range res.Steps {
		if s.Name == "Target ↔ service consistency" {
			found = true
			if s.OK || s.Hint == "" {
				t.Errorf("consistency step must FAIL with a hint: %+v", s)
			}
		}
	}
	if !found {
		t.Fatalf("consistency step missing: %+v", res.Steps)
	}
}

func TestTargetServiceConsistent(t *testing.T) {
	pod := mkPod("ingress-gw-1", corev1.PodRunning, map[string]string{"app": "ingress-gw"},
		[]corev1.ContainerPort{{ContainerPort: 8080}})
	svc := mkSvc("ingress-gw-svc", map[string]string{"app": "ingress-gw"}, 80, 8080)

	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod, mkSlice("ingress-gw-svc", []string{"ingress-gw-1"}, nil))}, "default",
		Input{Target: "ingress-gw", ServiceName: "ingress-gw-svc"})
	if !res.Passed {
		t.Fatalf("consistent target/service must pass: %+v", res.Steps)
	}
}

func TestServiceDiscovery(t *testing.T) {
	// Target redis-0; the service is named redis (not redis-0).
	pod := mkPod("redis-0", corev1.PodRunning, map[string]string{"app": "redis"},
		[]corev1.ContainerPort{{ContainerPort: 6379}})
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "redis"}}
	svc := mkSvc("redis", map[string]string{"app": "redis"}, 6379, 6379)

	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod, mkSlice("redis", []string{"redis-0"}, nil))}, "default",
		Input{Target: "redis-0"})

	if !res.Passed {
		t.Fatalf("discovery must find service redis for target redis-0: %+v", res.Steps)
	}
	found := false
	for _, s := range res.Steps {
		if s.Name == "Service discovery" && s.OK {
			found = true
		}
	}
	if !found {
		t.Fatalf("discovery step missing: %+v", res.Steps)
	}
}

func TestServiceDiscoveryNone(t *testing.T) {
	pod := mkPod("lonely-0", corev1.PodRunning, map[string]string{"app": "lonely"},
		[]corev1.ContainerPort{{ContainerPort: 8080}})
	svc := mkSvc("other", map[string]string{"app": "other"}, 80, 80)

	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod)}, "default",
		Input{Target: "lonely-0"})
	if res.Passed {
		t.Fatal("no selecting service → chain must fail")
	}
	if len(res.Steps) != 1 || res.Steps[0].OK {
		t.Fatalf("expected a single FAIL step: %+v", res.Steps)
	}
}

func TestEndpointsReadyStep(t *testing.T) {
	pod := mkPod("web-1", corev1.PodRunning, map[string]string{"app": "web"},
		[]corev1.ContainerPort{{ContainerPort: 8080}})
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)
	// Another service's slice must not be counted.
	other := mkSlice("other", nil, []string{"x"})
	other.Name = "other-zzz"

	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod, other,
		mkSlice("web", []string{"web-1"}, []string{"web-2"}))}, "default", Input{Target: "web"})

	step, idx := findStep(res, "Endpoints ready")
	if idx < 0 {
		t.Fatalf("endpoints step missing: %+v", res.Steps)
	}
	if !step.OK || step.Detail != "1 ready / 1 notReady endpoints." {
		t.Errorf("endpoints step = %+v, want PASS with counts", step)
	}
	if _, podIdx := findStep(res, "Pod phase"); podIdx != idx-1 {
		t.Errorf("endpoints step must follow the pod phase step (pod=%d, endpoints=%d)", podIdx, idx)
	}
	if n := findNode(t, res, "svc"); n.Meta["endpoints"] != "1 ready / 1 notReady" {
		t.Errorf("svc meta endpoints = %q", n.Meta["endpoints"])
	}
}

func TestEndpointsNotReady(t *testing.T) {
	// Pod is Running but its readiness probe fails: it is selected, yet it is
	// a notReady endpoint — no traffic reaches it.
	pod := mkPod("web-1", corev1.PodRunning, map[string]string{"app": "web"},
		[]corev1.ContainerPort{{ContainerPort: 8080}})
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)

	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod,
		mkSlice("web", nil, []string{"web-1"}))}, "default", Input{Target: "web"})

	if res.Passed {
		t.Fatal("zero ready endpoints must fail the chain")
	}
	step, idx := findStep(res, "Endpoints ready")
	if idx < 0 || step.OK || step.Hint == "" {
		t.Fatalf("endpoints step must FAIL with a hint: %+v", res.Steps)
	}
	// Not a cutting step: later steps still run.
	if _, i := findStep(res, "containerPort declaration"); i < 0 {
		t.Error("chain must continue after the endpoints step")
	}
	// Topology: the pod node and the svc→pod edge turn red.
	if n := findNode(t, res, "pod:web-1"); n.OK == nil || *n.OK {
		t.Errorf("not-ready pod node expected red, got %+v", n)
	}
	for _, e := range res.Topology.Edges {
		if e.From == "svc" && e.To == "pod:web-1" && (e.OK == nil || *e.OK) {
			t.Errorf("svc→pod edge expected ok=false, got %+v", e)
		}
	}
	if n := findNode(t, res, "app"); n.OK == nil || *n.OK {
		t.Errorf("app node expected red on failed chain, got %+v", n)
	}
}

func TestEndpointsNoSlice(t *testing.T) {
	pod := mkPod("web-1", corev1.PodRunning, map[string]string{"app": "web"},
		[]corev1.ContainerPort{{ContainerPort: 8080}})
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)

	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod)}, "default", Input{Target: "web"})
	if res.Passed {
		t.Fatal("no endpoints at all must fail the chain")
	}
	if step, idx := findStep(res, "Endpoints ready"); idx < 0 || step.OK {
		t.Fatalf("endpoints step must FAIL: %+v", res.Steps)
	}
}

func TestEndpointsDualStackDeduplicated(t *testing.T) {
	pod := mkPod("web-1", corev1.PodRunning, map[string]string{"app": "web"},
		[]corev1.ContainerPort{{ContainerPort: 8080}})
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)
	v4 := mkSlice("web", []string{"web-1"}, nil)
	v6 := mkSlice("web", []string{"web-1"}, nil)
	v6.Name = "web-v6"
	v6.AddressType = discoveryv1.AddressTypeIPv6

	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod, v4, v6)}, "default", Input{Target: "web"})
	if step, _ := findStep(res, "Endpoints ready"); step.Detail != "1 ready / 0 notReady endpoints." {
		t.Errorf("dual-stack endpoints must be counted once: %+v", step)
	}
}

func mkDeploy(name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
	}
}

func TestScaledToZeroNoPods(t *testing.T) {
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)
	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, mkDeploy("web", 0),
		mkSlice("web", nil, nil))}, "default", Input{Target: "web"})

	step, idx := findStep(res, "Endpoints ready")
	if idx < 0 || !step.OK || step.Detail != "workload scaled to 0 — no endpoints expected" || step.Hint == "" {
		t.Fatalf("scaled-to-zero endpoints step must be OK and informative: %+v", res.Steps)
	}
	if !res.Passed {
		t.Fatalf("scaled-to-zero target must not fail the chain: %+v", res.Steps)
	}
	for _, n := range res.Topology.Nodes {
		if n.OK != nil && !*n.OK {
			t.Errorf("no node may be red for a scaled-to-zero target: %+v", n)
		}
	}
	for _, e := range res.Topology.Edges {
		if e.OK != nil && !*e.OK {
			t.Errorf("no edge may be red for a scaled-to-zero target: %+v", e)
		}
	}
	if n := findNode(t, res, "svc"); n.Meta["endpoints"] != "scaled to 0" {
		t.Errorf("svc meta endpoints = %q, want scaled to 0", n.Meta["endpoints"])
	}
}

func TestScaledToZeroTerminatingPod(t *testing.T) {
	// Replicas set to 0 while a pod is still terminating (notReady endpoint):
	// the endpoints step stays OK and the pod/edge are not turned red.
	pod := mkPod("web-1", corev1.PodRunning, map[string]string{"app": "web"},
		[]corev1.ContainerPort{{ContainerPort: 8080}})
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)
	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod, mkDeploy("web", 0),
		mkSlice("web", nil, []string{"web-1"}))}, "default", Input{Target: "web"})

	if step, idx := findStep(res, "Endpoints ready"); idx < 0 || !step.OK || step.Detail != "workload scaled to 0 — no endpoints expected" {
		t.Fatalf("endpoints step must be OK for a scaled-to-zero target: %+v", res.Steps)
	}
	if n := findNode(t, res, "pod:web-1"); n.OK == nil || !*n.OK {
		t.Errorf("pod must not be red when the target is scaled to 0: %+v", n)
	}
	for _, e := range res.Topology.Edges {
		if e.From == "svc" && e.To == "pod:web-1" && (e.OK == nil || !*e.OK) {
			t.Errorf("svc→pod edge must not be red: %+v", e)
		}
	}
}

func TestScaledUpNoPodsStillFails(t *testing.T) {
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)
	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, mkDeploy("web", 2))}, "default", Input{Target: "web"})
	if res.Passed || len(res.Steps) != 2 || res.Steps[1].OK {
		t.Fatalf("desired>0 with no pods must keep the selector FAIL + cut: %+v", res.Steps)
	}
}
