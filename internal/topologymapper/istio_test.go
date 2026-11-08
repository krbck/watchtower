package topologymapper

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/krbck/watchtower/internal/checker"
	"github.com/krbck/watchtower/internal/integrations/istio"
)

// mkLabeledDeployment is a deployment whose pod template carries labels and
// the given containerPorts.
func mkLabeledDeployment(ns, name string, labels map[string]string, ports ...int32) *appsv1.Deployment {
	one := int32(1)
	cps := make([]corev1.ContainerPort, 0, len(ports))
	for _, p := range ports {
		cps = append(cps, corev1.ContainerPort{ContainerPort: p})
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Ports: cps}}},
			},
		},
		Status: appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
}

// liveLikeCluster mirrors the user's cluster: an LB ingress gateway in
// istio-system, Gateway default/nginx-gateway, VirtualServices in production
// and util.
func liveLikeCluster() ([]client.Object, *istio.Snapshot) {
	igwLabels := map[string]string{"app": "istio-ingressgateway", "istio": "ingressgateway"}
	lb := mkService("istio-system", "istio-ingressgateway", igwLabels, 80, 8080)
	lb.Spec.Type = corev1.ServiceTypeLoadBalancer
	lb.Spec.ExternalIPs = []string{"203.0.113.150", "203.0.113.152"}
	lb.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.150"}, {Hostname: "lb.example.net"}}

	objs := []client.Object{
		mkNamespace("default"), mkNamespace("istio-system"), mkNamespace("production"), mkNamespace("util"),
		mkLabeledDeployment("istio-system", "istio-ingressgateway", igwLabels, 8080, 8443),
		lb,
		mkLabeledDeployment("production", "portfolio", map[string]string{"app": "portfolio"}, 80),
		mkService("production", "portfolio", map[string]string{"app": "portfolio"}, 80, 80),
		mkLabeledDeployment("production", "express-website", map[string]string{"app": "express-website"}, 80),
		mkService("production", "express-website", map[string]string{"app": "express-website"}, 80, 80),
		mkLabeledDeployment("util", "immich-server", map[string]string{"app": "immich"}, 2283),
		mkService("util", "immich-server", map[string]string{"app": "immich"}, 2283, 2283),
	}
	route := func(host string, port uint32) []istio.Route {
		return []istio.Route{{Protocol: "http", Match: "/", Host: host, Port: port}}
	}
	snap := &istio.Snapshot{
		Gateways: []istio.Gateway{{
			Namespace: "default", Name: "nginx-gateway",
			Selector: map[string]string{"istio": "ingressgateway"},
			Servers: []istio.Server{{Port: 80, PortName: "http", Protocol: "HTTP",
				Hosts: []string{"portfolio.example.com", "immich.example.com"}}},
		}},
		VirtualServices: []istio.VirtualService{
			{Namespace: "production", Name: "express-website", Hosts: []string{"website.example.net"},
				Gateways: []string{"default/nginx-gateway"}, Routes: route("express-website", 80)},
			{Namespace: "production", Name: "portfolio", Hosts: []string{"portfolio.example.com"},
				Gateways: []string{"default/nginx-gateway", "mesh"}, Routes: route("portfolio", 80)},
			{Namespace: "util", Name: "immich", Hosts: []string{"immich.example.com"},
				Gateways: []string{"default/nginx-gateway"}, Routes: route("immich-server.util.svc.cluster.local", 2283)},
		},
	}
	return objs, snap
}

func buildWithIstio(t *testing.T, snap *istio.Snapshot, objs ...client.Object) *Graph {
	t.Helper()
	g, err := BuildWith(context.Background(), fakeClient(objs...), BuildOptions{Istio: snap})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func warningReasons(n *checker.TopologyNode) []string {
	out := []string{}
	for _, w := range n.Warnings {
		out = append(out, w.Reason)
	}
	return out
}

func TestIstioNorthSouthChain(t *testing.T) {
	objs, snap := liveLikeCluster()
	g := buildWithIstio(t, snap, objs...)

	gw := g.findNode("gw:default/nginx-gateway")
	if gw == nil || gw.Kind != "gateway" || gw.Namespace != "default" {
		t.Fatalf("gateway node missing: %+v", gw)
	}
	if gw.Meta["servers"] != "http:80 → 2 hosts" || gw.Meta["selector"] != "istio=ingressgateway" ||
		!strings.Contains(gw.Meta["hosts"], "immich.example.com") {
		t.Errorf("gateway meta = %v", gw.Meta)
	}
	if len(gw.Warnings) != 0 {
		t.Errorf("served gateway must have no warnings: %+v", gw.Warnings)
	}
	for _, id := range []string{"vs:production/express-website", "vs:production/portfolio", "vs:util/immich"} {
		n := g.findNode(id)
		if n == nil || n.Kind != "virtualservice" {
			t.Fatalf("missing VS node %s", id)
		}
		if e := g.findEdge("gw:default/nginx-gateway", id); e == nil || e.Label != "binds" {
			t.Errorf("missing binds edge gw → %s: %+v", id, e)
		}
	}

	// Full chain: LB svc → gateway workload → gateway → VS → svc → workload.
	chain := [][2]string{
		{"svc:istio-system/istio-ingressgateway", "dep:istio-system/istio-ingressgateway"},
		{"dep:istio-system/istio-ingressgateway", "gw:default/nginx-gateway"},
		{"gw:default/nginx-gateway", "vs:production/portfolio"},
		{"vs:production/portfolio", "svc:production/portfolio"},
		{"svc:production/portfolio", "dep:production/portfolio"},
	}
	for _, c := range chain {
		if !g.hasEdge(c[0], c[1]) {
			t.Errorf("missing chain edge %s → %s", c[0], c[1])
		}
	}
	if e := g.findEdge("dep:istio-system/istio-ingressgateway", "gw:default/nginx-gateway"); e.Label != "serves" {
		t.Errorf("workload → gateway label = %q, want serves", e.Label)
	}
	if e := g.findEdge("vs:production/portfolio", "svc:production/portfolio"); e.Label != "routes → 80" || e.OK != nil {
		t.Errorf("routes edge = %+v", e)
	}
	// FQDN destination resolves cross-namespace.
	if !g.hasEdge("vs:util/immich", "svc:util/immich-server") {
		t.Error("FQDN destination must resolve to svc:util/immich-server")
	}

	// D-032 anchoring: only the LB service (chain root) is anchored.
	if !g.hasEdge("ns:istio-system", "svc:istio-system/istio-ingressgateway") {
		t.Error("LB service is the chain root and must be anchored")
	}
	for _, id := range []string{
		"gw:default/nginx-gateway", "vs:production/portfolio", "vs:util/immich",
		"svc:production/portfolio", "dep:istio-system/istio-ingressgateway",
	} {
		ns := "ns:" + strings.SplitN(strings.SplitN(id, ":", 2)[1], "/", 2)[0]
		if g.hasEdge(ns, id) {
			t.Errorf("%s has an incoming edge and must not be anchored (D-032)", id)
		}
	}

	// LB entry point meta.
	lb := g.findNode("svc:istio-system/istio-ingressgateway")
	if lb.Meta["external"] != "203.0.113.150, lb.example.net, 203.0.113.152" {
		t.Errorf("LB external meta = %q", lb.Meta["external"])
	}
}

func TestIstioHostExposureWarning(t *testing.T) {
	objs, snap := liveLikeCluster()
	g := buildWithIstio(t, snap, objs...)

	ew := g.findNode("vs:production/express-website")
	if got := warningReasons(ew); len(got) != 1 || got[0] != ReasonVSHostNotExposed {
		t.Fatalf("express-website warnings = %v", got)
	}
	if !strings.Contains(ew.Warnings[0].Message, "website.example.net") ||
		!strings.Contains(ew.Warnings[0].Message, "default/nginx-gateway") || ew.Warnings[0].Hint == "" {
		t.Errorf("warning must name host + gateway: %+v", ew.Warnings[0])
	}
	if w := ew.Warnings[0]; !strings.Contains(w.Message, "(2 server hosts)") ||
		len(w.Items) != 2 || !sort.StringsAreSorted(w.Items) || strings.Contains(w.Message, w.Items[0]) {
		t.Errorf("the server hosts belong in Items, not the message: %+v", w)
	}
	if e := g.findEdge("gw:default/nginx-gateway", "vs:production/express-website"); e.OK == nil || *e.OK {
		t.Errorf("binds edge with no exposed host must be ok=false: %+v", e)
	}
	pf := g.findNode("vs:production/portfolio")
	if len(pf.Warnings) != 0 {
		t.Errorf("portfolio host is exposed; warnings = %+v", pf.Warnings)
	}
	if e := g.findEdge("gw:default/nginx-gateway", "vs:production/portfolio"); e.OK != nil {
		t.Errorf("exposed binds edge must stay neutral: %+v", e)
	}
}

func TestIstioMissingGatewayAndDestinations(t *testing.T) {
	objs := []client.Object{
		mkNamespace("default"), mkNamespace("shop"),
		mkLabeledDeployment("shop", "web", map[string]string{"app": "web"}, 8080),
		mkService("shop", "web", map[string]string{"app": "web"}, 80, 8080),
	}
	snap := &istio.Snapshot{
		Gateways: []istio.Gateway{{Namespace: "default", Name: "lonely",
			Selector: map[string]string{"istio": "nothing"},
			Servers:  []istio.Server{{Port: 80, Protocol: "HTTP", Hosts: []string{"*"}}}}},
		VirtualServices: []istio.VirtualService{{
			Namespace: "shop", Name: "web",
			Hosts:    []string{"web.example.com"},
			Gateways: []string{"missing-gw", "mesh"},
			Routes: []istio.Route{
				{Protocol: "http", Match: "/", Host: "web", Port: 80},
				{Protocol: "http", Match: "/a", Host: "ghost", Port: 80},
				{Protocol: "http", Match: "/b", Host: "web", Port: 9999},
				{Protocol: "http", Match: "/c", Host: "web.shop", Port: 80},
				{Protocol: "http", Match: "/d", Host: "api.example.com"},
			},
		}},
	}
	g := buildWithIstio(t, snap, objs...)
	vs := g.findNode("vs:shop/web")
	want := []string{
		ReasonVSGatewayNotFound, ReasonVSDestinationNotFound, ReasonVSDestinationPortNotFound,
		ReasonVSHostNotFullyQualified, ReasonVSExternalDestination,
	}
	got := warningReasons(vs)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("VS warnings = %v, want %v", got, want)
	}
	if !strings.Contains(vs.Warnings[0].Message, "shop/missing-gw") {
		t.Errorf("bare gateway ref must resolve in the VS namespace: %q", vs.Warnings[0].Message)
	}
	// No ghost nodes for missing gateway/service; one routes edge to web
	// with both ports, flagged by the bad port / partial host.
	if g.findNode("svc:shop/ghost") != nil || g.findNode("gw:shop/missing-gw") != nil {
		t.Error("missing references must not create nodes")
	}
	e := g.findEdge("vs:shop/web", "svc:shop/web")
	if e == nil || e.Label != "routes → 80, 9999" || e.OK == nil || *e.OK {
		t.Errorf("routes edge = %+v", e)
	}
	// VS without a resolvable gateway is its own chain root (D-032).
	if !g.hasEdge("ns:shop", "vs:shop/web") {
		t.Error("unbound VS must be anchored to its namespace")
	}
	if g.hasEdge("ns:shop", "svc:shop/web") {
		t.Error("routed service must not be anchored")
	}
	// Gateway whose selector matches nothing.
	gw := g.findNode("gw:default/lonely")
	if r := warningReasons(gw); len(r) != 1 || r[0] != ReasonGatewayNoWorkload {
		t.Errorf("gateway warnings = %v", r)
	}
	if !g.hasEdge("ns:default", "gw:default/lonely") {
		t.Error("unserved gateway is a chain root and must be anchored")
	}
}

// A standalone pod can implement a gateway too.
func TestIstioGatewayServedByStandalonePod(t *testing.T) {
	objs := []client.Object{
		mkNamespace("default"),
		mkStandalonePod("default", "gw-pod", map[string]string{"istio": "ingressgateway"}, nil),
	}
	snap := &istio.Snapshot{Gateways: []istio.Gateway{{Namespace: "default", Name: "g",
		Selector: map[string]string{"istio": "ingressgateway"}}}}
	g := buildWithIstio(t, snap, objs...)
	if !g.hasEdge("pod:default/gw-pod", "gw:default/g") {
		t.Error("pod → gateway serves edge missing")
	}
}

// Nil snapshot (integration disabled/inactive) = graph identical to Build.
func TestIstioNilSnapshotUnaffected(t *testing.T) {
	objs, _ := liveLikeCluster()
	a, err := Build(context.Background(), fakeClient(objs...))
	if err != nil {
		t.Fatal(err)
	}
	b := buildWithIstio(t, nil, objs...)
	ha, _ := graphHash(a)
	hb, _ := graphHash(b)
	if ha != hb {
		t.Fatal("a nil Istio snapshot must not change the graph")
	}
	for _, n := range b.Nodes {
		if n.Kind == "gateway" || n.Kind == "virtualservice" {
			t.Fatalf("unexpected istio node %s", n.ID)
		}
	}
}

// Ports: the nginx-auth 8082 case gets an explained reason/hint and a node
// warning; named and no-backend cases are explained too.
func TestServicePortWarnings(t *testing.T) {
	auth := mkLabeledDeployment("default", "nginx-auth", map[string]string{"app": "nginx-auth"}, 8080, 8081)
	svc := mkService("default", "nginx-auth", map[string]string{"app": "nginx-auth"}, 8082, 8082)
	svc.Spec.Ports = append(svc.Spec.Ports,
		corev1.ServicePort{Name: "ok", Port: 80, TargetPort: intstr.FromInt(8080)},
		corev1.ServicePort{Name: "named", Port: 81, TargetPort: intstr.FromString("web")})
	orphan := mkService("default", "orphan", map[string]string{"app": "nobody"}, 80, 80)
	manual := mkService("default", "manual", nil, 80, 80)
	g := buildWithIstio(t, nil, mkNamespace("default"), auth, svc, orphan, manual)

	n := g.findNode("svc:default/nginx-auth")
	p := n.Ports[0]
	if p.Matched || p.Reason != checker.ReasonTargetPortNotDeclared {
		t.Fatalf("8082 port = %+v", p)
	}
	wantMsg := "targetPort 8082 is not declared as a containerPort by any selected workload (nginx-auth declares 8080, 8081)"
	if !strings.HasPrefix(p.Hint, wantMsg) || !strings.Contains(p.Hint, "containerPort is informational") {
		t.Errorf("8082 hint = %q", p.Hint)
	}
	if !n.Ports[1].Matched || n.Ports[1].Reason != "" || n.Ports[1].Hint != "" {
		t.Errorf("matched port must carry no reason: %+v", n.Ports[1])
	}
	if n.Ports[2].Reason != checker.ReasonNamedPortUnresolved {
		t.Errorf("named port = %+v", n.Ports[2])
	}
	if r := warningReasons(n); len(r) != 2 || r[0] != checker.ReasonTargetPortNotDeclared || r[1] != checker.ReasonNamedPortUnresolved {
		t.Fatalf("node warnings = %v", r)
	}
	if n.Warnings[0].Message != wantMsg {
		t.Errorf("message = %q", n.Warnings[0].Message)
	}

	o := g.findNode("svc:default/orphan")
	if o.Ports[0].Reason != checker.ReasonNoBackend || len(o.Warnings) != 1 || o.Warnings[0].Reason != checker.ReasonNoBackend {
		t.Errorf("orphan = %+v / %+v", o.Ports, o.Warnings)
	}
	m := g.findNode("svc:default/manual")
	if m.Ports[0].Reason != checker.ReasonNoSelector || len(m.Warnings) != 0 {
		t.Errorf("selector-less service: ports=%+v warnings=%+v (no node warning expected)", m.Ports, m.Warnings)
	}
}

// fakeIstio is a controllable IstioSource.
type fakeIstio struct {
	mu   sync.Mutex
	snap *istio.Snapshot
	fns  []func()
}

func (f *fakeIstio) Snapshot() *istio.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeIstio) Subscribe(fn func()) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fns = append(f.fns, fn)
	return func() {}
}

func (f *fakeIstio) set(s *istio.Snapshot) {
	f.mu.Lock()
	f.snap = s
	fns := append([]func(){}, f.fns...)
	f.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// Integration snapshot changes feed the dirty signal: debounced rebuild and
// a version bump only on real change.
func TestMapperRebuildsOnIstioChange(t *testing.T) {
	objs, snap := liveLikeCluster()
	src := &fakeIstio{}
	m := New(fakeClient(objs...), &fakeInformers{})
	m.QuietWindow, m.MaxWait = 20*time.Millisecond, 100*time.Millisecond
	m.Istio = src
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = m.Start(ctx) }()

	waitFor(t, func() bool { return m.Version() == 1 })
	if m.Snapshot().findNode("gw:default/nginx-gateway") != nil {
		t.Fatal("no istio nodes expected before the integration is active")
	}
	src.set(snap)
	waitFor(t, func() bool { return m.Version() == 2 })
	if m.Snapshot().findNode("gw:default/nginx-gateway") == nil {
		t.Fatal("istio nodes expected after snapshot change")
	}
	// Same content again: rebuild happens but the version stays.
	src.set(snap)
	time.Sleep(200 * time.Millisecond)
	if m.Version() != 2 {
		t.Fatalf("unchanged istio snapshot must keep the version, got %d", m.Version())
	}
	src.set(nil)
	waitFor(t, func() bool { return m.Version() == 3 })
	cancel()
	<-done
}

// A Service selects pods by label: a selector key present only in the pod
// template labels (not in the workload matchLabels) still selects it — no
// false noBackend (seen live on istiod / argocd-redis).
func TestServiceSelectsByPodTemplateLabels(t *testing.T) {
	dep := mkLabeledDeployment("istio-system", "istiod", map[string]string{"app": "istiod", "istio": "pilot"}, 15010)
	dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"istio": "pilot"}}
	svc := mkService("istio-system", "istiod", map[string]string{"app": "istiod", "istio": "pilot"}, 15010, 15010)
	g := buildWithIstio(t, nil, mkNamespace("istio-system"), dep, svc)
	if !g.hasEdge("svc:istio-system/istiod", "dep:istio-system/istiod") {
		t.Fatal("service must select the workload via pod-template labels")
	}
	n := g.findNode("svc:istio-system/istiod")
	if len(n.Warnings) != 0 || !n.Ports[0].Matched {
		t.Fatalf("unexpected warnings/ports: %+v %+v", n.Warnings, n.Ports)
	}
}

// Labels added to pods at runtime (not in the template) still select the
// workload (seen live: longhorn-manager pods + admission-webhook service).
func TestServiceSelectsByLivePodLabels(t *testing.T) {
	ds := mkDaemonSet("longhorn-system", "longhorn-manager", 1, 1, 9502)
	pod := mkOwnedPod("longhorn-system", "longhorn-manager-x", "longhorn-manager", "DaemonSet", "longhorn-manager", corev1.PodRunning)
	pod.Labels["longhorn.io/admission-webhook"] = "longhorn-admission-webhook"
	svc := mkService("longhorn-system", "longhorn-admission-webhook",
		map[string]string{"longhorn.io/admission-webhook": "longhorn-admission-webhook"}, 9502, 9502)
	g := buildWithIstio(t, nil, mkNamespace("longhorn-system"), ds, pod, svc)
	if !g.hasEdge("svc:longhorn-system/longhorn-admission-webhook", "ds:longhorn-system/longhorn-manager") {
		t.Fatal("service must select the workload via live pod labels")
	}
	if n := g.findNode("svc:longhorn-system/longhorn-admission-webhook"); len(n.Warnings) != 0 {
		t.Fatalf("no noBackend expected: %+v", n.Warnings)
	}
}
