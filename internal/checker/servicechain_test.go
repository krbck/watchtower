package checker

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func cachedKube(objs ...client.Object) CachedKube {
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = appsv1.AddToScheme(sch)
	_ = discoveryv1.AddToScheme(sch)
	return CachedKube{R: crfake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()}
}

func stepNames(steps []Step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Name)
	}
	return out
}

// Parity: the tail of Run and ServiceChain yields the same steps (names,
// OK, status) for the same Service, whichever Kube adapter is used.
func TestServiceChainParityWithRun(t *testing.T) {
	pod := mkPod("web-1", corev1.PodRunning, map[string]string{"app": "web"},
		[]corev1.ContainerPort{{ContainerPort: 8080}})
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)
	slice := mkSlice("web", []string{"web-1"}, nil)

	run := Run(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, pod, slice)}, "default", Input{Target: "web", ServiceName: "web"})
	for _, kube := range []Kube{ClientsetKube{C: fake.NewSimpleClientset(svc, pod, slice)}, cachedKube(svc.DeepCopy(), pod.DeepCopy(), slice.DeepCopy())} {
		ch := ServiceChain(context.Background(), kube, "default", "web", 80)
		if !ch.Passed || ch.Cut() {
			t.Fatalf("expected a passing full chain: %+v", ch.Steps)
		}
		// Run: [service, tail...]; ServiceChain: [backend service, tail...].
		if len(run.Steps) != len(ch.Steps) {
			t.Fatalf("step count differs: run=%v chain=%v", stepNames(run.Steps), stepNames(ch.Steps))
		}
		for i := 1; i < len(run.Steps); i++ {
			r, c := run.Steps[i], ch.Steps[i]
			if r.Name != c.Name || r.OK != c.OK || r.EffectiveStatus() != c.EffectiveStatus() || r.Detail != c.Detail {
				t.Errorf("step %d differs:\n run=%+v\n chain=%+v", i, r, c)
			}
		}
		if len(ch.Ports) != 1 || !ch.Ports[0].Matched || !ch.PodReady("web-1") {
			t.Errorf("facts: ports=%+v ready=%v", ch.Ports, ch.PodReady("web-1"))
		}
	}
}

func TestServiceChainMissingServiceAndPort(t *testing.T) {
	ch := ServiceChain(context.Background(), cachedKube(), "default", "ghost", 80)
	if len(ch.Steps) != 1 || ch.Steps[0].Reason != ReasonBackendServiceMissing || ch.Steps[0].Status != StatusFail {
		t.Fatalf("missing service: %+v", ch.Steps)
	}
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)
	ch = ServiceChain(context.Background(), cachedKube(svc), "default", "web", 9999)
	last := ch.Steps[len(ch.Steps)-1]
	if last.Reason != ReasonBackendPortMissing || !ch.Cut() {
		t.Fatalf("missing port: %+v", ch.Steps)
	}
}

func TestServiceChainNoReadyEndpointsAndPortReasons(t *testing.T) {
	pod := mkPod("web-1", corev1.PodRunning, map[string]string{"app": "web"},
		[]corev1.ContainerPort{{ContainerPort: 9090}})
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)
	named := corev1.ServicePort{Name: "h", Port: 81, TargetPort: intstr.FromString("http")}
	svc.Spec.Ports = append(svc.Spec.Ports, named)
	ch := ServiceChain(context.Background(), cachedKube(svc, pod, mkSlice("web", nil, []string{"web-1"})), "default", "web", 0)

	reasons := map[string]string{}
	for _, s := range ch.Steps {
		if s.Reason != "" {
			reasons[s.Reason] = s.Status
		}
	}
	if reasons[ReasonNoReadyEndpoints] != StatusFail {
		t.Errorf("noReadyEndpoints must fail: %+v", ch.Steps)
	}
	if reasons[ReasonTargetPortNotDeclared] != StatusWarn {
		t.Errorf("targetPortNotDeclared must warn (informational): %+v", ch.Steps)
	}
	if reasons[ReasonNamedPortUnresolved] != StatusFail {
		t.Errorf("namedPortUnresolved must fail: %+v", ch.Steps)
	}
	if ch.PodReady("web-1") {
		t.Error("not-ready endpoint pod must not be ready")
	}
}

// Without a target name the scaled-to-zero detection uses the workloads
// selected by the Service (CachedKube implements WorkloadLister).
func TestServiceChainScaledToZeroViaWorkloads(t *testing.T) {
	svc := mkSvc("web", map[string]string{"app": "web"}, 80, 8080)
	zero := int32(0)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web-deploy", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &zero,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}}},
		},
	}
	ch := ServiceChain(context.Background(), cachedKube(svc, dep), "default", "web", 80)
	if !ch.Passed || !ch.ScaledToZero {
		t.Fatalf("scaled-to-zero backend must be neutral: %+v", ch.Steps)
	}
	for _, s := range ch.Steps {
		if s.EffectiveStatus() == StatusFail {
			t.Errorf("no failing step expected: %+v", s)
		}
	}
	// Without WorkloadLister (clientset adapter) and without a target, the
	// empty backend is a failure.
	ch = ServiceChain(context.Background(), ClientsetKube{C: fake.NewSimpleClientset(svc, dep)}, "default", "web", 80)
	if ch.Passed {
		t.Fatalf("clientset adapter cannot see the scale: %+v", ch.Steps)
	}
}

func TestServiceChainSelectorless(t *testing.T) {
	svc := mkSvc("ext", nil, 80, 80)
	ch := ServiceChain(context.Background(), cachedKube(svc, mkSlice("ext", []string{"x"}, nil)), "default", "ext", 80)
	if !ch.Passed || ch.Steps[1].Reason != ReasonNoSelector {
		t.Fatalf("selector-less service: %+v", ch.Steps)
	}
}
