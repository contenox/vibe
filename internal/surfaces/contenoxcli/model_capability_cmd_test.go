package contenoxcli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/models/modelcapability"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func TestUnit_ModelCapabilitySetCmd_PersistsThinkOverride(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "capability-cli.db")
	cmd := testCobraCmd()
	cmd.SetOut(&bytes.Buffer{})
	require.NoError(t, cmd.Root().PersistentFlags().Set("db", dbPath))
	cmd.Flags().String("think", "", "")
	require.NoError(t, cmd.Flags().Set("think", "true"))

	require.NoError(t, modelCapabilitySetCmd.RunE(cmd, []string{"OpenAI", "gpt-5-mini"}))

	db, err := libdb.NewSQLiteDBManager(context.Background(), dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer db.Close()

	override, ok, err := modelcapability.New(runtimetypes.New(db.WithoutTransaction())).Get(context.Background(), "openai", "gpt-5-mini")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, override.CanThink)
	require.True(t, *override.CanThink)
}

func TestUnit_ModelCapabilitySetCmd_PersistsVisionOverride(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "capability-vision-cli.db")
	cmd := testCobraCmd()
	cmd.SetOut(&bytes.Buffer{})
	require.NoError(t, cmd.Root().PersistentFlags().Set("db", dbPath))
	cmd.Flags().String("think", "", "")
	cmd.Flags().String("vision", "", "")
	require.NoError(t, cmd.Flags().Set("vision", "true"))

	require.NoError(t, modelCapabilitySetCmd.RunE(cmd, []string{"Ollama", "my-vlm"}))

	db, err := libdb.NewSQLiteDBManager(context.Background(), dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer db.Close()

	override, ok, err := modelcapability.New(runtimetypes.New(db.WithoutTransaction())).Get(context.Background(), "ollama", "my-vlm")
	require.NoError(t, err)
	require.True(t, ok)
	require.Nil(t, override.CanThink, "vision-only override must not fabricate a think override")
	require.NotNil(t, override.CanVision)
	require.True(t, *override.CanVision)
}
