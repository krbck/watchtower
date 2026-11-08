package topologymapper

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// lateReg is a handler registration whose HasSynced is controllable.
type lateReg struct{ synced *atomic.Bool }

func (r lateReg) HasSynced() bool { return r.synced.Load() }

// unsyncedInformers: the Service informer never syncs until released
// (simulates missing RBAC on one type); every other type syncs immediately.
type unsyncedInformers struct {
	mu        sync.Mutex
	svcSynced atomic.Bool
	opts      [][]cache.InformerGetOption
	removed   int
}

type unsyncedInformer struct {
	cache.Informer
	parent *unsyncedInformers
	svc    bool
}

func (u *unsyncedInformers) GetInformer(_ context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error) {
	u.mu.Lock()
	u.opts = append(u.opts, opts)
	u.mu.Unlock()
	_, isSvc := obj.(*corev1.Service)
	return &unsyncedInformer{parent: u, svc: isSvc}, nil
}

func (i *unsyncedInformer) AddEventHandler(toolscache.ResourceEventHandler) (toolscache.ResourceEventHandlerRegistration, error) {
	if i.svc {
		return lateReg{synced: &i.parent.svcSynced}, nil
	}
	return fakeReg{}, nil
}

func (i *unsyncedInformer) RemoveEventHandler(toolscache.ResourceEventHandlerRegistration) error {
	i.parent.mu.Lock()
	defer i.parent.mu.Unlock()
	i.parent.removed++
	return nil
}

// QA finding: an informer that never syncs (missing RBAC) must not block the
// mapper. After the bounded wait the type is skipped, the graph is built
// without it, and it is included once it finally syncs.
func TestUnsyncedInformerIsSkippedNotBlocking(t *testing.T) {
	inf := &unsyncedInformers{}
	objs := []client.Object{mkNamespace("default"), mkService("default", "web", nil, 80, 80)}
	m := New(fakeClient(objs...), inf)
	m.QuietWindow, m.MaxWait, m.SyncTimeout = 20*time.Millisecond, 100*time.Millisecond, 150*time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = m.Start(ctx) }()

	waitFor(t, func() bool { return m.Version() == 1 })
	g := m.Snapshot()
	if g.findNode("ns:default") == nil {
		t.Fatal("graph must be built from the synced types")
	}
	if g.findNode("svc:default/web") != nil {
		t.Fatal("unsynced Service type must be skipped")
	}
	if !m.skipped()["Service"] {
		t.Fatal("Service must be marked skipped")
	}

	// RBAC fixed: the informer syncs → un-skipped and rebuilt.
	inf.svcSynced.Store(true)
	waitFor(t, func() bool { return m.Snapshot().findNode("svc:default/web") != nil })
	if m.skipped()["Service"] {
		t.Error("Service must be un-skipped after sync")
	}

	cancel()
	<-done

	// GetInformer must have been asked not to block on sync.
	inf.mu.Lock()
	defer inf.mu.Unlock()
	for _, o := range inf.opts {
		if len(o) == 0 {
			t.Fatal("GetInformer must be called with BlockUntilSynced(false)")
		}
		var io cache.InformerGetOptions
		for _, fn := range o {
			fn(&io)
		}
		if io.BlockUntilSynced == nil || *io.BlockUntilSynced {
			t.Fatal("GetInformer must be called with BlockUntilSynced(false)")
		}
	}
}
