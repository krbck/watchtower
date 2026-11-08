package ui

import (
	"os/exec"
	"testing"
)

// TestGraphKit runs scripts/check-ui-graph.mjs (chainOf cycle safety,
// hubLayout overlap-freeness, grid fallback) when node exists.
func TestGraphKit(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command("node", "../scripts/check-ui-graph.mjs", "graphkit.js").CombinedOutput()
	if err != nil {
		t.Fatalf("graphkit check failed: %v\n%s", err, out)
	}
}
