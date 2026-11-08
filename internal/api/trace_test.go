package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	"github.com/krbck/watchtower/internal/checker"
	"github.com/krbck/watchtower/internal/integrations/istio"
	"github.com/krbck/watchtower/internal/trace"
)

// countingSource is an empty cluster that counts trace runs (Services is
// listed once per run) and can block to test the limiter.
type countingSource struct {
	runs  atomic.Int64
	block chan struct{}
}

func (c *countingSource) Kube() checker.Kube { return nil }
func (c *countingSource) Namespaces(context.Context) ([]corev1.Namespace, error) {
	return nil, nil
}
func (c *countingSource) Services(context.Context) ([]corev1.Service, error) {
	c.runs.Add(1)
	if c.block != nil {
		<-c.block
	}
	return nil, nil
}
func (c *countingSource) Pods(context.Context) ([]corev1.Pod, error) { return nil, nil }
func (c *countingSource) Ingresses(context.Context) ([]networkingv1.Ingress, error) {
	return nil, nil
}
func (c *countingSource) IngressClasses(context.Context) ([]networkingv1.IngressClass, error) {
	return nil, nil
}
func (c *countingSource) WorkloadIDs(context.Context) (map[string]bool, error) { return nil, nil }
func (c *countingSource) IstioSnapshot() *istio.Snapshot                       { return &istio.Snapshot{} }

func get(t *testing.T, url string) (int, map[string]interface{}) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]interface{}
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

func TestTraceEndpointValidationAndCache(t *testing.T) {
	src := &countingSource{}
	s := &Server{Trace: src}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	for _, q := range []string{"", "?host=", "?host=bad%20host", "?host=a.com&port=0", "?host=a.com&scheme=ftp",
		"?host=a.com&path=%2F%3Cimg%20src%3Dx%3E", "?host=api.example.com%2F%3Cimg%3E", "?host=a.com&path=%2Fa%60b"} {
		if code, _ := get(t, ts.URL+"/api/v1/trace"+q); code != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", q, code)
		}
	}
	code, body := get(t, ts.URL+"/api/v1/trace?host=Nothing.Example.org.&path=/x")
	if code != http.StatusOK || body["state"] != trace.StateNotFound {
		t.Fatalf("status=%d body=%v", code, body)
	}
	req := body["request"].(map[string]interface{})
	if req["host"] != "nothing.example.org" || req["path"] != "/x" || req["port"] != float64(80) {
		t.Errorf("normalized request = %v", req)
	}
	// Same (host, path, port, scheme, graphVersion) → served from cache.
	get(t, ts.URL+"/api/v1/trace?host=nothing.example.org&path=/x")
	if n := src.runs.Load(); n != 1 {
		t.Fatalf("trace ran %d times, want 1 (cached)", n)
	}
	get(t, ts.URL+"/api/v1/trace?host=nothing.example.org&path=/y")
	if n := src.runs.Load(); n != 2 {
		t.Fatalf("different path must run again, runs=%d", n)
	}
}

func TestTraceEndpointDisabled(t *testing.T) {
	ts := httptest.NewServer((&Server{}).routes())
	defer ts.Close()
	if code, _ := get(t, ts.URL+"/api/v1/trace?host=a.com"); code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", code)
	}
	if code, _ := get(t, ts.URL+"/api/v1/hosts"); code != http.StatusServiceUnavailable {
		t.Fatalf("hosts status %d, want 503", code)
	}
}

// At most traceConcurrency traces run at once.
func TestTraceConcurrencyLimit(t *testing.T) {
	src := &countingSource{block: make(chan struct{})}
	s := &Server{Trace: src}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	var wg sync.WaitGroup
	for i := 0; i < traceConcurrency+2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := http.Get(ts.URL + "/api/v1/trace?host=h" + string(rune('a'+i)) + ".com")
			if err == nil {
				res.Body.Close()
			}
		}(i)
	}
	deadline := time.Now().Add(2 * time.Second)
	for src.runs.Load() < traceConcurrency && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := src.runs.Load(); n != traceConcurrency {
		t.Fatalf("running traces = %d, want %d", n, traceConcurrency)
	}
	close(src.block)
	wg.Wait()
	if n := src.runs.Load(); n != traceConcurrency+2 {
		t.Fatalf("queued traces must run after release, runs=%d", n)
	}
}

func TestTracerCacheExpiry(t *testing.T) {
	now := time.Unix(0, 0)
	tr := &tracer{now: func() time.Time { return now }}
	tr.init()
	k := traceKey{host: "a"}
	tr.put(k, &trace.Response{State: trace.StatePassed})
	if tr.get(k) == nil {
		t.Fatal("fresh entry must be served")
	}
	now = now.Add(traceCacheTTL + time.Second)
	if tr.get(k) != nil {
		t.Fatal("expired entry must not be served")
	}
	if tr.get(traceKey{host: "a", version: 2}) != nil {
		t.Fatal("another graph version must miss")
	}
}
