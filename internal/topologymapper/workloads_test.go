package topologymapper

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func podTemplate(app string, port int32) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": app}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "app",
			Ports: []corev1.ContainerPort{{Name: "main", ContainerPort: port}},
		}}},
	}
}

func mkStatefulSet(ns, name string, replicas, ready int32, port int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: podTemplate(name, port),
		},
		Status: appsv1.StatefulSetStatus{ReadyReplicas: ready},
	}
}

func mkDaemonSet(ns, name string, desired, ready int32, port int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: podTemplate(name, port),
		},
		Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: desired, NumberReady: ready},
	}
}

func mkOwnedPod(ns, name, app, ownerKind, ownerName string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels:          map[string]string{"app": app},
			OwnerReferences: []metav1.OwnerReference{{Kind: ownerKind, Name: ownerName}},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// mkSlice builds an EndpointSlice for a service with the given pod
// endpoints' readiness.
func mkSlice(ns, svc, name string, ready, notReady []string) *discoveryv1.EndpointSlice {
	sl := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels: map[string]string{discoveryv1.LabelServiceName: svc},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	add := func(pod string, ok bool) {
		r := ok
		sl.Endpoints = append(sl.Endpoints, discoveryv1.Endpoint{
			Addresses:  []string{"10.1.0.1"},
			Conditions: discoveryv1.EndpointConditions{Ready: &r},
			TargetRef:  &corev1.ObjectReference{Kind: "Pod", Namespace: ns, Name: pod},
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

func TestStatefulSetAndDaemonSetNodes(t *testing.T) {
	c := fakeClient(
		mkNamespace("data"),
		mkStatefulSet("data", "redis", 2, 1, 6379),
		mkOwnedPod("data", "redis-0", "redis", "StatefulSet", "redis", corev1.PodRunning),
		mkOwnedPod("data", "redis-1", "redis", "StatefulSet", "redis", corev1.PodPending),
		mkService("data", "redis", map[string]string{"app": "redis"}, 6379, 6379),
		mkDaemonSet("data", "agent", 3, 3, 9100),
		mkOwnedPod("data", "agent-a", "agent", "DaemonSet", "agent", corev1.PodRunning),
		mkOwnedPod("data", "agent-b", "agent", "DaemonSet", "agent", corev1.PodRunning),
		mkService("data", "agent", map[string]string{"app": "agent"}, 80, 9999), // targetPort not declared
	)
	g, err := Build(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}

	sts := g.findNode("sts:data/redis")
	if sts == nil || sts.Kind != "statefulset" {
		t.Fatalf("statefulset node missing/wrong: %+v", sts)
	}
	if sts.Meta["pods"] != "1/2 running" || sts.Meta["replicas"] != "1/2 ready" {
		t.Errorf("statefulset meta = %+v", sts.Meta)
	}
	if len(sts.Ports) != 1 || sts.Ports[0].Port != 6379 {
		t.Errorf("statefulset ports = %+v", sts.Ports)
	}
	ds := g.findNode("ds:data/agent")
	if ds == nil || ds.Kind != "daemonset" {
		t.Fatalf("daemonset node missing/wrong: %+v", ds)
	}
	if ds.Meta["pods"] != "2/2 running" || ds.Meta["replicas"] != "3/3 ready" {
		t.Errorf("daemonset meta = %+v", ds.Meta)
	}

	// Owned pods are folded into the workload cards.
	for _, id := range []string{"pod:data/redis-0", "pod:data/redis-1", "pod:data/agent-a"} {
		if g.findNode(id) != nil {
			t.Errorf("workload-owned pod %s must not be a separate node", id)
		}
	}

	// Service → workload edges; services are the chain roots (D-032).
	if !g.hasEdge("svc:data/redis", "sts:data/redis") || !g.hasEdge("svc:data/agent", "ds:data/agent") {
		t.Errorf("service→workload edges missing: %+v", g.Edges)
	}
	if g.hasEdge("ns:data", "sts:data/redis") || !g.hasEdge("ns:data", "svc:data/redis") {
		t.Error("namespace anchoring must target the service, not the statefulset")
	}

	// Port ✓/⚠ matching considers the new workload kinds.
	if svc := g.findNode("svc:data/redis"); svc == nil || len(svc.Ports) != 1 || !svc.Ports[0].Matched {
		t.Errorf("redis service port must match the statefulset containerPort: %+v", svc)
	}
	if svc := g.findNode("svc:data/agent"); svc == nil || len(svc.Ports) != 1 || svc.Ports[0].Matched {
		t.Errorf("agent service targetPort 9999 must be unmatched: %+v", svc)
	}
}

func TestUnconnectedStatefulSetAnchored(t *testing.T) {
	c := fakeClient(mkNamespace("data"), mkStatefulSet("data", "db", 1, 1, 5432))
	g, err := Build(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if !g.hasEdge("ns:data", "sts:data/db") {
		t.Error("chain-root statefulset must be anchored to its namespace")
	}
}

func TestOrphanReplicaSetPodStaysStandalone(t *testing.T) {
	// ReplicaSet without a matching Deployment: the pod is not folded away.
	c := fakeClient(
		mkNamespace("default"),
		mkOwnedPod("default", "lone-abc-x", "lone", "ReplicaSet", "lone-abc", corev1.PodRunning),
	)
	g, err := Build(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if g.findNode("pod:default/lone-abc-x") == nil {
		t.Error("pod of an unknown workload must stay visible as a standalone pod")
	}
}

func TestEndpointSliceCountsAndOkFlag(t *testing.T) {
	c := fakeClient(
		mkNamespace("default"),
		// web: 2 ready + 1 notReady (split across two slices, one duplicated
		// endpoint as in dual-stack) → healthy, edge not flagged.
		mkDeployment("default", "web", 3),
		mkService("default", "web", map[string]string{"app": "web"}, 80, 80),
		mkSlice("default", "web", "web-a", []string{"web-1", "web-2"}, nil),
		mkSlice("default", "web", "web-b", []string{"web-2"}, []string{"web-3"}),
		// api: selector matches, but every endpoint is notReady → flagged.
		mkDeployment("default", "api", 1),
		mkService("default", "api", map[string]string{"app": "api"}, 80, 80),
		mkSlice("default", "api", "api-a", nil, []string{"api-1"}),
		// legacy: selector-less service with a manual slice and no ready
		// endpoints → counted but never flagged (no edges to flag anyway).
		mkService("default", "legacy", nil, 80, 80),
		mkSlice("default", "legacy", "legacy-a", nil, []string{"ext"}),
	)
	g, err := Build(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}

	if n := g.findNode("svc:default/web"); n == nil || n.Meta["endpoints"] != "2 ready / 1 notReady" {
		t.Errorf("web endpoints meta = %+v", n)
	}
	if e := g.findEdge("svc:default/web", "dep:default/web"); e == nil || e.OK != nil {
		t.Errorf("healthy service edge must exist without an ok flag: %+v", e)
	}

	if n := g.findNode("svc:default/api"); n == nil || n.Meta["endpoints"] != "0 ready / 1 notReady" {
		t.Errorf("api endpoints meta = %+v", n)
	}
	if e := g.findEdge("svc:default/api", "dep:default/api"); e == nil || e.OK == nil || *e.OK {
		t.Errorf("service with zero ready endpoints must flag its edge ok=false: %+v", e)
	}

	if n := g.findNode("svc:default/legacy"); n == nil || n.Meta["endpoints"] != "0 ready / 1 notReady" {
		t.Errorf("legacy endpoints meta = %+v", n)
	}
	for _, e := range g.Edges {
		if e.From == "svc:default/legacy" && e.OK != nil {
			t.Errorf("selector-less service must not be flagged: %+v", e)
		}
	}
}

func TestBuildDeterministic(t *testing.T) {
	objs := func() *Graph {
		c := fakeClient(
			mkNamespace("b"), mkNamespace("a"),
			mkDeployment("b", "z", 1), mkDeployment("a", "y", 1), mkDeployment("a", "x", 1),
			mkService("a", "x", map[string]string{"app": "x"}, 80, 80),
			mkStatefulSet("b", "s", 1, 1, 80),
		)
		g, err := Build(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	h1, _ := graphHash(objs())
	for i := 0; i < 5; i++ {
		if h, _ := graphHash(objs()); h != h1 {
			t.Fatal("Build output must be deterministic for identical input")
		}
	}
}

func TestScaledToZeroServiceNotFlagged(t *testing.T) {
	c := fakeClient(
		mkNamespace("default"),
		mkDeployment("default", "idle", 0),
		mkService("default", "idle", map[string]string{"app": "idle"}, 80, 80),
		mkSlice("default", "idle", "idle-a", nil, nil),
	)
	g, err := Build(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if e := g.findEdge("svc:default/idle", "dep:default/idle"); e == nil || e.OK != nil {
		t.Errorf("scaled-to-zero backend: edge must exist and stay neutral, got %+v", e)
	}
	if n := g.findNode("svc:default/idle"); n == nil || n.Meta["endpoints"] != "0 ready / 0 notReady · scaled to 0" {
		t.Errorf("service meta endpoints = %+v", n)
	}
}

func TestMixedScaleServiceStillFlagged(t *testing.T) {
	zero := mkDeployment("default", "a", 0)
	two := mkDeployment("default", "b", 2)
	for _, d := range []*appsv1.Deployment{zero, two} {
		d.Spec.Selector.MatchLabels["tier"] = "web"
	}
	c := fakeClient(
		mkNamespace("default"), zero, two,
		mkService("default", "web", map[string]string{"tier": "web"}, 80, 80),
		mkSlice("default", "web", "web-a", nil, []string{"b-1"}),
	)
	g, err := Build(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"dep:default/a", "dep:default/b"} {
		if e := g.findEdge("svc:default/web", to); e == nil || e.OK == nil || *e.OK {
			t.Errorf("mixed scale (one backend wants replicas) must stay red: %s → %+v", to, e)
		}
	}
	if n := g.findNode("svc:default/web"); n == nil || n.Meta["endpoints"] != "0 ready / 1 notReady" {
		t.Errorf("mixed service meta = %+v", n)
	}
}
