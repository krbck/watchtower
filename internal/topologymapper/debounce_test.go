package topologymapper

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/krbck/watchtower/internal/checker"
)

// countingMapper returns a mapper whose builder counts calls and emits a
// graph whose content is controlled by *label.
func countingMapper(quiet, maxWait time.Duration) (*Mapper, *atomic.Int64, *atomic.Value) {
	m := New(fakeClient(), nil)
	m.QuietWindow, m.MaxWait = quiet, maxWait
	var builds atomic.Int64
	var label atomic.Value
	label.Store("a")
	m.build = func(context.Context) (*Graph, error) {
		builds.Add(1)
		return &Graph{Nodes: []checker.TopologyNode{{ID: "n", Kind: "namespace", Label: label.Load().(string)}}}, nil
	}
	return m, &builds, &label
}

func runLoop(t *testing.T, m *Mapper) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.loop(ctx, false)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("debounce loop did not stop on context cancel")
		}
	}
}

func TestDebounceCoalescesBurst(t *testing.T) {
	m, builds, _ := countingMapper(50*time.Millisecond, 250*time.Millisecond)
	stop := runLoop(t, m)
	defer stop()

	for i := 0; i < 20; i++ {
		m.markDirty()
	}
	time.Sleep(400 * time.Millisecond)
	if got := builds.Load(); got != 1 {
		t.Fatalf("burst of events must produce exactly one rebuild, got %d", got)
	}
}

func TestDebounceNoEventsNoRebuild(t *testing.T) {
	m, builds, _ := countingMapper(20*time.Millisecond, 60*time.Millisecond)
	stop := runLoop(t, m)
	defer stop()

	time.Sleep(300 * time.Millisecond)
	if got := builds.Load(); got != 0 {
		t.Fatalf("no events must mean no rebuilds, got %d", got)
	}
}

func TestDebounceMaxWaitUnderChurn(t *testing.T) {
	// Events every 10ms never leave a 50ms quiet window; only MaxWait
	// (250ms) forces rebuilds.
	m, builds, _ := countingMapper(50*time.Millisecond, 250*time.Millisecond)
	stop := runLoop(t, m)
	defer stop()

	deadline := time.Now().Add(900 * time.Millisecond)
	for time.Now().Before(deadline) {
		m.markDirty()
		time.Sleep(10 * time.Millisecond)
	}
	got := builds.Load()
	if got < 2 || got > 5 {
		t.Fatalf("continuous churn over ~900ms with MaxWait=250ms: want 2..5 rebuilds, got %d", got)
	}
}

func TestVersionBumpsOnlyOnRealChange(t *testing.T) {
	m, builds, label := countingMapper(time.Second, time.Second)
	sub, cancel := m.Subscribe()
	defer cancel()
	ctx := context.Background()

	if changed, err := m.rebuild(ctx); err != nil || !changed {
		t.Fatalf("first rebuild must change the graph: changed=%v err=%v", changed, err)
	}
	if v := m.Version(); v != 1 {
		t.Fatalf("version after first build = %d, want 1", v)
	}
	if v := <-sub; v != 1 {
		t.Fatalf("subscriber got %d, want 1", v)
	}

	// Identical content: no new version, no notification.
	if changed, _ := m.rebuild(ctx); changed {
		t.Fatal("identical graph must not count as a change")
	}
	if v := m.Version(); v != 1 {
		t.Fatalf("version must stay 1 on identical rebuild, got %d", v)
	}
	select {
	case v := <-sub:
		t.Fatalf("unexpected notification for unchanged graph: %d", v)
	default:
	}

	label.Store("b")
	if changed, _ := m.rebuild(ctx); !changed {
		t.Fatal("changed content must bump the version")
	}
	if v := m.Version(); v != 2 {
		t.Fatalf("version after change = %d, want 2", v)
	}
	if v := <-sub; v != 2 {
		t.Fatalf("subscriber got %d, want 2", v)
	}
	if m.Snapshot().Nodes[0].Label != "b" {
		t.Fatal("snapshot not swapped to the new graph")
	}
	if builds.Load() != 3 {
		t.Fatalf("builds = %d, want 3", builds.Load())
	}
}

func TestSubscribeCoalescesAndCancels(t *testing.T) {
	m, _, label := countingMapper(time.Second, time.Second)
	sub, cancel := m.Subscribe()
	ctx := context.Background()
	for _, l := range []string{"x", "y", "z"} {
		label.Store(l)
		_, _ = m.rebuild(ctx)
	}
	// A slow subscriber only sees the latest version.
	if v := <-sub; v != 3 {
		t.Fatalf("coalesced notification = %d, want 3", v)
	}
	cancel()
	cancel() // idempotent
	label.Store("w")
	_, _ = m.rebuild(ctx)
	select {
	case v := <-sub:
		t.Fatalf("cancelled subscriber must not be notified, got %d", v)
	default:
	}
}

func TestEventHandlerIgnoresResync(t *testing.T) {
	m := New(fakeClient(), nil)
	h := m.eventHandler()
	pod := func(rv string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", ResourceVersion: rv}}
	}
	pending := func() bool {
		select {
		case <-m.dirty:
			return true
		default:
			return false
		}
	}

	h.OnUpdate(pod("7"), pod("7"))
	if pending() {
		t.Error("resync-only update (same ResourceVersion) must not mark dirty")
	}
	h.OnUpdate(pod("7"), pod("8"))
	if !pending() {
		t.Error("real update must mark dirty")
	}
	h.OnAdd(pod("1"), false)
	if !pending() {
		t.Error("add must mark dirty")
	}
	h.OnDelete(toolscache.DeletedFinalStateUnknown{Key: "ns/p", Obj: pod("2")})
	if !pending() {
		t.Error("delete (tombstone) must mark dirty")
	}
	// Many signals coalesce into one pending slot and never block.
	for i := 0; i < 100; i++ {
		h.OnAdd(pod("1"), false)
	}
	if !pending() || pending() {
		t.Error("dirty signals must coalesce into a single pending slot")
	}
}

// fakeInformers records handler registrations per type.
type fakeInformers struct {
	mu       sync.Mutex
	handlers []toolscache.ResourceEventHandler
	removed  int
}

type fakeInformer struct {
	cache.Informer // unused methods panic if called
	parent         *fakeInformers
}

type fakeReg struct{}

func (fakeReg) HasSynced() bool { return true }

func (f *fakeInformers) GetInformer(context.Context, client.Object, ...cache.InformerGetOption) (cache.Informer, error) {
	return &fakeInformer{parent: f}, nil
}

func (i *fakeInformer) AddEventHandler(h toolscache.ResourceEventHandler) (toolscache.ResourceEventHandlerRegistration, error) {
	i.parent.mu.Lock()
	defer i.parent.mu.Unlock()
	i.parent.handlers = append(i.parent.handlers, h)
	return fakeReg{}, nil
}

func (i *fakeInformer) RemoveEventHandler(toolscache.ResourceEventHandlerRegistration) error {
	i.parent.mu.Lock()
	defer i.parent.mu.Unlock()
	i.parent.removed++
	return nil
}

func TestStartEventDrivenEndToEnd(t *testing.T) {
	inf := &fakeInformers{}
	m := New(fakeClient(mkNamespace("default")), inf)
	m.QuietWindow, m.MaxWait = 30*time.Millisecond, 200*time.Millisecond
	var builds atomic.Int64
	real := m.build
	m.build = func(ctx context.Context) (*Graph, error) {
		builds.Add(1)
		return real(ctx)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.Start(ctx)
	}()

	// builds counts before the snapshot swap; wait for the swap too.
	waitFor(t, func() bool { return builds.Load() == 1 && m.Version() == 1 })
	if m.Version() != 1 || m.Snapshot().findNode("ns:default") == nil {
		t.Fatalf("initial graph not built: %+v", m.Snapshot())
	}
	inf.mu.Lock()
	n := len(inf.handlers)
	h := inf.handlers[0]
	inf.mu.Unlock()
	if n != len(watchedTypes()) {
		t.Fatalf("handlers registered = %d, want %d", n, len(watchedTypes()))
	}

	// Quiet period: nothing happens (no blind ticker).
	time.Sleep(250 * time.Millisecond)
	if builds.Load() != 1 {
		t.Fatalf("no events must mean no rebuilds, got %d builds", builds.Load())
	}

	// An event triggers one rebuild; identical content keeps the version.
	h.OnAdd(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "x"}}, false)
	waitFor(t, func() bool { return builds.Load() == 2 })
	if m.Version() != 1 {
		t.Fatalf("unchanged cluster must keep version 1, got %d", m.Version())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
	inf.mu.Lock()
	defer inf.mu.Unlock()
	if inf.removed != len(watchedTypes()) {
		t.Fatalf("handlers removed on shutdown = %d, want %d", inf.removed, len(watchedTypes()))
	}
}

func waitFor(t *testing.T, cond func() bool) {
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
