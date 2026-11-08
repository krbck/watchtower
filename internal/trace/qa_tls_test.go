package trace

import (
	"context"
	"strings"
	"testing"

	"github.com/krbck/watchtower/internal/integrations/istio"
)

// QA (D-038a round 2): Istio TLS-mode edge cases not covered by
// TestIstioTLSModes.

func qaTrace443(t *testing.T, f *fixture, proto, mode string, cred bool, tls []istio.TLSRule) Branch {
	t.Helper()
	f.snap.Gateways[0].Servers = append(f.snap.Gateways[0].Servers, istio.Server{
		Port: 443, Protocol: proto, TLSMode: mode, HasCredential: cred, Hosts: []string{"portfolio.example.com"},
	})
	f.snap.VirtualServices[2].TLS = tls
	req, err := ParseRequest("https://portfolio.example.com", "/", "", "")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := Trace(context.Background(), f.source(), req, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Branches[0]
}

func qaStep(b Branch, hop string) Step {
	for _, s := range b.Steps {
		if s.Hop == hop {
			return s
		}
	}
	return Step{}
}

// Protocol TLS with a terminating mode routes via tcp[]: reported unknown,
// never evaluated against http[]; a missing credential still warns.
func TestQATLSTerminatedIsUnknown(t *testing.T) {
	for _, mode := range []string{"", "SIMPLE", "MUTUAL"} {
		b := qaTrace443(t, homelab(), "TLS", mode, false, nil)
		r := qaStep(b, HopRoute)
		if r.Status != StatusUnknown || !strings.Contains(r.Name, "TLS-terminated TCP") {
			t.Errorf("TLS/%q route = %+v, want unknown tcp step", mode, r)
		}
		if contains(b.Highlight.Nodes, "svc:production/portfolio") {
			t.Errorf("TLS/%q must not follow http[]: %v", mode, b.Highlight.Nodes)
		}
		if l := qaStep(b, HopListener); l.Status != StatusWarn || !strings.Contains(l.Detail, "credentialName is not set") {
			t.Errorf("TLS/%q without credential must warn: %+v", mode, l)
		}
	}
}

// PASSTHROUGH with no matching tls[] rule is noRouteMatch, not http[].
func TestQAPassthroughNoTLSRule(t *testing.T) {
	b := qaTrace443(t, homelab(), "HTTPS", "PASSTHROUGH", false,
		[]istio.TLSRule{{SNIHosts: []string{"other.example.com"}, Route: []istio.Destination{{Host: "v1-website", Port: 80}}}})
	r := qaStep(b, HopRoute)
	if r.Status != StatusFail || r.Reason != ReasonNoRouteMatch || contains(b.Highlight.Nodes, "svc:production/portfolio") {
		t.Fatalf("passthrough without matching tls[]: %+v %v", r, b.Highlight.Nodes)
	}
}
