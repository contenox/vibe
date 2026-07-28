package contenoxcli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// agentTestRoot builds an isolated root command carrying the persistent --db
// flag (as the real rootCmd does) with sub attached, so tests exercise the real
// RunE logic without touching the package-global rootCmd's flag state.
func agentTestRoot(sub *cobra.Command) *cobra.Command {
	root := &cobra.Command{Use: "contenox", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("db", "", "SQLite database path")
	root.AddCommand(sub)
	return root
}

func openServiceAt(t *testing.T, dbPath string) (context.Context, agentregistryservice.Service, func()) {
	t.Helper()
	ctx := context.Background()
	db, err := OpenDBAt(ctx, dbPath)
	require.NoError(t, err)
	return ctx, agentregistryservice.New(db), func() { _ = db.Close() }
}

// seedChainAgent persists a chain-kind agent (the runtime's only agent kind)
// so the inspect/toggle commands have something to operate on.
func seedChainAgent(t *testing.T, dbPath, name, path string) {
	t.Helper()
	ctx, svc, done := openServiceAt(t, dbPath)
	defer done()
	a := &runtimetypes.Agent{Name: name, Enabled: true}
	require.NoError(t, a.SetChainConfig(runtimetypes.ChainConfig{Path: path}))
	source := runtimetypes.AgentSourceDiscovered
	a.Source = &source
	require.NoError(t, svc.Create(ctx, a))
}

// ─── dispatch / reservation ─────────────────────────────────────────────────

func TestUnit_agentIsReservedSubcommand(t *testing.T) {
	require.True(t, reservedSubcommands["agent"], `"agent" must be reserved so it dispatches as a subcommand`)
	require.True(t, firstNonFlagIsReserved([]string{"agent", "list"}))
	require.True(t, firstNonFlagIsReserved([]string{"--db", "/tmp/x", "agent", "show"}))
}

// ─── list / show ────────────────────────────────────────────────────────────

func TestUnit_AgentList_And_Show(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "agents.db")
	seedChainAgent(t, dbPath, "shown-agent", "/home/user/.contenox/agent-reviewer.json")

	list := &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: agentListCmd.RunE}
	root := agentTestRoot(list)
	var listBuf bytes.Buffer
	root.SetOut(&listBuf)
	root.SetErr(&listBuf)
	root.SetArgs([]string{"--db", dbPath, "list"})
	require.NoError(t, root.Execute())
	require.Contains(t, listBuf.String(), "shown-agent")
	require.Contains(t, listBuf.String(), runtimetypes.AgentKindChain)

	show := &cobra.Command{Use: "show", Args: cobra.ExactArgs(1), RunE: agentShowCmd.RunE}
	rootShow := agentTestRoot(show)
	var showBuf bytes.Buffer
	rootShow.SetOut(&showBuf)
	rootShow.SetErr(&showBuf)
	rootShow.SetArgs([]string{"--db", dbPath, "show", "shown-agent"})
	require.NoError(t, rootShow.Execute())
	out := showBuf.String()
	require.Contains(t, out, "/home/user/.contenox/agent-reviewer.json")
	require.Contains(t, out, "config_json")
}

// ─── remove ─────────────────────────────────────────────────────────────────

func TestUnit_AgentRemove(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "agents.db")
	seedChainAgent(t, dbPath, "gone-agent", "/home/user/.contenox/x.json")

	rm := &cobra.Command{Use: "remove", Args: cobra.ExactArgs(1), RunE: agentRemoveCmd.RunE}
	root := agentTestRoot(rm)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--db", dbPath, "remove", "gone-agent"})
	require.NoError(t, root.Execute())

	ctx, svc, done := openServiceAt(t, dbPath)
	defer done()
	_, err := svc.GetByName(ctx, "gone-agent")
	require.Error(t, err)
}

// ─── enable / disable ───────────────────────────────────────────────────────

func TestUnit_AgentEnableDisable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "agents.db")
	seedChainAgent(t, dbPath, "toggle-agent", "/home/user/.contenox/y.json")

	disable := &cobra.Command{Use: "disable", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return setAgentEnabled(cmd, args[0], false) }}
	root := agentTestRoot(disable)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--db", dbPath, "disable", "toggle-agent"})
	require.NoError(t, root.Execute())

	ctx, svc, done := openServiceAt(t, dbPath)
	got, err := svc.GetByName(ctx, "toggle-agent")
	require.NoError(t, err)
	require.False(t, got.Enabled)
	done()
}

// ─── pure helpers ───────────────────────────────────────────────────────────

func TestUnit_AgentHelpers(t *testing.T) {
	require.Equal(t, "-", derefOr(nil, "-"))
	s := "discovered"
	require.Equal(t, "discovered", derefOr(&s, "-"))
	empty := ""
	require.Equal(t, "fallback", derefOr(&empty, "fallback"))

	require.Equal(t, "enabled", enabledWord(true))
	require.Equal(t, "disabled", enabledWord(false))

	pretty, err := prettyJSON([]byte(`{"a":1}`))
	require.NoError(t, err)
	require.Contains(t, pretty, "\n  \"a\": 1")
	pretty2, err := prettyJSON(nil)
	require.NoError(t, err)
	require.Equal(t, "{}", pretty2)
}
