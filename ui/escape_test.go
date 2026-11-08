package ui

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestEscaperNotDecoded guards app.js esc() against the regression found
// in qa D-038a F1: its entity map had been decoded into a no-op (every
// character mapped to itself), so API data reached innerHTML unescaped.
// The escaper must build numeric entities by code, and no literal
// self-mapping such as '<': '<' may exist.
func TestEscaperNotDecoded(t *testing.T) {
	b, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	m := regexp.MustCompile(`(?s)function esc\(s\) \{.*?\n\}`).FindString(src)
	if m == "" {
		t.Fatal("esc() not found in app.js")
	}
	if !strings.Contains(m, "charCodeAt(0)") || !strings.Contains(m, "'&#'") {
		t.Fatalf("esc() must build numeric entities by code:\n%s", m)
	}
	for _, ch := range []string{"&", "<", ">", `"`, "'", "`"} {
		if !strings.Contains(m, ch) {
			t.Errorf("esc() character class misses %q:\n%s", ch, m)
		}
	}
	if regexp.MustCompile(`'([<>&"])'\s*:\s*'([<>&"])'`).MatchString(src) {
		t.Error("app.js contains a decoded self-mapping entity map (e.g. '<': '<')")
	}
}

// TestEscaperBehaviour runs scripts/check-ui-escape.mjs when node exists.
func TestEscaperBehaviour(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command("node", "../scripts/check-ui-escape.mjs", "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("escape check failed: %v\n%s", err, out)
	}
}
