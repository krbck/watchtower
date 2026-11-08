package istio

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func istioResources(version string, withGateways bool) *metav1.APIResourceList {
	rl := &metav1.APIResourceList{
		GroupVersion: Group + "/" + version,
		APIResources: []metav1.APIResource{{Name: "virtualservices", Namespaced: true, Kind: "VirtualService"}},
	}
	if withGateways {
		rl.APIResources = append(rl.APIResources, metav1.APIResource{Name: "gateways", Namespaced: true, Kind: "Gateway"})
	}
	return rl
}

func mkVS(version, ns, name string, hosts ...string) *unstructured.Unstructured {
	h := make([]interface{}, 0, len(hosts))
	for _, x := range hosts {
		h = append(h, x)
	}
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"hosts": h},
	}}
	u.SetAPIVersion(Group + "/" + version)
	u.SetKind("VirtualService")
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func mkGW(version, ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"selector": map[string]interface{}{"istio": "ingressgateway"}},
	}}
	u.SetAPIVersion(Group + "/" + version)
	u.SetKind("Gateway")
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func fakeDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	lists := map[schema.GroupVersionResource]string{}
	for _, v := range candidateVersions {
		lists[schema.GroupVersionResource{Group: Group, Version: v, Resource: "virtualservices"}] = "VirtualServiceList"
		lists[schema.GroupVersionResource{Group: Group, Version: v, Resource: "gateways"}] = "GatewayList"
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists)
	// Add through the tracker with explicit GVRs: the fake's plural guess
	// for "Gateway" would be "gatewaies".
	for _, o := range objs {
		u := o.(*unstructured.Unstructured)
		gv, _ := schema.ParseGroupVersion(u.GetAPIVersion())
		res := "virtualservices"
		if u.GetKind() == "Gateway" {
			res = "gateways"
		}
		if err := dyn.Tracker().Create(gv.WithResource(res), u, u.GetNamespace()); err != nil {
			panic(err)
		}
	}
	return dyn
}

func newTestIntegration(mode Mode, disc *fakediscovery.FakeDiscovery, objs ...runtime.Object) (*Integration, *dynamicfake.FakeDynamicClient) {
	dyn := fakeDynamic(objs...)
	i := New(mode, disc, dyn, nil)
	i.kick = make(chan struct{}, 1)
	return i, dyn
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// mode × detected matrix: only (auto|true) ∧ detected becomes active.
func TestModeDetectedMatrix(t *testing.T) {
	cases := []struct {
		mode     Mode
		detected bool
		want     string
	}{
		{ModeAuto, true, StatusActive},
		{ModeAuto, false, StatusNotDetected},
		{ModeEnabled, true, StatusActive},
		{ModeEnabled, false, StatusMissing},
		{ModeDisabled, true, StatusDisabled},
		{ModeDisabled, false, StatusDisabled},
	}
	for _, c := range cases {
		disc := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
		if c.detected {
			disc.Resources = []*metav1.APIResourceList{istioResources("v1", true)}
		}
		i, _ := newTestIntegration(c.mode, disc, mkVS("v1", "production", "a", "a.com"))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = i.Start(ctx) }()
		waitUntil(t, func() bool { return i.Status() == c.want })
		if c.want == StatusActive {
			waitUntil(t, func() bool { return i.Snapshot() != nil })
			if got := i.Snapshot(); len(got.VirtualServices) != 1 || got.VirtualServices[0].Key() != "production/a" {
				t.Errorf("%s/%v: unexpected snapshot %+v", c.mode, c.detected, got)
			}
		} else if i.Snapshot() != nil {
			t.Errorf("%s/%v: inactive integration must have a nil snapshot", c.mode, c.detected)
		}
		cancel()
		<-done
	}
}

func TestDetectPrefersV1FallsBackToV1beta1(t *testing.T) {
	disc := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
	disc.Resources = []*metav1.APIResourceList{istioResources("v1beta1", false)}
	i, _ := newTestIntegration(ModeAuto, disc)
	gv, hasGW, err := i.detect()
	if err != nil || gv.Version != "v1beta1" || hasGW {
		t.Fatalf("detect = %v,%v,%v; want v1beta1 without gateways", gv, hasGW, err)
	}
	disc.Resources = append(disc.Resources, istioResources("v1", true))
	gv, hasGW, err = i.detect()
	if err != nil || gv.Version != "v1" || !hasGW {
		t.Fatalf("detect = %v,%v,%v; want v1 with gateways", gv, hasGW, err)
	}
}

// CRDs appearing/disappearing: reconcile starts and stops the informers and
// the snapshot follows (nil when gone); listeners are notified on change.
func TestReconcileStartStopFollowsDetection(t *testing.T) {
	disc := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
	i, _ := newTestIntegration(ModeAuto, disc, mkVS("v1", "p", "a", "a.com"), mkGW("v1", "default", "gw"))
	var notified atomic.Int64
	cancelSub := i.Subscribe(func() { notified.Add(1) })
	defer cancelSub()
	ctx := context.Background()

	i.reconcile(ctx)
	if i.Status() != StatusNotDetected || i.running != nil {
		t.Fatalf("not installed: status=%s running=%v", i.Status(), i.running != nil)
	}

	disc.Resources = []*metav1.APIResourceList{istioResources("v1", true)}
	i.reconcile(ctx)
	if i.Status() != StatusActive || i.running == nil {
		t.Fatalf("installed: status=%s", i.Status())
	}
	waitUntil(t, func() bool { s := i.Snapshot(); return s != nil && len(s.Gateways) == 1 && len(s.VirtualServices) == 1 })
	if notified.Load() != 1 {
		t.Fatalf("first snapshot must notify once, got %d", notified.Load())
	}
	// Idempotent: nothing changes, no restart, no notification.
	rs := i.running
	i.reconcile(ctx)
	if i.running != rs || notified.Load() != 1 {
		t.Fatal("reconcile with unchanged detection must be a no-op")
	}

	disc.Resources = nil
	i.reconcile(ctx)
	if i.running != nil || i.Snapshot() != nil || i.Status() != StatusNotDetected {
		t.Fatalf("uninstalled: running=%v snapshot=%v status=%s", i.running != nil, i.Snapshot(), i.Status())
	}
	if notified.Load() != 2 {
		t.Fatalf("snapshot cleared must notify, got %d", notified.Load())
	}
}

// Live updates flow from the private informers into the snapshot.
func TestSnapshotFollowsInformerEvents(t *testing.T) {
	disc := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
	disc.Resources = []*metav1.APIResourceList{istioResources("v1", true)}
	i, dyn := newTestIntegration(ModeAuto, disc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = i.Start(ctx) }()
	waitUntil(t, func() bool { return i.Snapshot() != nil })

	gvr := schema.GroupVersionResource{Group: Group, Version: "v1", Resource: "virtualservices"}
	if _, err := dyn.Resource(gvr).Namespace("p").Create(ctx, mkVS("v1", "p", "new", "n.com"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { s := i.Snapshot(); return s != nil && len(s.VirtualServices) == 1 })
	cancel()
	<-done
}

// Forbidden (missing RBAC): degraded, no informers left retrying, no panic.
func TestForbiddenDegrades(t *testing.T) {
	disc := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
	disc.Resources = []*metav1.APIResourceList{istioResources("v1", true)}
	i, dyn := newTestIntegration(ModeAuto, disc)
	dyn.PrependReactor("list", "virtualservices", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: Group, Resource: "virtualservices"}, "", nil)
	})
	i.reconcile(context.Background())
	if i.Status() != StatusDegraded || i.running != nil || i.Snapshot() != nil {
		t.Fatalf("forbidden: status=%s running=%v", i.Status(), i.running != nil)
	}
}

func TestNilIntegrationIsSafe(t *testing.T) {
	var i *Integration
	if i.Snapshot() != nil || i.Status() != StatusDisabled {
		t.Fatal("nil integration must report no snapshot / disabled")
	}
	i.Subscribe(func() {})()
}

// Gateways becoming served later (same group version) must restart the
// informers so the snapshot picks them up (qa D-037: reconcile used to
// early-return on an unchanged group version).
func TestReconcileRestartsWhenGatewaysAppear(t *testing.T) {
	disc := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
	disc.Resources = []*metav1.APIResourceList{istioResources("v1", false)}
	i, _ := newTestIntegration(ModeAuto, disc, mkVS("v1", "p", "a", "a.com"), mkGW("v1", "default", "gw"))
	ctx := context.Background()
	defer i.stopInformers()

	i.reconcile(ctx)
	if i.running == nil || i.running.hasGW {
		t.Fatalf("expected informers without gateways, running=%+v", i.running)
	}
	waitUntil(t, func() bool { s := i.Snapshot(); return s != nil && len(s.VirtualServices) == 1 })
	if s := i.Snapshot(); len(s.Gateways) != 0 {
		t.Fatalf("gateways not served yet, snapshot has %d", len(s.Gateways))
	}

	first := i.running
	disc.Resources = []*metav1.APIResourceList{istioResources("v1", true)}
	i.reconcile(ctx)
	if i.running == first || i.running == nil || !i.running.hasGW {
		t.Fatal("gateways appearing must restart the informers with the gateways informer")
	}
	waitUntil(t, func() bool { s := i.Snapshot(); return s != nil && len(s.Gateways) == 1 })

	// And back: gateways no longer served → restart without them.
	second := i.running
	disc.Resources = []*metav1.APIResourceList{istioResources("v1", false)}
	i.reconcile(ctx)
	if i.running == second || i.running == nil || i.running.hasGW {
		t.Fatal("gateways disappearing must restart the informers without gateways")
	}
	waitUntil(t, func() bool { s := i.Snapshot(); return s != nil && len(s.Gateways) == 0 })
}
