package localhooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/runtime/taskengine"
)

func TestLocalFSHook(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "contenox-fs-test-*")
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
		} else if !strings.Contains(err.Error(), "escapes allowed directory") {
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

	t.Run("maxReadBytesRejectsLargeFile", func(t *testing.T) {
		bigPath := filepath.Join(tempDir, "big.bin")
		f, err := os.Create(bigPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(make([]byte, 2*1024*1024)); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()

		args := map[string]any{"path": "big.bin"}
		hookCall := &taskengine.HookCall{ToolName: "read_file"}
		_, _, err = h.Exec(ctx, now, args, false, hookCall)
		if err == nil {
			t.Fatal("expected error for file over default max read size")
		}
		if !strings.Contains(err.Error(), "max") {
			t.Fatalf("expected max size hint: %v", err)
		}
	})

	t.Run("maxReadBytesUnlimited", func(t *testing.T) {
		ctxUnlimited := taskengine.WithHookArgs(ctx, localFSHookName, map[string]string{
			"_max_read_bytes":   "-1",
			"_max_output_bytes": "-1",
		})
		args := map[string]any{"path": "big.bin"}
		hookCall := &taskengine.HookCall{ToolName: "read_file"}
		_, _, err := h.Exec(ctxUnlimited, now, args, false, hookCall)
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("maxOutputBytesRejectsOversizedResult", func(t *testing.T) {
		ctxSmallOut := taskengine.WithHookArgs(ctx, localFSHookName, map[string]string{
			"_max_read_bytes":   "-1",
			"_max_output_bytes": "64",
		})
		args := map[string]any{"path": "big.bin"}
		hookCall := &taskengine.HookCall{ToolName: "read_file"}
		_, _, err := h.Exec(ctxSmallOut, now, args, false, hookCall)
		if err == nil {
			t.Fatal("expected error when tool output exceeds _max_output_bytes")
		}
		if !strings.Contains(err.Error(), "read_file output") || !strings.Contains(err.Error(), "max") {
			t.Fatalf("expected output limit hint: %v", err)
		}
	})

	t.Run("maxOutputBytesUnlimited", func(t *testing.T) {
		ctxBoth := taskengine.WithHookArgs(ctx, localFSHookName, map[string]string{
			"_max_read_bytes":   "-1",
			"_max_output_bytes": "-1",
		})
		args := map[string]any{"path": "big.bin"}
		hookCall := &taskengine.HookCall{ToolName: "read_file"}
		_, _, err := h.Exec(ctxBoth, now, args, false, hookCall)
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("deniedPathSubstrings", func(t *testing.T) {
		ctxDeny := taskengine.WithHookArgs(ctx, localFSHookName, map[string]string{
			"_denied_path_substrings": "node_modules,secret",
		})
		args := map[string]any{"path": "pkg/node_modules/foo.txt"}
		_ = os.MkdirAll(filepath.Join(tempDir, "pkg/node_modules"), 0755)
		if err := os.WriteFile(filepath.Join(tempDir, "pkg/node_modules/foo.txt"), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		hookCall := &taskengine.HookCall{ToolName: "read_file"}
		_, _, err := h.Exec(ctxDeny, now, args, false, hookCall)
		if err == nil {
			t.Fatal("expected denied path error")
		}
		if !strings.Contains(err.Error(), "denied") {
			t.Fatalf("expected denied: %v", err)
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

	t.Run("grepLineRange", func(t *testing.T) {
		args := map[string]any{
			"path":        "test.txt",
			"pattern":     "line",
			"start_line":  float64(2),
			"end_line":    float64(2),
		}
		hookCall := &taskengine.HookCall{ToolName: "grep"}
		res, _, err := h.Exec(ctx, now, args, false, hookCall)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.(string), "2: line 2") {
			t.Fatalf("expected line 2 only: %q", res)
		}
	})

	t.Run("grepRegex", func(t *testing.T) {
		args := map[string]any{
			"path":    "test.txt",
			"pattern": `^line \d$`,
			"regex":   true,
		}
		hookCall := &taskengine.HookCall{ToolName: "grep"}
		res, _, err := h.Exec(ctx, now, args, false, hookCall)
		if err != nil {
			t.Fatal(err)
		}
		s := res.(string)
		if !strings.Contains(s, "2: line 2") || strings.Contains(s, "modified") {
			t.Fatalf("unexpected regex grep: %q", s)
		}
	})

	t.Run("grepInvalidRegex", func(t *testing.T) {
		args := map[string]any{
			"path":    "test.txt",
			"pattern": "(",
			"regex":   true,
		}
		hookCall := &taskengine.HookCall{ToolName: "grep"}
		_, _, err := h.Exec(ctx, now, args, false, hookCall)
		if err == nil {
			t.Fatal("expected invalid regex error")
		}
	})

	t.Run("grepMaxMatches", func(t *testing.T) {
		ctxLim := taskengine.WithHookArgs(ctx, localFSHookName, map[string]string{
			"_max_grep_matches": "1",
		})
		args := map[string]any{
			"path":    "test.txt",
			"pattern": "e",
		}
		hookCall := &taskengine.HookCall{ToolName: "grep"}
		_, _, err := h.Exec(ctxLim, now, args, false, hookCall)
		if err == nil {
			t.Fatal("expected max grep matches error")
		}
		if !strings.Contains(err.Error(), "_max_grep_matches") {
			t.Fatalf("expected policy hint: %v", err)
		}
	})

	t.Run("listDirRecursive", func(t *testing.T) {
		_ = os.MkdirAll(filepath.Join(tempDir, "walktree/sub"), 0755)
		if err := os.WriteFile(filepath.Join(tempDir, "walktree/sub/leaf.txt"), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		args := map[string]any{
			"path":        "walktree",
			"recursive":   true,
			"max_depth":   float64(3),
		}
		hookCall := &taskengine.HookCall{ToolName: "list_dir"}
		res, _, err := h.Exec(ctx, now, args, false, hookCall)
		if err != nil {
			t.Fatal(err)
		}
		s := res.(string)
		if !strings.Contains(s, "walktree/sub/") || !strings.Contains(s, "walktree/sub/leaf.txt") {
			t.Fatalf("expected nested paths: %q", s)
		}
	})

	t.Run("listDirMustBeDirectory", func(t *testing.T) {
		args := map[string]any{"path": "test.txt"}
		hookCall := &taskengine.HookCall{ToolName: "list_dir"}
		_, _, err := h.Exec(ctx, now, args, false, hookCall)
		if err == nil || !strings.Contains(err.Error(), "directory") {
			t.Fatalf("expected not-a-directory: %v", err)
		}
	})
}
