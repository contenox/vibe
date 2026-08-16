package localtools_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/localtools"
)

// Declarative `tools` chain tasks carry their arguments on the ToolsCall;
// local_fs must honor them when the chain input isn't an args map (parity
// with local_shell).
func TestUnit_LocalFSTools_FallsBackToDeclaredArgs(t *testing.T) {
	tempDir := t.TempDir()
	h := localtools.NewLocalFSToolsForTest(tempDir, nil)

	toolsCall := &taskengine.ToolsCall{
		ToolName: "write_file",
		Args:     map[string]string{"path": "declared.txt", "content": "from declared args"},
	}
	input := taskengine.ChatHistory{Messages: []taskengine.Message{{Role: "user", Content: "go"}}}

	res, dataType, err := h.Exec(context.Background(), time.Now(), input, false, toolsCall)
	if err != nil {
		t.Fatalf("expected declared-args fallback, got error: %v", err)
	}
	if dataType != taskengine.DataTypeJSON {
		t.Fatalf("unexpected data type: %v", dataType)
	}
	got, err := os.ReadFile(filepath.Join(tempDir, "declared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from declared args" {
		t.Fatalf("unexpected file content: %q", got)
	}
	_ = res
}

func TestUnit_LocalFSTools_NoInputNoArgsStillErrors(t *testing.T) {
	h := localtools.NewLocalFSToolsForTest(t.TempDir(), nil)
	_, _, err := h.Exec(context.Background(), time.Now(), "not-a-map", false, &taskengine.ToolsCall{ToolName: "write_file"})
	if err == nil {
		t.Fatal("expected error when input is not a map and no declared args exist")
	}
}
