package trace

import (
	"strings"
	"testing"

	"github.com/krbck/watchtower/internal/integrations/istio"
)

// QA (D-038a): an invalid RE2 in a VirtualService uri regex must not panic
// and must match nothing (Envoy rejects it too).
func TestQAInvalidRegexMatchesNothing(t *testing.T) {
	vs := &istio.VirtualService{Namespace: "p", Name: "t", HTTP: []istio.HTTPRule{
		{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "regex", Value: "(["}}}},
		{Match: []istio.HTTPMatch{{URI: istio.StringMatch{Kind: "prefix", Value: "/"}}}},
	}}
	_, _, idx := selectHTTPRule(vs, &istio.Gateway{}, &istio.Server{Port: 80}, Request{Host: "h", Path: "/", Port: 80, Scheme: "http"})
	if idx != 1 {
		t.Fatalf("invalid regex rule must be skipped, got rule %d", idx)
	}
}

// QA (D-038a): a non-nil but empty Istio snapshot is "active, nothing
// configured" (NotFound), and a mesh-only VirtualService matching the host
// is named in the hostNotClaimed step.
func TestQAEmptySnapshotAndMeshOnlyVS(t *testing.T) {
	f := homelab()
	f.snap = &istio.Snapshot{}
	resp := f.trace(t, "portfolio.example.com", "/")
	if resp.State != StateNotFound {
		t.Fatalf("empty snapshot: state %s", resp.State)
	}
	if r := reasonsOf(resp.Steps); r[ReasonProviderInactive] != "" {
		t.Fatalf("non-nil snapshot must not report providerInactive: %+v", resp.Steps)
	}
	f.snap.VirtualServices = []istio.VirtualService{{Namespace: "production", Name: "m", Hosts: []string{"*.example.com"}}}
	resp = f.trace(t, "portfolio.example.com", "/")
	last := resp.Steps[len(resp.Steps)-1]
	if last.Reason != ReasonHostNotClaimed || !strings.Contains(last.Detail, "production/m") {
		t.Fatalf("mesh-only VS not mentioned: %+v", last)
	}
}
