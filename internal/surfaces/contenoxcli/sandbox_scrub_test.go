package contenoxcli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/shellenvservice"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// sandboxScrubTestDB opens an isolated sqlite db (schema includes the kv table
// shellenvservice needs), returning it and its on-disk path, and closes it on
// test cleanup.
func sandboxScrubTestDB(t *testing.T) (libdb.DBManager, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "sandbox-scrub.db")
	db, err := OpenDBAt(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, dbPath
}

// TestUnit_ResolvedSandboxEnv_StripsSecretKeepsToolchain pins the credential-leak
// fix at the one composition every spawn root calls: under the default
// agent-shell posture (deny-secrets, SANDBOX_SHELL_SCRUB unset), a
// credential-shaped variable must not survive while PATH/HOME do.
func TestUnit_ResolvedSandboxEnv_StripsSecretKeepsToolchain(t *testing.T) {
	t.Setenv("TESTSECRET_API_KEY", "leaked-value")
	t.Setenv("SANDBOX_SHELL_SCRUB", "")

	db, _ := sandboxScrubTestDB(t)
	shellScrub, _, err := resolvedSandboxEnv(db, libtracker.NoopTracker{}, nil)
	require.NoError(t, err)
	require.NotNil(t, shellScrub, "the default posture must be active, not off")

	applied := shellScrub([]string{"TESTSECRET_API_KEY=leaked-value", "PATH=/usr/bin", "HOME=/home/x"})
	joined := strings.Join(applied, "\n")
	require.NotContains(t, joined, "TESTSECRET_API_KEY", "the default scrub must strip credential-shaped names")
	require.Contains(t, joined, "PATH=/usr/bin", "PATH must survive the default scrub or every spawned shell breaks")
	require.Contains(t, joined, "HOME=/home/x", `HOME must survive the default scrub (Allow="*" under deny-secrets)`)
}

// TestUnit_SandboxEnvPreview_MatchesAppliedComposition is the truth-in-preview
// regression: `contenox sandbox env` and every real spawn root call the same
// resolvedSandboxEnv, so the preview's printed names must equal what the
// composition keeps.
func TestUnit_SandboxEnvPreview_MatchesAppliedComposition(t *testing.T) {
	t.Setenv("TESTSECRET_API_KEY", "leaked-value")
	t.Setenv("SANDBOX_SHELL_SCRUB", "")

	db, dbPath := sandboxScrubTestDB(t)
	require.NoError(t, shellenvservice.New(db).Set(context.Background(), map[string]string{
		"INJECTED_TOOLCHAIN_VAR": "present",
	}))

	// The applied side: the exact function every spawn root calls.
	shellScrub, _, err := resolvedSandboxEnv(db, libtracker.NoopTracker{}, nil)
	require.NoError(t, err)
	applied := shellScrub([]string{"PATH=/usr/bin", "HOME=/home/x", "TESTSECRET_API_KEY=leaked-value"})
	appliedNames := map[string]bool{}
	for _, kv := range applied {
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			appliedNames[kv[:eq]] = true
		}
	}
	require.True(t, appliedNames["INJECTED_TOOLCHAIN_VAR"], "the applied composition must carry the operator's shellenvservice entry")
	require.False(t, appliedNames["TESTSECRET_API_KEY"], "the applied composition must strip the credential-shaped name")

	// The preview side: the actual `sandbox env` command, run against the same db.
	envCmd := &cobra.Command{Use: "env", Args: cobra.NoArgs, RunE: runSandboxEnv}
	envCmd.Flags().Bool("terminal", false, "")
	root := &cobra.Command{Use: "contenox", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("db", "", "SQLite database path")
	root.AddCommand(envCmd)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--db", dbPath, "env"})
	require.NoError(t, root.Execute())
	preview := out.String()

	require.NotContains(t, preview, "TESTSECRET_API_KEY", "the preview must strip the same credential shapes a real spawn strips")
	require.Contains(t, preview, "INJECTED_TOOLCHAIN_VAR", "the preview must show the operator's shellenvservice entries — the same ones a real spawn gets")

	// Names printed by the preview must equal the applied composition's names
	// evaluated against THIS process's real environment (what the preview
	// actually scrubs — os.Environ(), per runSandboxEnv).
	realApplied, _, err := resolvedSandboxEnv(db, libtracker.NoopTracker{}, nil)
	require.NoError(t, err)
	wantEnv := realApplied(os.Environ())
	var wantNames []string
	for _, kv := range wantEnv {
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			wantNames = append(wantNames, kv[:eq])
		}
	}

	var previewNames []string
	for _, line := range strings.Split(preview, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		previewNames = append(previewNames, line)
	}
	sort.Strings(previewNames)
	sort.Strings(wantNames)
	require.Equal(t, wantNames, previewNames, "the preview's printed names must equal the applied composition's names")
}
