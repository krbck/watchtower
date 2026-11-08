package trace

import (
	"context"
	"testing"

	"github.com/krbck/watchtower/internal/integrations/istio"
)

// QA (B-001): RoutesTo must not panic and must return no Istio routes when
// the Istio integration is disabled or its snapshot is not available.
func TestRoutesToWithoutIstioSnapshot(t *testing.T) {
	f := homelab()
	src := f.source()
	cs, ok := src.(CachedSource)
	if !ok {
		t.Skipf("fixture source is %T, not CachedSource", src)
	}
	cs.Istio = func() *istio.Snapshot { return nil }
	routes, err := RoutesTo(context.Background(), cs, "production", "mobile-api")
	if err != nil {
		t.Fatalf("RoutesTo: %v", err)
	}
	for _, r := range routes {
		if r.Provider == ProviderIstio {
			t.Fatalf("istio route returned without a snapshot: %+v", r)
		}
	}
}
