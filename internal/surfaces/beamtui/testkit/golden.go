package testkit

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGoldens = flag.Bool("update", false, "rewrite beam golden files with current output")

// Golden compares got against testdata/<name>.golden relative to the
// calling test's package, failing with a line diff. Run the package tests
// with -update to (re)write the file; the diff output is designed so an
// agent can self-correct from `go test` output alone.
func Golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("golden %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("golden %s: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s: %v (run `go test ./... -update` to create it)", name, err)
	}
	if string(want) == got {
		return
	}
	t.Errorf("golden %s mismatch (run with -update after verifying the change is intended):\n%s",
		name, lineDiff(string(want), got))
}

func lineDiff(want, got string) string {
	wl := strings.Split(want, "\n")
	gl := strings.Split(got, "\n")
	var b strings.Builder
	max := len(wl)
	if len(gl) > max {
		max = len(gl)
	}
	for i := 0; i < max; i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w == g {
			continue
		}
		if i < len(wl) {
			b.WriteString("-" + w + "\n")
		}
		if i < len(gl) {
			b.WriteString("+" + g + "\n")
		}
	}
	return b.String()
}
