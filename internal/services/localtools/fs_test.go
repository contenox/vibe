package localtools_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/localtools"
)

func TestUnit_LocalFSTools_Exec(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "contenox-fs-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	h := localtools.NewLocalFSTools(tempDir, nil)
	ctx := context.Background()
	now := time.Now()

	// testFileContent is the canonical fixture every test.txt subtest starts
	// from. Subtests must not depend on each other's leftovers: `sed` rewrites
	// test.txt in place, so without a per-subtest reseed the assertions below
	// only hold when the whole parent runs top to bottom. Running one subtest
	// alone — which is what an IDE's per-test run button does — would then fail
	// on a file that was never created.
	const testFileContent = "hello world\nline 2\nline 3"

	seedTestFile := func(t *testing.T) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tempDir, "test.txt"), []byte(testFileContent), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// seedBigFile writes the oversized fixture for the read/output size-policy
	// tests. Content is ASCII text, not NUL bytes: an all-zero fixture would be
	// refused as binary before ever reaching the size checks these tests
	// exercise (see TestUnit_LocalFSTools_ReadFile_RefusesBinary).
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

	t.Run("listDir", func(t *testing.T) {
		seedTestFile(t)
		args := map[string]any{"path": "."}
		toolsCall := &taskengine.ToolsCall{ToolName: "list_dir"}
		res, dataType, err := h.Exec(ctx, now, args, false, toolsCall)
		if err != nil {
			t.Fatal(err)
		}
		if dataType != taskengine.DataTypeString {
			t.Errorf("unexpected data type: %v", dataType)
		}
		files := res.(string)
		if files == "." {
			t.Errorf("list_dir output should not literally be just the requested path string %q", files)
		}
		if !strings.Contains(files, "test.txt") {
			t.Errorf("unexpected files: %q", files)
		}
	})

	t.Run("findFiles", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(tempDir, "main.go"), []byte("package main\n"), 0644); err != nil {
			t.Fatal(err)
		}
		_ = os.MkdirAll(filepath.Join(tempDir, "subdir"), 0755)
		if err := os.WriteFile(filepath.Join(tempDir, "subdir", "lib.go"), []byte("package sub\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tempDir, "subdir", "data.txt"), []byte("x\n"), 0644); err != nil {
			t.Fatal(err)
		}

		args := map[string]any{"pattern": "*.go"}
		toolsCall := &taskengine.ToolsCall{ToolName: "find_files"}
		res, dataType, err := h.Exec(ctx, now, args, false, toolsCall)
		if err != nil {
			t.Fatal(err)
		}
		if dataType != taskengine.DataTypeJSON {
			t.Fatalf("unexpected data type: %v", dataType)
		}
		var out struct {
			Matches   []string `json:"matches"`
			Count     int      `json:"count"`
			Truncated bool     `json:"truncated"`
		}
		if err := json.Unmarshal([]byte(res.(string)), &out); err != nil {
			t.Fatalf("invalid find_files JSON output: %v", err)
		}
		if out.Count != len(out.Matches) {
			t.Fatalf("count mismatch: count=%d matches=%d", out.Count, len(out.Matches))
		}
		if len(out.Matches) != 2 {
			t.Fatalf("expected 2 go files, got %d: %v", len(out.Matches), out.Matches)
		}
		if !(contains(out.Matches, "main.go") && contains(out.Matches, "subdir/lib.go")) {
			t.Fatalf("expected go file matches, got %v", out.Matches)
		}
		if out.Truncated {
			t.Fatalf("did not expect truncation for small result set")
		}

		args = map[string]any{"pattern": "subdir/*.go", "path": "."}
		res, _, err = h.Exec(ctx, now, args, false, toolsCall)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(res.(string)), &out); err != nil {
			t.Fatalf("invalid scoped find_files JSON output: %v", err)
		}
		if len(out.Matches) != 1 || out.Matches[0] != "subdir/lib.go" {
			t.Fatalf("expected only subdir/lib.go, got %v", out.Matches)
		}
	})

	t.Run("searchRepoNoLongerAvailable", func(t *testing.T) {
		toolsCall := &taskengine.ToolsCall{ToolName: "search_repo"}
		_, _, err := h.Exec(ctx, now, map[string]any{"pattern": "x"}, false, toolsCall)
		if err == nil {
			t.Fatal("expected search_repo to be unavailable")
		}
		if !strings.Contains(err.Error(), "unknown tool search_repo") {
			t.Fatalf("unexpected dispatch error: %v", err)
		}
	})

	t.Run("grep", func(t *testing.T) {
		seedTestFile(t)
		args := map[string]any{
			"path":    "test.txt",
			"pattern": "line 2",
		}
		toolsCall := &taskengine.ToolsCall{ToolName: "grep"}
		res, dataType, err := h.Exec(ctx, now, args, false, toolsCall)
		if err != nil {
			t.Fatal(err)
		}
		match := res.(string)
		if !strings.Contains(match, "2: line 2") || dataType != taskengine.DataTypeString {
			t.Errorf("unexpected match: %q", match)
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

		// Verify change
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

	t.Run("countStats", func(t *testing.T) {
		seedTestFile(t)
		args := map[string]any{"path": "test.txt"}
		toolsCall := &taskengine.ToolsCall{ToolName: "count_stats"}
		res, dataType, err := h.Exec(ctx, now, args, false, toolsCall)
		if err != nil {
			t.Fatal(err)
		}
		stats := res.(string)
		// test.txt is the seeded fixture: "hello world\nline 2\nline 3".
		// Lines: 3, Words: 6.
		if !strings.Contains(stats, "Lines: 3") || dataType != taskengine.DataTypeString {
			t.Errorf("unexpected stats: %q", stats)
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
		// Rec 4 (tool-hardening.md): read_file no longer hard-errors when its output
		// exceeds _max_output_bytes — it returns a bounded head plus a notice naming
		// the exact next step ("start_line: N"), so nothing is dropped silently.
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
		h2 := localtools.NewLocalFSTools("", nil)
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
		h2 := localtools.NewLocalFSToolsWith("", nil, nil, localtools.LocalFSToolsName, func(context.Context) string {
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

	t.Run("statFile", func(t *testing.T) {
		seedTestFile(t)
		args := map[string]any{"path": "test.txt"}
		toolsCall := &taskengine.ToolsCall{ToolName: "stat_file"}
		res, dataType, err := h.Exec(ctx, now, args, false, toolsCall)
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
		seedTestFile(t)
		args := map[string]any{
			"path":       "test.txt",
			"pattern":    "line",
			"start_line": float64(2),
			"end_line":   float64(2),
		}
		toolsCall := &taskengine.ToolsCall{ToolName: "grep"}
		res, _, err := h.Exec(ctx, now, args, false, toolsCall)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.(string), "2: line 2") {
			t.Fatalf("expected line 2 only: %q", res)
		}
	})

	t.Run("grepRegex", func(t *testing.T) {
		seedTestFile(t)
		args := map[string]any{
			"path":    "test.txt",
			"pattern": `^line \d$`,
			"regex":   true,
		}
		toolsCall := &taskengine.ToolsCall{ToolName: "grep"}
		res, _, err := h.Exec(ctx, now, args, false, toolsCall)
		if err != nil {
			t.Fatal(err)
		}
		s := res.(string)
		if !strings.Contains(s, "2: line 2") || strings.Contains(s, "modified") {
			t.Fatalf("unexpected regex grep: %q", s)
		}
	})

	t.Run("grepInvalidRegex", func(t *testing.T) {
		seedTestFile(t)
		args := map[string]any{
			"path":    "test.txt",
			"pattern": "(",
			"regex":   true,
		}
		toolsCall := &taskengine.ToolsCall{ToolName: "grep"}
		_, _, err := h.Exec(ctx, now, args, false, toolsCall)
		if err == nil {
			t.Fatal("expected invalid regex error")
		}
	})

	t.Run("grepMaxMatches", func(t *testing.T) {
		seedTestFile(t)
		ctxLim := taskengine.WithToolsArgs(ctx, localtools.LocalFSToolsName, map[string]string{
			"_max_grep_matches": "1",
		})
		args := map[string]any{
			"path":    "test.txt",
			"pattern": "e",
		}
		toolsCall := &taskengine.ToolsCall{ToolName: "grep"}
		res, _, err := h.Exec(ctxLim, now, args, false, toolsCall)
		// Like read_file over its output cap, grep over _max_grep_matches
		// truncates rather than erroring: hard-failing threw away every match
		// already found, so the model paid for the search and got back neither
		// the hits nor the knowledge of where they started.
		if err != nil {
			t.Fatalf("grep over the match cap must truncate, not error: %v", err)
		}
		out, ok := res.(string)
		if !ok {
			t.Fatalf("expected truncated string result, got %T", res)
		}
		if !strings.Contains(out, "1: hello world") {
			t.Fatalf("truncated result must still return the matches found: %q", out)
		}
		if !strings.Contains(out, "_max_grep_matches") {
			t.Fatalf("expected policy hint naming the knob to turn: %q", out)
		}
		if !strings.Contains(out, "start_line: 2") {
			t.Fatalf("truncation notice must name the exact resume point: %q", out)
		}
		if !strings.Contains(out, "(recoverable:") {
			t.Fatalf("truncation notice must carry the recoverable severity marker: %q", out)
		}
	})

	t.Run("listDirRecursive", func(t *testing.T) {
		_ = os.MkdirAll(filepath.Join(tempDir, "walktree/sub"), 0755)
		if err := os.WriteFile(filepath.Join(tempDir, "walktree/sub/leaf.txt"), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		args := map[string]any{
			"path":      "walktree",
			"recursive": true,
			"max_depth": float64(3),
		}
		toolsCall := &taskengine.ToolsCall{ToolName: "list_dir"}
		res, _, err := h.Exec(ctx, now, args, false, toolsCall)
		if err != nil {
			t.Fatal(err)
		}
		s := res.(string)
		if !strings.Contains(s, "walktree/sub/") || !strings.Contains(s, "walktree/sub/leaf.txt") {
			t.Fatalf("expected nested paths: %q", s)
		}
	})

	t.Run("listDirMustBeDirectory", func(t *testing.T) {
		seedTestFile(t)
		args := map[string]any{"path": "test.txt"}
		toolsCall := &taskengine.ToolsCall{ToolName: "list_dir"}
		_, _, err := h.Exec(ctx, now, args, false, toolsCall)
		if err == nil || !strings.Contains(err.Error(), "directory") {
			t.Fatalf("expected not-a-directory: %v", err)
		}
	})

	// --- list_dir noise-filtering tests ---

	// TestFailure: before the fix, list_dir(".") on a project root would return
	// .git/ and node_modules/ entries, flooding the model's context window with
	// thousands of irrelevant paths. The default skip set must prevent this.
	t.Run("listDirSkipsNoiseDirsDefault", func(t *testing.T) {
		root := t.TempDir()
		// Simulate the real-project layout that caused the context flood.
		_ = os.MkdirAll(filepath.Join(root, ".git", "objects"), 0755)
		_ = os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0644)
		_ = os.MkdirAll(filepath.Join(root, "node_modules", "react"), 0755)
		_ = os.WriteFile(filepath.Join(root, "node_modules", "react", "index.js"), []byte("//react\n"), 0644)
		_ = os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0644)

		h2 := localtools.NewLocalFSTools(root, nil)
		res, _, err := h2.Exec(ctx, now, map[string]any{"path": "."}, false, &taskengine.ToolsCall{ToolName: "list_dir"})
		if err != nil {
			t.Fatal(err)
		}
		listing := res.(string)
		if strings.Contains(listing, ".git") {
			t.Errorf("default listing must not contain .git, got:\n%s", listing)
		}
		if strings.Contains(listing, "node_modules") {
			t.Errorf("default listing must not contain node_modules, got:\n%s", listing)
		}
		if !strings.Contains(listing, "main.go") {
			t.Errorf("default listing must contain main.go, got:\n%s", listing)
		}
	})

	t.Run("listDirSkipDirNamesCustom", func(t *testing.T) {
		root := t.TempDir()
		_ = os.MkdirAll(filepath.Join(root, "build"), 0755)
		_ = os.MkdirAll(filepath.Join(root, "src"), 0755)
		_ = os.WriteFile(filepath.Join(root, "src", "app.go"), []byte("package main\n"), 0644)

		ctxCustom := taskengine.WithToolsArgs(ctx, localtools.LocalFSToolsName, map[string]string{
			"_skip_dir_names": "build",
		})
		h2 := localtools.NewLocalFSTools(root, nil)
		res, _, err := h2.Exec(ctxCustom, now, map[string]any{"path": "."}, false, &taskengine.ToolsCall{ToolName: "list_dir"})
		if err != nil {
			t.Fatal(err)
		}
		listing := res.(string)
		if strings.Contains(listing, "build") {
			t.Errorf("build/ must be skipped by custom policy, got:\n%s", listing)
		}
		if !strings.Contains(listing, "src/") {
			t.Errorf("src/ must appear when not in skip list, got:\n%s", listing)
		}
	})

	t.Run("listDirSkipDirNamesDisabled", func(t *testing.T) {
		root := t.TempDir()
		_ = os.MkdirAll(filepath.Join(root, ".git"), 0755)
		_ = os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0644)

		// Empty string disables the default filter entirely.
		ctxDisabled := taskengine.WithToolsArgs(ctx, localtools.LocalFSToolsName, map[string]string{
			"_skip_dir_names": "",
		})
		h2 := localtools.NewLocalFSTools(root, nil)
		res, _, err := h2.Exec(ctxDisabled, now, map[string]any{"path": "."}, false, &taskengine.ToolsCall{ToolName: "list_dir"})
		if err != nil {
			t.Fatal(err)
		}
		listing := res.(string)
		if !strings.Contains(listing, ".git/") {
			t.Errorf("disabled skip filter must show .git/, got:\n%s", listing)
		}
	})

	t.Run("listDirExtensionFilter", func(t *testing.T) {
		root := t.TempDir()
		_ = os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0644)
		_ = os.WriteFile(filepath.Join(root, "README.md"), []byte("# readme\n"), 0644)
		_ = os.WriteFile(filepath.Join(root, "go.sum"), []byte("hash\n"), 0644)
		_ = os.WriteFile(filepath.Join(root, "logo.png"), []byte("\x89PNG\r\n"), 0644)

		ctxExt := taskengine.WithToolsArgs(ctx, localtools.LocalFSToolsName, map[string]string{
			"_list_extensions": ".go,.md",
			"_skip_dir_names":  "", // show everything dir-wise; we only filter files
		})
		h2 := localtools.NewLocalFSTools(root, nil)
		res, _, err := h2.Exec(ctxExt, now, map[string]any{"path": "."}, false, &taskengine.ToolsCall{ToolName: "list_dir"})
		if err != nil {
			t.Fatal(err)
		}
		listing := res.(string)
		if !strings.Contains(listing, "main.go") {
			t.Errorf("main.go must appear with .go filter, got:\n%s", listing)
		}
		if !strings.Contains(listing, "README.md") {
			t.Errorf("README.md must appear with .md filter, got:\n%s", listing)
		}
		if strings.Contains(listing, "go.sum") {
			t.Errorf("go.sum must be hidden by extension filter, got:\n%s", listing)
		}
		if strings.Contains(listing, "logo.png") {
			t.Errorf("logo.png must be hidden by extension filter, got:\n%s", listing)
		}
	})

	t.Run("listDirExtensionFilterRecursive", func(t *testing.T) {
		root := t.TempDir()
		_ = os.MkdirAll(filepath.Join(root, "pkg", "sub"), 0755)
		_ = os.WriteFile(filepath.Join(root, "pkg", "sub", "util.go"), []byte("package sub\n"), 0644)
		_ = os.WriteFile(filepath.Join(root, "pkg", "sub", "util_test.go"), []byte("package sub\n"), 0644)
		_ = os.WriteFile(filepath.Join(root, "pkg", "sub", "data.json"), []byte("{}"), 0644)

		ctxExt := taskengine.WithToolsArgs(ctx, localtools.LocalFSToolsName, map[string]string{
			"_list_extensions": ".go",
			"_skip_dir_names":  "",
		})
		h2 := localtools.NewLocalFSTools(root, nil)
		res, _, err := h2.Exec(ctxExt, now, map[string]any{
			"path":      "pkg",
			"recursive": true,
			"max_depth": float64(3),
		}, false, &taskengine.ToolsCall{ToolName: "list_dir"})
		if err != nil {
			t.Fatal(err)
		}
		listing := res.(string)
		if !strings.Contains(listing, "util.go") {
			t.Errorf("util.go must appear in recursive .go filter, got:\n%s", listing)
		}
		if !strings.Contains(listing, "util_test.go") {
			t.Errorf("util_test.go must appear in recursive .go filter, got:\n%s", listing)
		}
		if strings.Contains(listing, "data.json") {
			t.Errorf("data.json must be hidden by .go extension filter, got:\n%s", listing)
		}
	})
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
		"read_file": true, "write_file": true, "list_dir": true, "grep": true,
		"find_files": true,
		"sed":        true, "count_stats": true, "read_file_range": true, "stat_file": true,
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
