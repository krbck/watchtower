// Package topologymapper builds a live graph of every resource in the
// cluster (D-015, D-034).
//
// Design principles ("do not drown the process"):
//   - resources are read from informer caches (no API server polling);
//   - rebuilds are event-driven: informer add/update/delete events raise a
//     non-blocking "dirty" signal; a debounce (quiet window + max wait)
//     coalesces bursts into a single rebuild, and no events means no rebuild;
//   - rebuilds happen on a copy and are swapped atomically — readers never
//     observe a half-built graph;
//   - the graph carries a monotonically increasing Version that only moves
//     when the rebuilt graph actually differs; consumers Subscribe to version
//     changes instead of polling.
package topologymapper

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/krbck/watchtower/internal/checker"
	"github.com/krbck/watchtower/internal/integrations/istio"
)

// Graph is the immutable snapshot consumed by the API/UI.
type Graph struct {
	// Version increases by one every time the graph content changes (D-034).
	Version uint64                 `json:"version"`
	Nodes   []checker.TopologyNode `json:"nodes"`
	Edges   []checker.TopologyEdge `json:"edges"`
}

// findNode returns the node with the given ID (nil when absent).
func (g *Graph) findNode(id string) *checker.TopologyNode {
	for i := range g.Nodes {
		if g.Nodes[i].ID == id {
			return &g.Nodes[i]
		}
	}
	return nil
}

// findEdge returns the directed edge between two node IDs (nil when absent).
func (g *Graph) findEdge(from, to string) *checker.TopologyEdge {
	for i := range g.Edges {
		if g.Edges[i].From == from && g.Edges[i].To == to {
			return &g.Edges[i]
		}
	}
	return nil
}

// hasEdge reports whether a directed edge exists between the two node IDs.
func (g *Graph) hasEdge(from, to string) bool { return g.findEdge(from, to) != nil }

// InformerGetter is the narrow slice of controller-runtime's cache the mapper
// needs to attach event handlers (mgr.GetCache() satisfies it).
type InformerGetter interface {
	GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error)
}

// IstioSource is the read side of the optional Istio integration (D-037).
// The mapper never lists Istio types itself (D-036): it reads the
// integration's snapshot and treats its change notifications as dirty
// signals. A nil source (or a nil snapshot) simply means "no Istio".
type IstioSource interface {
	Snapshot() *istio.Snapshot
	Subscribe(fn func()) (cancel func())
}

// Debounce defaults (D-034).
const (
	DefaultQuietWindow = time.Second
	DefaultMaxWait     = 5 * time.Second
	// DefaultSyncTimeout bounds the wait for watched informers to sync. An
	// informer that cannot sync (typically missing RBAC for its type) is
	// skipped by the builder instead of blocking the mapper forever.
	DefaultSyncTimeout = 30 * time.Second
)

// Read-only RBAC for the cluster graph (D-003, D-015, D-035).
// +kubebuilder:rbac:groups="",resources=namespaces;pods;services,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses;ingressclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch

// Mapper is a manager.Runnable that keeps the cluster graph fresh.
type Mapper struct {
	Client client.Client

	// Informers supplies the informers whose events trigger rebuilds. When
	// nil (or when registration fails) the mapper falls back to marking the
	// graph dirty every MaxWait — degraded, but still correct.
	Informers InformerGetter

	// QuietWindow: rebuild once no event arrived for this long.
	QuietWindow time.Duration
	// MaxWait: under continuous churn never delay a rebuild longer than this
	// after the first pending event.
	MaxWait time.Duration
	// SyncTimeout bounds the initial informer sync wait (default 30s).
	SyncTimeout time.Duration

	// Istio is the optional Istio integration (nil = disabled).
	Istio IstioSource

	// skip holds the watched types whose informer has not synced (built as
	// empty lists until they do).
	skipMu sync.Mutex
	skip   map[string]bool

	graph atomic.Pointer[Graph]
	dirty chan struct{}

	// build is the graph builder; overridable in tests.
	build func(ctx context.Context) (*Graph, error)

	mu       sync.Mutex
	version  uint64
	lastHash [sha256.Size]byte
	subs     map[int]chan uint64
	nextSub  int
}

// New creates a Mapper with sane defaults. inf may be nil (tests).
func New(c client.Client, inf InformerGetter) *Mapper {
	m := &Mapper{
		Client:      c,
		Informers:   inf,
		QuietWindow: DefaultQuietWindow,
		MaxWait:     DefaultMaxWait,
		dirty:       make(chan struct{}, 1),
		subs:        map[int]chan uint64{},
	}
	m.build = func(ctx context.Context) (*Graph, error) {
		return BuildWith(ctx, m.Client, BuildOptions{Istio: m.istioSnapshot(), Skip: m.skipped()})
	}
	m.graph.Store(&Graph{})
	return m
}

// istioSnapshot returns the Istio snapshot, or nil when the integration is
// absent or inactive.
func (m *Mapper) istioSnapshot() *istio.Snapshot {
	if m.Istio == nil {
		return nil
	}
	return m.Istio.Snapshot()
}

// skipped returns a copy of the currently skipped (unsynced) types.
func (m *Mapper) skipped() map[string]bool {
	m.skipMu.Lock()
	defer m.skipMu.Unlock()
	if len(m.skip) == 0 {
		return nil
	}
	out := make(map[string]bool, len(m.skip))
	for k := range m.skip {
		out[k] = true
	}
	return out
}

func (m *Mapper) setSkip(name string, on bool) {
	m.skipMu.Lock()
	defer m.skipMu.Unlock()
	if m.skip == nil {
		m.skip = map[string]bool{}
	}
	if on {
		m.skip[name] = true
	} else {
		delete(m.skip, name)
	}
}

// Snapshot returns the current graph (cheap; no locking).
func (m *Mapper) Snapshot() *Graph { return m.graph.Load() }

// Version returns the version of the current snapshot.
func (m *Mapper) Version() uint64 { return m.Snapshot().Version }

// Subscribe returns a channel that receives the new version every time the
// graph changes. The channel is buffered (size 1) and coalescing: a slow
// consumer only ever sees the latest version, and should read Snapshot()
// on wakeup. cancel unregisters the subscription (the channel is not closed).
func (m *Mapper) Subscribe() (<-chan uint64, func()) {
	ch := make(chan uint64, 1)
	m.mu.Lock()
	id := m.nextSub
	m.nextSub++
	m.subs[id] = ch
	m.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			m.mu.Lock()
			delete(m.subs, id)
			m.mu.Unlock()
		})
	}
}

// markDirty raises the non-blocking dirty signal.
func (m *Mapper) markDirty() {
	select {
	case m.dirty <- struct{}{}:
	default: // a signal is already pending; coalesce.
	}
}

// watchedTypes lists every kind the graph is built from; each one gets an
// event handler on its informer.
func watchedTypes() []client.Object {
	return []client.Object{
		&corev1.Namespace{},
		&corev1.Pod{},
		&corev1.Service{},
		&appsv1.Deployment{},
		&appsv1.StatefulSet{},
		&appsv1.DaemonSet{},
		&networkingv1.Ingress{},
		&networkingv1.IngressClass{},
		&discoveryv1.EndpointSlice{},
	}
}

// eventHandler turns informer events into dirty signals. Resync-only updates
// (same ResourceVersion) are ignored.
func (m *Mapper) eventHandler() toolscache.ResourceEventHandler {
	return toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(interface{}) { m.markDirty() },
		UpdateFunc: func(oldObj, newObj interface{}) {
			if sameResourceVersion(oldObj, newObj) {
				return
			}
			m.markDirty()
		},
		DeleteFunc: func(interface{}) { m.markDirty() },
	}
}

func sameResourceVersion(a, b interface{}) bool {
	am, err := meta.Accessor(a)
	if err != nil {
		return false
	}
	bm, err := meta.Accessor(b)
	if err != nil {
		return false
	}
	return am.GetResourceVersion() != "" && am.GetResourceVersion() == bm.GetResourceVersion()
}

type registration struct {
	name string
	inf  cache.Informer
	reg  toolscache.ResourceEventHandlerRegistration
}

// registerHandlers attaches the dirty-signal handler to every watched
// informer. It returns the registrations made and whether all succeeded.
func (m *Mapper) registerHandlers(ctx context.Context) ([]registration, bool) {
	logger := log.FromContext(ctx).WithName("topologymapper")
	regs := make([]registration, 0, len(watchedTypes()))
	ok := true
	h := m.eventHandler()
	for _, obj := range watchedTypes() {
		// Do not block on sync here: with missing RBAC the informer never
		// syncs and GetInformer would hang forever (QA finding). Sync is
		// awaited with a bound in waitSynced.
		inf, err := m.Informers.GetInformer(ctx, obj, cache.BlockUntilSynced(false))
		if err != nil {
			logger.Error(err, "failed to get informer; type left out of the graph", "type", typeName(obj))
			// A cached List of this type would fail (or hang) as well.
			m.setSkip(typeName(obj), true)
			ok = false
			continue
		}
		reg, err := inf.AddEventHandler(h)
		if err != nil {
			logger.Error(err, "failed to add event handler", "type", typeName(obj))
			ok = false
			continue
		}
		regs = append(regs, registration{name: typeName(obj), inf: inf, reg: reg})
	}
	return regs, ok
}

func (m *Mapper) syncTimeout() time.Duration {
	if m.SyncTimeout <= 0 {
		return DefaultSyncTimeout
	}
	return m.SyncTimeout
}

// waitSynced waits (bounded by SyncTimeout) until every registered handler
// has seen its informer's initial list. Types still unsynced afterwards are
// skipped by the builder, and a background watcher un-skips each one (and
// marks the graph dirty) once it finally syncs — e.g. after an RBAC fix.
func (m *Mapper) waitSynced(ctx context.Context, regs []registration) {
	logger := log.FromContext(ctx).WithName("topologymapper")
	deadline := time.Now().Add(m.syncTimeout())
	pending := regs
	for len(pending) > 0 && ctx.Err() == nil && time.Now().Before(deadline) {
		next := pending[:0:0]
		for _, r := range pending {
			if !r.reg.HasSynced() {
				next = append(next, r)
			}
		}
		pending = next
		if len(pending) == 0 {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
	if len(pending) == 0 || ctx.Err() != nil {
		return
	}
	names := make([]string, 0, len(pending))
	for _, r := range pending {
		names = append(names, r.name)
		m.setSkip(r.name, true)
		go func(r registration) {
			if toolscache.WaitForCacheSync(ctx.Done(), r.reg.HasSynced) {
				logger.Info("informer synced late; including it in the graph", "type", r.name)
				m.setSkip(r.name, false)
				m.markDirty()
			}
		}(r)
	}
	logger.Info("WARNING: informers not synced; building the graph without them (missing RBAC? check the ClusterRole)",
		"types", names, "after", m.syncTimeout())
}

func typeName(obj client.Object) string {
	switch obj.(type) {
	case *corev1.Namespace:
		return "Namespace"
	case *corev1.Pod:
		return "Pod"
	case *corev1.Service:
		return "Service"
	case *appsv1.Deployment:
		return "Deployment"
	case *appsv1.StatefulSet:
		return "StatefulSet"
	case *appsv1.DaemonSet:
		return "DaemonSet"
	case *networkingv1.Ingress:
		return "Ingress"
	case *networkingv1.IngressClass:
		return "IngressClass"
	case *discoveryv1.EndpointSlice:
		return "EndpointSlice"
	}
	return "unknown"
}

// Start implements manager.Runnable: it attaches informer event handlers,
// builds the initial graph and then rebuilds only when something changed
// (debounced, D-034). A full rebuild is cheap (cache reads only) and far
// simpler than incremental graph surgery (faz-2 note).
func (m *Mapper) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("topologymapper")

	// Istio snapshot changes feed the same dirty signal (D-037). Subscribe
	// before the initial build so no change can be missed.
	if m.Istio != nil {
		cancel := m.Istio.Subscribe(m.markDirty)
		defer cancel()
	}

	allRegistered := false
	if m.Informers != nil {
		regs, ok := m.registerHandlers(ctx)
		allRegistered = ok
		defer func() {
			for _, r := range regs {
				_ = r.inf.RemoveEventHandler(r.reg)
			}
		}()
		// Bounded: unsynced types are skipped, never block forever.
		m.waitSynced(ctx, regs)
	}
	if !allRegistered {
		logger.Info("event handlers unavailable; falling back to periodic rebuild checks",
			"interval", m.maxWait())
	}

	if m.Informers == nil {
		// No informers to wait on: wait until the client can serve the
		// core lists.
		m.warmup(ctx)
	}
	if ctx.Err() != nil {
		return nil
	}

	if _, err := m.rebuild(ctx); err != nil {
		logger.Error(err, "initial graph build failed")
	} else {
		g := m.Snapshot()
		logger.Info("initial cluster graph built",
			"nodes", len(g.Nodes), "edges", len(g.Edges), "version", g.Version)
	}

	m.loop(ctx, !allRegistered)
	return nil
}

func (m *Mapper) quietWindow() time.Duration {
	if m.QuietWindow <= 0 {
		return DefaultQuietWindow
	}
	return m.QuietWindow
}

func (m *Mapper) maxWait() time.Duration {
	if m.MaxWait <= 0 {
		return DefaultMaxWait
	}
	return m.MaxWait
}

// loop is the debounce state machine. A dirty signal (re)arms a timer for
// the quiet window, clamped so the rebuild happens at most MaxWait after the
// first pending signal. With fallback=true the graph is also marked dirty
// every MaxWait (used only when informer handlers could not be attached).
func (m *Mapper) loop(ctx context.Context, fallback bool) {
	logger := log.FromContext(ctx).WithName("topologymapper")
	quiet, maxWait := m.quietWindow(), m.maxWait()

	var fallbackC <-chan time.Time
	if fallback {
		t := time.NewTicker(maxWait)
		defer t.Stop()
		fallbackC = t.C
	}

	var (
		timer    *time.Timer
		timerC   <-chan time.Time
		deadline time.Time
	)
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	arm := func() {
		now := time.Now()
		if timerC == nil {
			deadline = now.Add(maxWait)
		}
		wait := quiet
		if rem := deadline.Sub(now); rem < wait {
			wait = rem
		}
		if wait < 0 {
			wait = 0
		}
		if timer == nil {
			timer = time.NewTimer(wait)
		} else {
			timer.Stop()
			timer.Reset(wait) // go1.23 timers: no stale fire after Stop/Reset
		}
		timerC = timer.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-fallbackC:
			arm()
		case <-m.dirty:
			arm()
		case <-timerC:
			timerC = nil
			changed, err := m.rebuild(ctx)
			if err != nil {
				logger.Error(err, "graph rebuild failed")
				continue
			}
			if changed {
				g := m.Snapshot()
				logger.V(1).Info("cluster graph rebuilt",
					"nodes", len(g.Nodes), "edges", len(g.Edges), "version", g.Version)
			}
		}
	}
}

// warmup blocks until the cache can list the core resources (or ctx ends).
func (m *Mapper) warmup(ctx context.Context) {
	for {
		var pods corev1.PodList
		if err := m.Client.List(ctx, &pods); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// rebuild builds a fresh graph and swaps it in atomically — but only when its
// content differs from the current snapshot. Returns whether it changed.
func (m *Mapper) rebuild(ctx context.Context) (bool, error) {
	g, err := m.build(ctx)
	if err != nil {
		return false, err
	}
	g.Version = 0
	h, err := graphHash(g)
	if err != nil {
		return false, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.version > 0 && h == m.lastHash {
		return false, nil
	}
	m.version++
	m.lastHash = h
	g.Version = m.version
	m.graph.Store(g)
	for _, ch := range m.subs {
		notifyLatest(ch, m.version)
	}
	return true, nil
}

// notifyLatest delivers v without blocking, replacing a stale pending value.
func notifyLatest(ch chan uint64, v uint64) {
	select {
	case ch <- v:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- v:
	default:
	}
}

// graphHash is a cheap content fingerprint of the serialized graph (the
// builder emits nodes/edges in a deterministic order and json sorts map keys).
func graphHash(g *Graph) ([sha256.Size]byte, error) {
	b, err := json.Marshal(g)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// NeedLeaderElection — the mapper is read-only and idempotent; no lease needed.
func (m *Mapper) NeedLeaderElection() bool { return false }
