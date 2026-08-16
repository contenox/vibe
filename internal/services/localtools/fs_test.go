package localtools_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/localtools"
)

func TestUnit_LocalFSTools_Exec(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "contenox-fs-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	h := localtools.NewLocalFSToolsForTest(tempDir, nil)
	ctx := context.Background()
	now := time.Now()

	// Subtests share this fixture but sed mutates it in place, so any subtest depending on its content must reseed via seedTestFile.
	const testFileContent = "hello world\nline 2\nline 3"

	seedTestFile := func(t *testing.T) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tempDir, "test.txt"), []byte(testFileContent), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// ASCII content, not NUL bytes, so the fixture isn't refused as binary before the size checks run.
	seedBigFile := func(t *testing.T) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tempDir, "big.bin"), bytes.Repeat([]byte("a"), 2*1024*1024), 0644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("writeFile", func(t *testing.T) {
		args := map[string]any{
			"path":    "test.txt",
			"content": "hello world\nline 2\nline 3",
		}
		toolsCall := &taskengine.ToolsCall{ToolName: "write_file"}
		res, dataType, err := h.Exec(ctx, now, args, false, toolsCall)
		if err != nil {
			t.Fatal(err)
		}
		fw, ok := res.(localtools.FsWriteResult)
		if !ok || dataType != taskengine.DataTypeJSON {
			t.Errorf("unexpected result: %v (%T), %v", res, res, dataType)
		}
		if !fw.Written || fw.NewText != "hello world\nline 2\nline 3" {
			t.Errorf("unexpected FsWriteResult: %+v", fw)
		}
	})

	t.Run("readFile", func(t *testing.T) {
		seedTestFile(t)
		args := map[string]any{"path": "test.txt"}
		toolsCall := &taskengine.ToolsCall{ToolName: "read_file"}
		res, dataType, err := h.Exec(ctx, now, args, false, toolsCall)
		if err != nil {
			t.Fatal(err)
		}
		content := res.(string)
		if !strings.Contains(content, "hello world") || dataType != taskengine.DataTypeString {
			t.Errorf("unexpected content: %q", content)
		}
	})

	t.Run("sed", func(t *testing.T) {
		seedTestFile(t)
		args := map[string]any{
			"path":        "test.txt",
			"pattern":     "line 3",
			"replacement": "modified line 3",
		}
		toolsCall := &taskengine.ToolsCall{ToolName: "sed"}
		res, _, err := h.Exec(ctx, now, args, false, toolsCall)
		if err != nil {
			t.Fatal(err)
		}
		sed, ok := res.(localtools.FsSedResult)
		if !ok || !sed.Written || !sed.Changed || sed.Replacements != 1 {
			t.Errorf("unexpected result: %v", res)
		}

		argsRead := map[string]any{"path": "test.txt"}
		readCall := &taskengine.ToolsCall{ToolName: "read_file"}
		resRead, _, _ := h.Exec(ctx, now, argsRead, false, readCall)
		if !strings.Contains(resRead.(string), "modified line 3") {
			t.Errorf("sed failed to modify content: %q", resRead)
		}
	})

	t.Run("SecurityRestriction", func(t *testing.T) {
		args := map[string]any{"path": "/etc/passwd"}
		toolsCall := &taskengine.ToolsCall{ToolName: "read_file"}
		_, _, err := h.Exec(ctx, now, args, false, toolsCall)
		if err == nil {
			t.Error("expected error for path outside allowed dir, got nil")
		} else if !strings.Contains(err.Error(), "escapes allowed directory") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("UnknownArgsRejected", func(t *testing.T) {
		seedTestFile(t)
		args := map[string]any{"path": "test.txt", "unexpected": true}
		toolsCall := &taskengine.ToolsCall{ToolName: "read_file"}
		_, _, err := h.Exec(ctx, now, args, false, toolsCall)
		if err == nil {
			t.Fatal("expected unknown argument error")
		}
		if !strings.Contains(err.Error(), "unexpected") {
			t.Fatalf("expected error to name unknown argument, got %v", err)
		}
	})

	t.Run("MkdirAllVerification", func(t *testing.T) {
		args := map[string]any{
			"path":    "subdir/another/file.txt",
			"content": "nested content",
		}
		toolsCall := &taskengine.ToolsCall{ToolName: "write_file"}
		_, _, err := h.Exec(ctx, now, args, false, toolsCall)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := os.Stat(filepath.Join(tempDir, "subdir/another/file.txt")); os.IsNotExist(err) {
			t.Error("failed to create nested directories and file")
		}
	})

	t.Run("readFileRange", func(t *testing.T) {
		seedTestFile(t)
		args := map[string]any{
			"path":       "test.txt",
			"start_line": float64(2),
			"end_line":   float64(2),
		}
		toolsCall := &taskengine.ToolsCall{ToolName: "read_file_range"}
		res, dataType, err := h.Exec(ctx, now, args, false, toolsCall)
		if err != nil {
			t.Fatal(err)
		}
		rangeContent := res.(string)
		if rangeContent != "line 2" || dataType != taskengine.DataTypeString {
			t.Errorf("unexpected range content: %q", rangeContent)
		}
	})

	t.Run("maxReadBytesRejectsLargeFile", func(t *testing.T) {
		seedBigFile(t)
		args := map[string]any{"path": "big.bin"}
		toolsCall := &taskengine.ToolsCall{ToolName: "read_file"}
		_, _, err := h.Exec(ctx, now, args, false, toolsCall)
		if err == nil {
			t.Fatal("expected error for file over default max read size")
		}
		if !strings.Contains(err.Error(), "max") {
			t.Fatalf("expected max size hint: %v", err)
		}
	})

	t.Run("maxReadBytesUnlimited", func(t *testing.T) {
		seedBigFile(t)
		ctxUnlimited := taskengine.WithToolsArgs(ctx, localtools.LocalFSToolsName, map[string]string{
			"_max_read_bytes":   "-1",
			"_max_output_bytes": "-1",
		})
		args := map[string]any{"path": "big.bin"}
		toolsCall := &taskengine.ToolsCall{ToolName: "read_file"}
		_, _, err := h.Exec(ctxUnlimited, now, args, false, toolsCall)
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("maxOutputBytesTruncatesRatherThanErrors", func(t *testing.T) {
		// read_file does not hard-error when output exceeds _max_output_bytes:
		// it returns a bounded head plus a notice naming the next step.
		seedBigFile(t)
		ctxSmallOut := taskengine.WithToolsArgs(ctx, localtools.LocalFSToolsName, map[string]string{
			"_max_read_bytes":   "-1",
			"_max_output_bytes": "64",
		})
		args := map[string]any{"path": "big.bin"}
		toolsCall := &taskengine.ToolsCall{ToolName: "read_file"}
		res, _, err := h.Exec(ctxSmallOut, now, args, false, toolsCall)
		if err != nil {
			t.Fatalf("read_file over the output cap must truncate, not error: %v", err)
		}
		out, ok := res.(string)
		if !ok {
			t.Fatalf("expected truncated string result, got %T", res)
		}
		if !strings.Contains(out, "truncated") || !strings.Contains(out, "start_line:") {
			t.Fatalf("truncated result must name the exact next step: %q", out)
		}
		if !strings.Contains(out, "(recoverable:") {
			t.Fatalf("truncation notice must carry the recoverable severity marker: %q", out)
		}
	})

	t.Run("maxOutputBytesUnlimited", func(t *testing.T) {
		seedBigFile(t)
		ctxBoth := taskengine.WithToolsArgs(ctx, localtools.LocalFSToolsName, map[string]string{
			"_max_read_bytes":   "-1",
			"_max_output_bytes": "-1",
		})
		args := map[string]any{"path": "big.bin"}
		toolsCall := &taskengine.ToolsCall{ToolName: "read_file"}
		_, _, err := h.Exec(ctxBoth, now, args, false, toolsCall)
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("deniedPathSubstrings", func(t *testing.T) {
		ctxDeny := taskengine.WithToolsArgs(ctx, localtools.LocalFSToolsName, map[string]string{
			"_denied_path_substrings": "node_modules,secret",
		})
		args := map[string]any{"path": "pkg/node_modules/foo.txt"}
		_ = os.MkdirAll(filepath.Join(tempDir, "pkg/node_modules"), 0755)
		if err := os.WriteFile(filepath.Join(tempDir, "pkg/node_modules/foo.txt"), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		toolsCall := &taskengine.ToolsCall{ToolName: "read_file"}
		_, _, err := h.Exec(ctxDeny, now, args, false, toolsCall)
		if err == nil {
			t.Fatal("expected denied path error")
		}
		if !strings.Contains(err.Error(), "denied") {
			t.Fatalf("expected denied: %v", err)
		}
	})

	t.Run("allowedDirFromPolicy", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "policy.txt"), []byte("policy ok"), 0644); err != nil {
			t.Fatal(err)
		}
		ctxPolicy := taskengine.WithToolsArgs(ctx, localtools.LocalFSToolsName, map[string]string{
			"_allowed_dir": root,
		})
		h2 := localtools.NewLocalFSToolsForTest("", nil)
		res, dataType, err := h2.Exec(ctxPolicy, now, map[string]any{"path": "policy.txt"}, false, &taskengine.ToolsCall{ToolName: "read_file"})
		if err != nil {
			t.Fatal(err)
		}
		if dataType != taskengine.DataTypeString || res.(string) != "policy ok" {
			t.Fatalf("unexpected read result: %v (%v)", res, dataType)
		}
	})

	t.Run("relativeAllowedDirFromPolicyUsesCwdResolver", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "workspace"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "workspace", "policy.txt"), []byte("workspace policy ok"), 0644); err != nil {
			t.Fatal(err)
		}
		ctxPolicy := taskengine.WithToolsArgs(ctx, localtools.LocalFSToolsName, map[string]string{
			"_allowed_dir": "workspace",
		})
		h2 := localtools.NewLocalFSToolsWith("", nil, localtools.TestHostFileIO{}, localtools.LocalFSToolsName, func(context.Context) string {
			return root
		})
		res, dataType, err := h2.Exec(ctxPolicy, now, map[string]any{"path": "policy.txt"}, false, &taskengine.ToolsCall{ToolName: "read_file"})
		if err != nil {
			t.Fatal(err)
		}
		if dataType != taskengine.DataTypeString || res.(string) != "workspace policy ok" {
			t.Fatalf("unexpected read result: %v (%v)", res, dataType)
		}
	})

	// --- list_dir noise-filtering tests ---

	// listDirSkipsNoiseDirsDefault: the default skip set must prevent .git/ and node_modules/ from flooding a listing.
}

func TestUnit_LocalFSTools_InjectedNamePlumbsThrough(t *testing.T) {
	ctx := context.Background()
	h := localtools.NewLocalFSToolsWith(t.TempDir(), nil, nil, "scoped_fs", nil)

	names, err := h.Supports(ctx)
	if err != nil {
		t.Fatalf("Supports: %v", err)
	}
	if len(names) == 0 || names[0] != "scoped_fs" {
		t.Fatalf("Supports must lead with the injected name, got %v", names)
	}

	tools, err := h.GetToolsForToolsByName(ctx, "scoped_fs")
	if err != nil {
		t.Fatalf("GetToolsForToolsByName(scoped_fs): %v", err)
	}
	want := map[string]bool{
		"read_file": true, "write_file": true, "edit_file": true,
		"sed": true, "read_file_range": true,
	}
	if len(tools) != len(want) {
		t.Fatalf("expected %d tools, got %d", len(want), len(tools))
	}
	for _, tl := range tools {
		if !want[tl.Function.Name] {
			t.Fatalf("unexpected tool %q advertised under scoped_fs", tl.Function.Name)
		}
	}

	if _, err := h.GetToolsForToolsByName(ctx, "local_fs"); err == nil {
		t.Fatalf("a renamed instance must not answer to the old name local_fs")
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
