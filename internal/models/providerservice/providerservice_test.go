package providerservice_test

import (
	"context"
	"fmt"
	"testing"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/internal/models/providerservice"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func testSQLiteDB(t *testing.T) libdb.DBManager {
	t.Helper()
	db, err := libdb.NewSQLiteDBManager(context.Background(), t.TempDir()+"/test.db", runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func TestUnit_ProviderService_ConfiguresOpenAIWithEnvReference(t *testing.T) {
	db := testSQLiteDB(t)
	svc := providerservice.New(db, "workspace")
	t.Setenv("OPENAI_API_KEY", "sk-env")

	status, err := svc.Configure(context.Background(), "openai", providerservice.ConfigureProviderRequest{
		APIKeyEnv:    "OPENAI_API_KEY",
		DefaultModel: "gpt-5-mini",
		Upsert:       true,
		SetDefault:   true,
	})
	require.NoError(t, err)
	require.True(t, status.Configured)
	require.Equal(t, "env", status.SecretSource)
	require.True(t, status.SecretPresent)
	require.Equal(t, "OPENAI_API_KEY", status.APIKeyEnv)
	require.NotContains(t, fmt.Sprintf("%+v", status), "sk-env")

	store := runtimetypes.New(db.WithoutTransaction())
	var cfg runtimestate.ProviderConfig
	require.NoError(t, store.GetKV(context.Background(), runtimestate.OpenaiKey, &cfg))
	require.Empty(t, cfg.APIKey)
	require.Equal(t, "OPENAI_API_KEY", cfg.APIKeyEnv)

	backend, err := store.GetBackendByName(context.Background(), "openai")
	require.NoError(t, err)
	require.Equal(t, "https://api.openai.com/v1", backend.BaseURL)
	require.Equal(t, "openai", clikv.Read(context.Background(), store, "default-provider"))
	require.Equal(t, "gpt-5-mini", clikv.Read(context.Background(), store, "default-model"))
}

func TestUnit_ProviderService_RequiresSecretForOpenAI(t *testing.T) {
	db := testSQLiteDB(t)
	svc := providerservice.New(db, "workspace")

	_, err := svc.Configure(context.Background(), "openai", providerservice.ConfigureProviderRequest{Upsert: true})
	require.Error(t, err)
	require.ErrorIs(t, err, providerservice.ErrInvalidProvider)
}

func TestUnit_ProviderService_ConfiguresLocalOllamaWithoutSecret(t *testing.T) {
	db := testSQLiteDB(t)
	svc := providerservice.New(db, "workspace")

	status, err := svc.Configure(context.Background(), "ollama", providerservice.ConfigureProviderRequest{
		Upsert:     true,
		SetDefault: true,
	})
	require.NoError(t, err)
	require.True(t, status.Configured)
	require.Equal(t, "none", status.SecretSource)
	require.Equal(t, "http://127.0.0.1:11434", status.BaseURL)
}

func TestUnit_ProviderService_ListsSupportedProviderCapabilities(t *testing.T) {
	db := testSQLiteDB(t)
	svc := providerservice.New(db, "workspace")

	providers, err := svc.ListSupportedProviders(context.Background())
	require.NoError(t, err)

	byProvider := map[string]providerservice.ProviderCapability{}
	for _, provider := range providers {
		byProvider[provider.Provider] = provider
	}
	require.Contains(t, byProvider, "openai")
	require.Contains(t, byProvider, "anthropic")
	require.Contains(t, byProvider, "gemini")
	require.Contains(t, byProvider, "vertex-google")
	require.NotContains(t, byProvider, "vertex-anthropic")
	require.NotContains(t, byProvider, "vertex-meta")
	require.NotContains(t, byProvider, "vertex-mistralai")
	require.True(t, byProvider["openai"].RequiresSecretConfig)
	require.True(t, byProvider["vertex-google"].RequiresBaseURL)
}
