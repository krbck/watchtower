package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/krbck/watchtower/internal/checker"
	"github.com/krbck/watchtower/internal/trace"
)

// Trace API tuning (D-038): traces are on-demand reads of the informer
// cache; a short cache absorbs UI re-requests and a semaphore bounds the
// work a burst of queries can cause.
const (
	traceCacheTTL     = 10 * time.Second
	traceCacheMax     = 256
	traceConcurrency  = 4
	traceQueueTimeout = 5 * time.Second
	traceRunTimeout   = 15 * time.Second
)

type traceKey struct {
	host, path, scheme string
	port               int32
	version            uint64
}

type traceEntry struct {
	resp *trace.Response
	at   time.Time
}

// tracer holds the trace cache and the concurrency limiter.
type tracer struct {
	once    sync.Once
	sem     chan struct{}
	mu      sync.Mutex
	entries map[traceKey]traceEntry
	now     func() time.Time
}

func (t *tracer) init() {
	t.once.Do(func() {
		t.sem = make(chan struct{}, traceConcurrency)
		t.entries = map[traceKey]traceEntry{}
		if t.now == nil {
			t.now = time.Now
		}
	})
}

func (t *tracer) get(k traceKey) *trace.Response {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[k]
	if !ok || t.now().Sub(e.at) > traceCacheTTL {
		return nil
	}
	return e.resp
}

func (t *tracer) put(k traceKey, r *trace.Response) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if len(t.entries) >= traceCacheMax {
		for key, e := range t.entries {
			if now.Sub(e.at) > traceCacheTTL {
				delete(t.entries, key)
			}
		}
		if len(t.entries) >= traceCacheMax {
			t.entries = map[traceKey]traceEntry{}
		}
	}
	t.entries[k] = traceEntry{resp: r, at: now}
}

// acquire takes a trace slot, waiting at most traceQueueTimeout.
func (t *tracer) acquire(ctx context.Context) bool {
	timer := time.NewTimer(traceQueueTimeout)
	defer timer.Stop()
	select {
	case t.sem <- struct{}{}:
		return true
	case <-ctx.Done():
	case <-timer.C:
	}
	return false
}

func (t *tracer) release() { <-t.sem }

// handleTrace answers GET /api/v1/trace?host=&path=&port=&scheme= (D-038).
func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	if s.Trace == nil {
		httpError(w, http.StatusServiceUnavailable, "trace not enabled")
		return
	}
	q := r.URL.Query()
	req, err := trace.ParseRequest(q.Get("host"), q.Get("path"), q.Get("port"), q.Get("scheme"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, status, err := s.cachedTrace(r.Context(), req)
	if err != nil {
		httpError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// cachedTrace runs one host trace through the shared cache and concurrency
// limiter (D-038); the upstream endpoint reuses it (B-001). On error the
// HTTP status to answer with is returned. Cached responses are shared:
// callers must not modify them.
func (s *Server) cachedTrace(ctx context.Context, req trace.Request) (*trace.Response, int, error) {
	s.tracer.init()
	var version uint64
	var lookup func(string) *checker.TopologyNode
	if s.Mapper != nil {
		g := s.Mapper.Snapshot()
		version = g.Version
		idx := make(map[string]int, len(g.Nodes))
		for i := range g.Nodes {
			idx[g.Nodes[i].ID] = i
		}
		lookup = func(id string) *checker.TopologyNode {
			if i, ok := idx[id]; ok {
				n := g.Nodes[i]
				return &n
			}
			return nil
		}
	}
	key := traceKey{host: req.Host, path: req.Path, scheme: req.Scheme, port: req.Port, version: version}
	if cached := s.tracer.get(key); cached != nil {
		return cached, http.StatusOK, nil
	}
	if !s.tracer.acquire(ctx) {
		return nil, http.StatusServiceUnavailable, errors.New("too many concurrent traces; retry shortly")
	}
	defer s.tracer.release()
	runCtx, cancel := context.WithTimeout(ctx, traceRunTimeout)
	defer cancel()
	resp, err := trace.Trace(runCtx, s.Trace, req, trace.Options{GraphVersion: version, NodeLookup: lookup})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.tracer.put(key, resp)
	return resp, http.StatusOK, nil
}

// handleHosts lists every claimed host for the UI autocomplete (D-038).
func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	if s.Trace == nil {
		httpError(w, http.StatusServiceUnavailable, "trace not enabled")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), traceRunTimeout)
	defer cancel()
	hosts, err := trace.Hosts(ctx, s.Trace)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, hosts)
}
