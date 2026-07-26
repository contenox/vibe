package contenoxcli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/models/backendservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/internal/surfaces/acpsvc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envSetupTestDB(t *testing.T) libdb.DBManager {
	t.Helper()
	db, err := libdb.NewSQLiteDBManager(context.Background(), filepath.Join(t.TempDir(), "env-setup.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestUnit_CompleteEnvSetup_OllamaWithDefaults(t *testing.T) {
	t.Setenv(envDefaultProvider, "ollama")
	t.Setenv(envDefaultModel, "")
	db := envSetupTestDB(t)
	ctx := context.Background()

	require.NoError(t, completeEnvSetup(ctx, db))

	assert.Equal(t, "ollama", acpsvc.ReadConfigValue(ctx, db, "default-provider"))
	assert.Equal(t, "qwen2.5:7b", acpsvc.ReadConfigValue(ctx, db, "default-model"),
		"empty model must fall back to the provider's default, matching the wizard")

	backends, err := backendservice.New(db).List(ctx, nil, 10)
	require.NoError(t, err)
	require.Len(t, backends, 1)
	assert.Equal(t, "ollama", backends[0].Type)

	// Idempotent: a second run must not duplicate the backend.
	require.NoError(t, completeEnvSetup(ctx, db))
	backends, err = backendservice.New(db).List(ctx, nil, 10)
	require.NoError(t, err)
	assert.Len(t, backends, 1)
}

func TestUnit_CompleteEnvSetup_ActionableErrors(t *testing.T) {
	db := envSetupTestDB(t)
	ctx := context.Background()

	t.Setenv(envDefaultProvider, "")
	t.Setenv(envDefaultModel, "")
	err := completeEnvSetup(ctx, db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), envDefaultProvider, "error must name the missing variable")

	t.Setenv(envDefaultProvider, "not-a-provider")
	err = completeEnvSetup(ctx, db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-a-provider")
	assert.Contains(t, err.Error(), "ollama", "error must list valid providers")

	// A cloud provider without its API key must name the key variable.
	t.Setenv(envDefaultProvider, "openai")
	err = completeEnvSetup(ctx, db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OPENAI_API_KEY")

	// Nothing may have been persisted by the failed attempts.
	assert.Empty(t, acpsvc.ReadConfigValue(ctx, db, "default-provider"))
	assert.Empty(t, acpsvc.ReadConfigValue(ctx, db, "default-model"))
}

func TestUnit_CompleteEnvSetup_CloudProviderWithKey(t *testing.T) {
	t.Setenv(envDefaultProvider, "openai")
	t.Setenv(envDefaultModel, "gpt-5-mini")
	t.Setenv("OPENAI_API_KEY", "sk-test-not-real")
	db := envSetupTestDB(t)
	ctx := context.Background()

	require.NoError(t, completeEnvSetup(ctx, db))
	assert.Equal(t, "openai", acpsvc.ReadConfigValue(ctx, db, "default-provider"))
	assert.Equal(t, "gpt-5-mini", acpsvc.ReadConfigValue(ctx, db, "default-model"))

	backends, err := backendservice.New(db).List(ctx, nil, 10)
	require.NoError(t, err)
	require.Len(t, backends, 1)
	assert.Equal(t, "openai", backends[0].Type)
	assert.True(t, strings.HasPrefix(backends[0].BaseURL, "https://api.openai.com"))
}

func TestUnit_ACPEnvSetupVars_ContractShape(t *testing.T) {
	vars := acpEnvSetupVars()
	require.NotEmpty(t, vars)
	assert.Equal(t, envDefaultProvider, vars[0].Name)
	require.NotNil(t, vars[0].Secret)
	assert.False(t, *vars[0].Secret, "the provider name is not a secret")
	assert.Equal(t, envDefaultModel, vars[1].Name)
	assert.True(t, vars[1].Optional)

	var keyVars []string
	for _, v := range vars[2:] {
		assert.True(t, v.Optional, "API keys are per-provider, so each is optional: %s", v.Name)
		assert.Nil(t, v.Secret, "API keys omit secret so the spec default (true) applies: %s", v.Name)
		keyVars = append(keyVars, v.Name)
	}
	assert.Contains(t, keyVars, "OPENAI_API_KEY")
	assert.Contains(t, keyVars, "GEMINI_API_KEY")
}

// TestUnit_ChainAgentSpawnContract_MatchesTheACPCommand guards the one piece of
// duplication chains-as-agents needs. The instance kernel spawns a chain unit by
// re-executing this binary with a subcommand and a chain-path environment
// variable it has to name literally — it cannot import the CLI or the ACP
// service to ask, because both sit above it and one imports it (a cycle). This
// test is the join: if the acp subcommand were renamed, or its chain-path
// variable changed, every chain unit would silently stop finding its chain, and
// only an end-to-end spawn would notice. Here it fails at compile-and-assert
// time instead.
func TestUnit_ChainAgentSpawnContract_MatchesTheACPCommand(t *testing.T) {
	assert.Equal(t, acpProfileACP.chainEnv, agentinstance.ChainPathEnvVar,
		"the kernel's chain-path variable must be the one `contenox acp` reads")
	assert.Equal(t, acpCmd.Use, agentinstance.ChainACPSubcommand,
		"the kernel's self-spawn argv must name the subcommand that serves ACP")
}
