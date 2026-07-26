package contenoxcli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/services/project"
	"github.com/contenox/beam/internal/services/workspacegrants"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// workspaceTestRoot builds an isolated root command carrying the persistent --db
// flag (as the real rootCmd does) with sub attached, so tests exercise the real
// RunE logic without touching the package-global rootCmd's flag/parent state.
func workspaceTestRoot(sub *cobra.Command) *cobra.Command {
	root := &cobra.Command{Use: "contenox", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("db", "", "SQLite database path")
	root.AddCommand(sub)
	return root
}

func runWorkspace(t *testing.T, dbPath string, args ...string) (string, error) {
	t.Helper()
	sub := &cobra.Command{Use: "workspace"}
	addCmd := &cobra.Command{Use: "add", Args: cobra.ExactArgs(1), RunE: runWorkspaceAdd}
	addCmd.Flags().String("name", "", "friendly project name (mirrors the real flag)")
	sub.AddCommand(addCmd)
	sub.AddCommand(&cobra.Command{Use: "remove", Args: cobra.ExactArgs(1), RunE: runWorkspaceRemove})
	sub.AddCommand(&cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: runWorkspaceList})
	root := workspaceTestRoot(sub)
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"--db", dbPath, "workspace"}, args...))
	err := root.Execute()
	return buf.String(), err
}

func storeAt(t *testing.T, dbPath string) (context.Context, runtimetypes.Store, func()) {
	t.Helper()
	ctx := context.Background()
	db, err := OpenDBAt(ctx, dbPath)
	require.NoError(t, err)
	return ctx, runtimetypes.New(db.WithoutTransaction()), func() { _ = db.Close() }
}

func TestUnit_workspaceIsReservedSubcommand(t *testing.T) {
	require.True(t, reservedSubcommands["workspace"], `"workspace" must be reserved so it dispatches as a subcommand`)
	require.True(t, firstNonFlagIsReserved([]string{"workspace", "add", "/x"}))
}

func TestUnit_WorkspaceCLI_AddListRemoveRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "workspace-cli.db")
	grant := t.TempDir()

	out, err := runWorkspace(t, dbPath, "add", grant)
	require.NoError(t, err)
	require.Contains(t, out, filepath.Clean(grant), "add echoes the granted root")

	// The grant is durable in the shared config DB.
	ctx, store, done := storeAt(t, dbPath)
	require.Equal(t, []string{filepath.Clean(grant)}, workspacegrants.ReadGrants(ctx, store))
	done()

	out, err = runWorkspace(t, dbPath, "list")
	require.NoError(t, err)
	require.Contains(t, out, filepath.Clean(grant))

	out, err = runWorkspace(t, dbPath, "remove", grant)
	require.NoError(t, err)
	require.Contains(t, out, "no workspace-root grants configured")

	ctx, store, done = storeAt(t, dbPath)
	require.Empty(t, workspacegrants.ReadGrants(ctx, store))
	done()
}

// TestUnit_WorkspaceCLI_AddStampsProjectMarker verifies CLI/serve parity for the
// project registry: `workspace add --name` stamps the same .contenox marker the
// serve-side REST mutator writes, `list` shows the friendly name, re-adding with
// a new --name renames, and a bad name is refused before anything persists.
func TestUnit_WorkspaceCLI_AddStampsProjectMarker(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "workspace-cli.db")
	grant := t.TempDir()

	out, err := runWorkspace(t, dbPath, "add", grant, "--name", "acme-api")
	require.NoError(t, err)
	require.Contains(t, out, "(acme-api)", "the add echo shows the project name")

	m, ok := project.ReadFromProjectRoot(grant)
	require.True(t, ok, "the grant carries a project marker")
	require.NotEmpty(t, m.ID)
	require.Equal(t, "acme-api", m.Name)

	out, err = runWorkspace(t, dbPath, "list")
	require.NoError(t, err)
	require.Contains(t, out, "(acme-api)", "list shows the friendly name next to the path")

	// Re-adding under a new name renames the project; the ID stays stable.
	out, err = runWorkspace(t, dbPath, "add", grant, "--name", "acme-api-v2")
	require.NoError(t, err)
	require.Contains(t, out, "(acme-api-v2)")
	renamed, ok := project.ReadFromProjectRoot(grant)
	require.True(t, ok)
	require.Equal(t, m.ID, renamed.ID, "renaming never rewrites the workspace id")
	require.Equal(t, "acme-api-v2", renamed.Name)

	// A control-character name is refused BEFORE the grant persists.
	other := t.TempDir()
	_, err = runWorkspace(t, dbPath, "add", other, "--name", "bad\nname")
	require.Error(t, err)
	require.ErrorIs(t, err, workspacegrants.ErrInvalidGrant)
	ctx, store, done := storeAt(t, dbPath)
	require.NotContains(t, workspacegrants.ReadGrants(ctx, store), filepath.Clean(other),
		"a refused name must not leave a grant behind")
	done()
}

func TestUnit_WorkspaceCLI_AddNonexistentIsError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "workspace-cli.db")
	out, err := runWorkspace(t, dbPath, "add", filepath.Join(t.TempDir(), "does-not-exist"))
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "does not exist") || strings.Contains(out, "does not exist"),
		"a non-existent grant must be refused with a teaching error")
}

func TestUnit_WorkspaceCLI_ListEmpty(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "workspace-cli.db")
	out, err := runWorkspace(t, dbPath, "list")
	require.NoError(t, err)
	require.Contains(t, out, "no workspace-root grants configured")
}
