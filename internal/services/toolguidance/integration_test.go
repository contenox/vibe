package toolguidance_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/contenox/beam/internal/services/toolguidance"
)

// TestUnit_ToolGuidance_RealLocalFS_RepeatMarker pins that the decorator appends the repeat marker without altering the real local_fs listing.
func TestUnit_ToolGuidance_RealLocalFS_RepeatMarker(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"alpha.go", "beta.go", "gamma.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	inner := localtools.NewLocalFSTools(dir, nil)
	wrapped := toolguidance.Wrap(inner)

	ctx := toolguidance.WithSession(context.Background(), "integration-session")
	call := &taskengine.ToolsCall{Name: "local_fs", ToolName: "list_dir"}
	list := func() string {
		res, dt, err := wrapped.Exec(ctx, time.Now(), map[string]any{"path": "."}, false, call)
		if err != nil {
			t.Fatalf("list_dir exec: %v", err)
		}
		if dt != taskengine.DataTypeString {
			t.Fatalf("expected string result from list_dir, got %v", dt)
		}
		return res.(string)
	}

	first := list()
	second := list()
	third := list()

	if strings.Contains(first, "[harness]") || strings.Contains(second, "[harness]") {
		t.Fatalf("marker fired too early:\n1: %q\n2: %q", first, second)
	}
	if !strings.Contains(third, "[harness] 3rd identical list_dir call this session.") {
		t.Fatalf("third call missing repeat marker, got:\n%q", third)
	}
	for _, name := range []string{"alpha.go", "beta.go", "gamma.md"} {
		if !strings.Contains(third, name) {
			t.Fatalf("guidance clobbered the tool's own output; %q missing from:\n%q", name, third)
		}
	}
	if idx := strings.Index(third, "\n[harness] "); idx <= 0 {
		t.Fatalf("marker must be appended on a new line after the listing, got:\n%q", third)
	}
}
