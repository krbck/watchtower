package checker

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

func findTopoNode(topo Topology, id string) *TopologyNode {
	for i := range topo.Nodes {
		if topo.Nodes[i].ID == id {
			return &topo.Nodes[i]
		}
	}
	return nil
}

// D-037: an unresolved numeric targetPort is explained on the step, the
// service node and the ghost port node.
func TestTargetPortMismatchExplained(t *testing.T) {
	pod := mkPod("web-1", corev1.PodRunning, map[string]string{"app": "web"},
		[]corev1.ContainerPort{{ContainerPort: 8080}, {Name: "admin", ContainerPort: 8081}})
	svc := mkSvc("web", map[string]string{"app": "web"}, 8082, 8082)
	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod)}, "default", Input{Target: "web"})

	wantMsg := "targetPort 8082 is not declared as a containerPort by any selected workload (web declares 8080, admin=8081)"
	for _, id := range []string{"svc", "cport:8082"} {
		n := findTopoNode(res.Topology, id)
		if n == nil || len(n.Warnings) != 1 {
			t.Fatalf("%s: expected one warning, got %+v", id, n)
		}
		w := n.Warnings[0]
		if w.Reason != ReasonTargetPortNotDeclared || w.Message != wantMsg ||
			!strings.Contains(w.Hint, "containerPort is informational") {
			t.Errorf("%s warning = %+v", id, w)
		}
	}
	last := res.Steps[len(res.Steps)-1]
	if last.OK || !strings.Contains(last.Hint, "Add containerPort 8082") {
		t.Errorf("targetPort step hint = %q", last.Hint)
	}
}

// A named targetPort that does not resolve is a real outage (no endpoint
// for that port) and the hint must say so.
func TestNamedTargetPortUnresolvedExplained(t *testing.T) {
	pod := mkPod("web-1", corev1.PodRunning, map[string]string{"app": "web"},
		[]corev1.ContainerPort{{Name: "http", ContainerPort: 8080}})
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromString("web")}},
		},
	}
	res := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod)}, "default", Input{Target: "web"})
	n := findTopoNode(res.Topology, "svc")
	if n == nil || len(n.Warnings) != 1 || n.Warnings[0].Reason != ReasonNamedPortUnresolved ||
		!strings.Contains(n.Warnings[0].Hint, "no endpoint") {
		t.Fatalf("named port warning = %+v", n)
	}
}

func TestDescribeBackends(t *testing.T) {
	got := describeBackends([]BackendPorts{
		{Name: "a", Ports: []DeclaredPort{{Port: 8081}, {Port: 8080}, {Port: 8080}}},
		{Name: "b"},
	})
	if got != "a declares 8080, 8081; b declares no containerPorts" {
		t.Errorf("describeBackends = %q", got)
	}
	if describeBackends(nil) != "no backend selected" {
		t.Error("empty backends")
	}
}
