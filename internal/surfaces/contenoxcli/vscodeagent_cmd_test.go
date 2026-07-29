package contenoxcli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestUnit_VSCodeAgentDataDirSelectsLocalDB(t *testing.T) {
	cmd := testVSCodeAgentCommand(t)
	dataDir := filepath.Join(t.TempDir(), "state")
	require.NoError(t, cmd.Root().PersistentFlags().Set("data-dir", dataDir))

	contenoxDir, err := resolveVSCodeAgentContenoxDir(cmd)
	require.NoError(t, err)
	require.Equal(t, dataDir, contenoxDir)
	info, err := os.Stat(contenoxDir)
	require.NoError(t, err)
	require.True(t, info.IsDir())

	dbPath, err := resolveVSCodeAgentDBPath(cmd, contenoxDir)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dataDir, "local.db"), dbPath)
}

func TestUnit_VSCodeAgentDBFlagOverridesDataDir(t *testing.T) {
	cmd := testVSCodeAgentCommand(t)
	dataDir := filepath.Join(t.TempDir(), "state")
	dbPath := filepath.Join(t.TempDir(), "custom.db")
	require.NoError(t, cmd.Root().PersistentFlags().Set("data-dir", dataDir))
	require.NoError(t, cmd.Root().PersistentFlags().Set("db", dbPath))

	contenoxDir, err := resolveVSCodeAgentContenoxDir(cmd)
	require.NoError(t, err)
	got, err := resolveVSCodeAgentDBPath(cmd, contenoxDir)
	require.NoError(t, err)
	require.Equal(t, dbPath, got)
}

func TestUnit_VSCodeAgentSeedsChatFIMAndCompactChains(t *testing.T) {
	contenoxDir := t.TempDir()

	require.NoError(t, seedVSCodeAgentChainsIfMissing(contenoxDir))

	for _, name := range []string{"default-acp-chain.json", "default-fim-chain.json", "chain-compact.json"} {
		data, err := os.ReadFile(filepath.Join(contenoxDir, name))
		require.NoError(t, err, name)
		require.NotEmpty(t, data, name)
	}
}

func testVSCodeAgentCommand(t *testing.T) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "contenox"}
	root.PersistentFlags().String("db", "", "SQLite database path")
	root.PersistentFlags().String("data-dir", "", "Override the .contenox data directory path")
	child := &cobra.Command{Use: "vscode-agent"}
	root.AddCommand(child)
	return child
}
