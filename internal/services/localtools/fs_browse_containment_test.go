package localtools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/stretchr/testify/require"
)

// TestUnit_LocalFSBrowseTools_WalkRefusesControlPlane asserts the recursive
// walk contains every path it reaches through the vfs control-plane denylist,
// not just the walk root: a .contenox directory nested under the allowed root
// (where the relay token lives) is never descended, listed, grepped or matched,
// even with every noise filter disabled so only access control can withhold it.
func TestUnit_LocalFSBrowseTools_WalkRefusesControlPlane(t *testing.T) {
	root := t.TempDir()
	const marker = "SHARED_MARKER"
	require.NoError(t, os.WriteFile(filepath.Join(root, "visible.txt"), []byte(marker+"\n"), 0o644))
	cp := filepath.Join(root, ".contenox")
	require.NoError(t, os.MkdirAll(cp, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cp, "relay.token"), []byte(marker+" RELAY_SECRET\n"), 0o600))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	// Disable the noise filters so the only thing that can withhold .contenox is
	// the containment guard, never skip-dir names or gitignore.
	ctx := browsePolicyCtx(map[string]string{"_skip_dir_names": "", "_use_gitignore": "false"})

	call := func(tool string, args map[string]any) (string, error) {
		res, _, err := h.Exec(ctx, time.Now(), args, false, &taskengine.ToolsCall{ToolName: tool})
		if err != nil {
			return "", err
		}
		return res.(string), nil
	}
	listRecursive := func() string {
		out, err := call("list_dir", map[string]any{"path": ".", "recursive": true})
		require.NoError(t, err)
		return out
	}
	grepAll := func() string {
		out, err := call("grep", map[string]any{"path": ".", "pattern": marker})
		require.NoError(t, err)
		return out
	}
	findTokens := func() []string {
		out, err := call("find_files", map[string]any{"pattern": "*.token"})
		require.NoError(t, err)
		var parsed struct {
			Matches []string `json:"matches"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &parsed))
		return parsed.Matches
	}

	// Baseline: with no control plane registered and the noise filters off, the
	// walk really does reach the nested directory — so the guard, not a filter,
	// is what withholds it once one is registered.
	require.NoError(t, vfs.SetControlPlaneDenied())
	require.Contains(t, listRecursive(), "relay.token")
	require.Contains(t, grepAll(), "relay.token")
	require.NotEmpty(t, findTokens())

	// Register the runtime control plane at the nested .contenox directory.
	require.NoError(t, vfs.SetControlPlaneDenied(cp))
	defer vfs.SetControlPlaneDenied()

	list := listRecursive()
	require.Contains(t, list, "visible.txt", "the allowed root is still listed")
	require.NotContains(t, list, "relay.token", "the walk must not descend into the control plane")
	require.NotContains(t, list, ".contenox")

	grep := grepAll()
	require.Contains(t, grep, "visible.txt", "grep still reads the allowed file")
	require.NotContains(t, grep, "relay.token", "grep must not read a file inside the control plane")
	require.NotContains(t, grep, "RELAY_SECRET")

	require.Empty(t, findTokens(), "find_files must not match inside the control plane")

	// The control plane is refused as a walk root too: naming it directly is
	// contained by resolveTarget, not merely skipped mid-walk.
	_, err := call("list_dir", map[string]any{"path": ".contenox"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "control plane")

	// Escape is still refused: a path outside the allowed root is contained.
	_, err = call("list_dir", map[string]any{"path": "../.."})
	require.Error(t, err)
	require.Contains(t, err.Error(), "escapes allowed directory")
}

// TestUnit_LocalFSBrowseTools_SupportsReportsScopedName asserts Supports reports
// the toolset under its native- namespaced name, the one name an allowlist
// addresses it by.
func TestUnit_LocalFSBrowseTools_SupportsReportsScopedName(t *testing.T) {
	h := localtools.NewLocalFSBrowseTools(t.TempDir(), nil)
	supported, err := h.Supports(context.Background())
	require.NoError(t, err)
	require.Equal(t, localtools.LocalFSBrowseToolsName, supported[0])
	// native- is a namespace, so a declared MCP source cannot mint this key.
	require.True(t, strings.HasPrefix(supported[0], "native-"),
		"the registry key dropped the native- namespace; a declared source could collide with it")

	require.Equal(t, []string{supported[0]},
		taskengine.ExportedApplyAllowlist([]string{"*"}, []string{supported[0]}),
		"\"*\" must admit the scoped toolset; the scope is a namespace, not a hidden exclusion")
	require.Empty(t, taskengine.ExportedApplyAllowlist([]string{"*", "!" + supported[0]}, []string{supported[0]}),
		"\"!\"+the toolset name must remove it from under the wildcard")
	require.Equal(t, []string{supported[0]},
		taskengine.ExportedApplyAllowlist([]string{supported[0]}, []string{supported[0]}),
		"a bare name must grant exactly it")
	require.Empty(t, taskengine.ExportedApplyAllowlist(nil, []string{supported[0]}),
		"an empty allowlist grants nothing")
}
