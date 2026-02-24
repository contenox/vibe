package localhooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/vibe/taskengine"
)

func TestLocalFSHook(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "vibe-fs-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	h := NewLocalFSHook(tempDir)
	ctx := context.Background()
	now := time.Now()

	t.Run("writeFile", func(t *testing.T) {
		args := map[string]any{
			"path":    "test.txt",
			"content": "hello world\nline 2\nline 3",
		}
		hookCall := &taskengine.HookCall{ToolName: "write_file"}
		res, dataType, err := h.Exec(ctx, now, args, false, hookCall)
		if err != nil {
			t.Fatal(err)
		}
		if res != "ok" || dataType != taskengine.DataTypeString {
			t.Errorf("unexpected result: %v, %v", res, dataType)
		}
	})

	t.Run("readFile", func(t *testing.T) {
		args := map[string]any{"path": "test.txt"}
		hookCall := &taskengine.HookCall{ToolName: "read_file"}
		res, dataType, err := h.Exec(ctx, now, args, false, hookCall)
		if err != nil {
			t.Fatal(err)
		}
		content := res.(string)
		if !strings.Contains(content, "hello world") || dataType != taskengine.DataTypeString {
			t.Errorf("unexpected content: %q", content)
		}
	})

	t.Run("listDir", func(t *testing.T) {
		args := map[string]any{"path": "."}
		hookCall := &taskengine.HookCall{ToolName: "list_dir"}
		res, dataType, err := h.Exec(ctx, now, args, false, hookCall)
		if err != nil {
			t.Fatal(err)
		}
		files := res.(string)
		if !strings.Contains(files, "test.txt") || dataType != taskengine.DataTypeString {
			t.Errorf("unexpected files: %q", files)
		}
	})

	t.Run("grep", func(t *testing.T) {
		args := map[string]any{
			"path":    "test.txt",
			"pattern": "line 2",
		}
		hookCall := &taskengine.HookCall{ToolName: "grep"}
		res, dataType, err := h.Exec(ctx, now, args, false, hookCall)
		if err != nil {
			t.Fatal(err)
		}
		match := res.(string)
		if !strings.Contains(match, "2: line 2") || dataType != taskengine.DataTypeString {
			t.Errorf("unexpected match: %q", match)
		}
	})

	t.Run("sed", func(t *testing.T) {
		args := map[string]any{
			"path":        "test.txt",
			"pattern":     "line 3",
			"replacement": "modified line 3",
		}
		hookCall := &taskengine.HookCall{ToolName: "sed"}
		res, _, err := h.Exec(ctx, now, args, false, hookCall)
		if err != nil {
			t.Fatal(err)
		}
		if res != "ok" {
			t.Errorf("unexpected result: %v", res)
		}

		// Verify change
		argsRead := map[string]any{"path": "test.txt"}
		readCall := &taskengine.HookCall{ToolName: "read_file"}
		resRead, _, _ := h.Exec(ctx, now, argsRead, false, readCall)
		if !strings.Contains(resRead.(string), "modified line 3") {
			t.Errorf("sed failed to modify content: %q", resRead)
		}
	})

	t.Run("SecurityRestriction", func(t *testing.T) {
		args := map[string]any{"path": "/etc/passwd"}
		hookCall := &taskengine.HookCall{ToolName: "read_file"}
		_, _, err := h.Exec(ctx, now, args, false, hookCall)
		if err == nil {
			t.Error("expected error for path outside allowed dir, got nil")
		} else if !strings.Contains(err.Error(), "not under allowed directory") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("MkdirAllVerification", func(t *testing.T) {
		args := map[string]any{
			"path":    "subdir/another/file.txt",
			"content": "nested content",
		}
		hookCall := &taskengine.HookCall{ToolName: "write_file"}
		_, _, err := h.Exec(ctx, now, args, false, hookCall)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := os.Stat(filepath.Join(tempDir, "subdir/another/file.txt")); os.IsNotExist(err) {
			t.Error("failed to create nested directories and file")
		}
	})

	t.Run("countStats", func(t *testing.T) {
		args := map[string]any{"path": "test.txt"}
		hookCall := &taskengine.HookCall{ToolName: "count_stats"}
		res, dataType, err := h.Exec(ctx, now, args, false, hookCall)
		if err != nil {
			t.Fatal(err)
		}
		stats := res.(string)
		// test.txt has: "hello world\nline 2\nmodified line 3" (modified in sed test)
		// Lines: 3, Words: 6, Bytes: ?
		if !strings.Contains(stats, "Lines: 3") || dataType != taskengine.DataTypeString {
			t.Errorf("unexpected stats: %q", stats)
		}
	})

	t.Run("readFileRange", func(t *testing.T) {
		args := map[string]any{
			"path":       "test.txt",
			"start_line": float64(2),
			"end_line":   float64(2),
		}
		hookCall := &taskengine.HookCall{ToolName: "read_file_range"}
		res, dataType, err := h.Exec(ctx, now, args, false, hookCall)
		if err != nil {
			t.Fatal(err)
		}
		rangeContent := res.(string)
		if rangeContent != "line 2" || dataType != taskengine.DataTypeString {
			t.Errorf("unexpected range content: %q", rangeContent)
		}
	})

	t.Run("statFile", func(t *testing.T) {
		args := map[string]any{"path": "test.txt"}
		hookCall := &taskengine.HookCall{ToolName: "stat_file"}
		res, dataType, err := h.Exec(ctx, now, args, false, hookCall)
		if err != nil {
			t.Fatal(err)
		}
		if dataType != taskengine.DataTypeJSON {
			t.Errorf("unexpected data type: %v", dataType)
		}
		statStr := res.(string)
		if !strings.Contains(statStr, "\"name\":\"test.txt\"") {
			t.Errorf("unexpected stat output: %q", statStr)
		}
	})
}
