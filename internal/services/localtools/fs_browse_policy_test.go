package localtools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/stretchr/testify/require"
)

func browsePolicyCtx(args map[string]string) context.Context {
	return taskengine.WithToolsArgs(context.Background(), localtools.LocalFSBrowseToolsName, args)
}

// TestUnit_LocalFSBrowseTools_PolicyArgsKeyedByToolsetName asserts the toolset
// reads its own tools_policies block and nothing else: the same keys under
// local_fs must not reach it.
func TestUnit_LocalFSBrowseTools_PolicyArgsKeyedByToolsetName(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("f%d.go", i)), []byte("x"), 0o644))
	}
	h := localtools.NewLocalFSBrowseTools(root, nil)

	capped, _, err := h.Exec(browsePolicyCtx(map[string]string{"_max_find_results": "2"}), time.Now(),
		map[string]any{"pattern": "*.go"}, false, &taskengine.ToolsCall{ToolName: "find_files"})
	require.NoError(t, err)
	var got struct {
		Matches   []string `json:"matches"`
		Truncated bool     `json:"truncated"`
		Note      string   `json:"note"`
	}
	require.NoError(t, json.Unmarshal([]byte(capped.(string)), &got))
	require.Len(t, got.Matches, 2)
	require.True(t, got.Truncated)
	require.Contains(t, got.Note, "capped at 2 results")

	// The same key under the write half's toolset name governs nothing here.
	strayCtx := taskengine.WithToolsArgs(context.Background(), localtools.LocalFSToolsName, map[string]string{"_max_find_results": "2"})
	full, _, err := h.Exec(strayCtx, time.Now(), map[string]any{"pattern": "*.go"}, false,
		&taskengine.ToolsCall{ToolName: "find_files"})
	require.NoError(t, err)
	var ungoverned struct {
		Matches   []string `json:"matches"`
		Truncated bool     `json:"truncated"`
	}
	require.NoError(t, json.Unmarshal([]byte(full.(string)), &ungoverned))
	require.Len(t, ungoverned.Matches, 5)
	require.False(t, ungoverned.Truncated)
}

// TestUnit_LocalFSBrowseTools_PolicyMaxListDepthCapsRequestedDepth asserts
// _max_list_depth clamps the model's max_depth argument rather than the other
// way round.
func TestUnit_LocalFSBrowseTools_PolicyMaxListDepthCapsRequestedDepth(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "one.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "b", "two.txt"), []byte("x"), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	res, _, err := h.Exec(browsePolicyCtx(map[string]string{"_max_list_depth": "2"}), time.Now(),
		map[string]any{"path": ".", "recursive": true, "max_depth": 9}, false,
		&taskengine.ToolsCall{ToolName: "list_dir"})
	require.NoError(t, err)
	out := res.(string)
	require.Contains(t, out, "a/one.txt")
	require.NotContains(t, out, "a/b/two.txt")
}

// TestUnit_LocalFSBrowseTools_PolicyMaxGrepMatchesTruncatesWithNamedKey asserts
// the truncation notice names the policy key that raises the cap, under this
// toolset's own block.
func TestUnit_LocalFSBrowseTools_PolicyMaxGrepMatchesTruncatesWithNamedKey(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte(strings.Repeat("needle\n", 20)), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	res, _, err := h.Exec(browsePolicyCtx(map[string]string{"_max_grep_matches": "3"}), time.Now(),
		map[string]any{"path": "a.txt", "pattern": "needle"}, false, &taskengine.ToolsCall{ToolName: "grep"})
	require.NoError(t, err)
	out := res.(string)
	require.Equal(t, 3, strings.Count(out, ": needle"))
	require.Contains(t, out, "grep truncated")
	require.Contains(t, out, "tools_policies."+localtools.LocalFSBrowseToolsName+"._max_grep_matches")
	require.Contains(t, out, "(recoverable:")
}

// TestUnit_LocalFSBrowseTools_DirectoryGrepHardCapBeatsPolicy asserts the
// directory-search match cap never exceeds the hard cap even when
// _max_grep_matches is set far higher.
func TestUnit_LocalFSBrowseTools_DirectoryGrepHardCapBeatsPolicy(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 150; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("f%03d.txt", i)), []byte("needle\n"), 0o644))
	}

	h := localtools.NewLocalFSBrowseTools(root, nil)
	res, _, err := h.Exec(browsePolicyCtx(map[string]string{"_max_grep_matches": "100000"}), time.Now(),
		map[string]any{"path": ".", "pattern": "needle"}, false, &taskengine.ToolsCall{ToolName: "grep"})
	require.NoError(t, err)
	out := res.(string)
	require.Equal(t, 100, strings.Count(out, ": needle"),
		"directory grep must never return more than the hard cap regardless of policy")
	require.Contains(t, out, "truncated")
	require.Contains(t, out, "100-match")
	require.Contains(t, out, "(recoverable:")
}

// TestUnit_LocalFSBrowseTools_PolicyDeniedPathSubstringsRefuse asserts
// _denied_path_substrings blocks a resolved path and names the key that did it.
func TestUnit_LocalFSBrowseTools_PolicyDeniedPathSubstringsRefuse(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "secrets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "secrets", "key.txt"), []byte("x"), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	_, _, err := h.Exec(browsePolicyCtx(map[string]string{"_denied_path_substrings": "secrets/"}), time.Now(),
		map[string]any{"path": "secrets/key.txt"}, false, &taskengine.ToolsCall{ToolName: "stat_file"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "matches denied substring")
	require.Contains(t, err.Error(), "tools_policies."+localtools.LocalFSBrowseToolsName+"._denied_path_substrings")
}

// TestUnit_LocalFSBrowseTools_PolicyUseGitignoreCanBeDisabled asserts the
// gitignore noise filter is policy-controlled, not hardcoded.
func TestUnit_LocalFSBrowseTools_PolicyUseGitignoreCanBeDisabled(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.tmp\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "scratch.tmp"), []byte("x"), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	res, _, err := h.Exec(browsePolicyCtx(map[string]string{"_use_gitignore": "false"}), time.Now(),
		map[string]any{"pattern": "*.tmp"}, false, &taskengine.ToolsCall{ToolName: "find_files"})
	require.NoError(t, err)
	require.Contains(t, res.(string), "scratch.tmp")
}

// TestUnit_LocalFSBrowseTools_PolicyAllowedDirRescopesTheWorkspace asserts
// _allowed_dir narrows containment, so a path outside the narrowed root is
// refused even though it sits under the configured directory.
func TestUnit_LocalFSBrowseTools_PolicyAllowedDirRescopesTheWorkspace(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "inner"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "outside.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "inner", "kept.txt"), []byte("x"), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	ctx := browsePolicyCtx(map[string]string{"_allowed_dir": filepath.Join(root, "inner")})

	res, _, err := h.Exec(ctx, time.Now(), map[string]any{"path": "."}, false,
		&taskengine.ToolsCall{ToolName: "list_dir"})
	require.NoError(t, err)
	require.Equal(t, "kept.txt", res.(string))

	_, _, err = h.Exec(ctx, time.Now(), map[string]any{"path": "../outside.txt"}, false,
		&taskengine.ToolsCall{ToolName: "stat_file"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "escapes allowed directory")
}

// TestUnit_LocalFSBrowseTools_HITLGateWrapsEveryCall asserts the toolset gains
// approval gating from the existing wrapper rather than any gate of its own: a
// denying policy stops the call before it touches the filesystem.
func TestUnit_LocalFSBrowseTools_HITLGateWrapsEveryCall(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644))
	inner := localtools.NewLocalFSBrowseTools(root, nil)

	denied := localtools.NewHITLWrapper(inner, alwaysDeny, denyPolicy(), nil)
	res, dt, err := denied.Exec(context.Background(), time.Now(), map[string]any{"path": "."}, false,
		&taskengine.ToolsCall{Name: localtools.LocalFSBrowseToolsName, ToolName: "list_dir"})
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeString, dt)
	require.Contains(t, res, "Denied by the active policy")

	allowed := localtools.NewHITLWrapper(inner, alwaysApprove, allowPolicy(), nil)
	res, _, err = allowed.Exec(context.Background(), time.Now(), map[string]any{"path": "."}, false,
		&taskengine.ToolsCall{Name: localtools.LocalFSBrowseToolsName, ToolName: "list_dir"})
	require.NoError(t, err)
	require.Equal(t, "a.txt", res)
}

// TestUnit_LocalFSBrowseTools_HITLApprovalPathReachesTheTool asserts an
// approve-action call still runs once the human says yes, and is dropped when
// they say no.
func TestUnit_LocalFSBrowseTools_HITLApprovalPathReachesTheTool(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644))
	inner := localtools.NewLocalFSBrowseTools(root, nil)
	call := &taskengine.ToolsCall{Name: localtools.LocalFSBrowseToolsName, ToolName: "list_dir"}

	approved := localtools.NewHITLWrapper(inner, alwaysApprove, approvePolicy(), nil)
	res, _, err := approved.Exec(context.Background(), time.Now(), map[string]any{"path": "."}, false, call)
	require.NoError(t, err)
	require.Equal(t, "a.txt", res)

	refused := localtools.NewHITLWrapper(inner, alwaysDeny, approvePolicy(), nil)
	res, _, err = refused.Exec(context.Background(), time.Now(), map[string]any{"path": "."}, false, call)
	require.NoError(t, err)
	require.Equal(t, localtools.DenyMessage, res)
}
