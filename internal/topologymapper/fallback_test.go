package topologymapper

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// partialInformers fails GetInformer for EndpointSlices (e.g. a type whose
// informer cannot be obtained) and delegates everything else.
type partialInformers struct{ fakeInformers }

func (p *partialInformers) GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error) {
	if _, ok := obj.(*discoveryv1.EndpointSlice); ok {
		return nil, errors.New("no informer for EndpointSlice")
	}
	return p.fakeInformers.GetInformer(ctx, obj, opts...)
}

// QA: when one informer cannot be registered, the mapper must fall back to
// periodic rebuild checks (every MaxWait), keep the version stable while the
// content is unchanged, and still remove the handlers it did register.
func TestStartPartialRegistrationFallsBackToPeriodic(t *testing.T) {
	inf := &partialInformers{}
	m := New(fakeClient(mkNamespace("default")), inf)
	m.QuietWindow, m.MaxWait = 20*time.Millisecond, 100*time.Millisecond
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

	waitFor(t, func() bool { return m.Version() == 1 })
	// Fallback ticker: several periodic rebuild checks happen without events.
	waitFor(t, func() bool { return builds.Load() >= 3 })
	if m.Version() != 1 {
		t.Fatalf("periodic rebuilds of an unchanged cluster must keep version 1, got %d", m.Version())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
	inf.mu.Lock()
	defer inf.mu.Unlock()
	if want := len(watchedTypes()) - 1; len(inf.handlers) != want || inf.removed != want {
		t.Fatalf("registered=%d removed=%d, want %d each", len(inf.handlers), inf.removed, want)
	}
}
