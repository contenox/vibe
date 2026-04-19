package providerservice

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/runtime/internal/runtimestate"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtime/runtimetypes"
)

func TestSetProviderConfig_OllamaCreatesHostedBackend(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "test.db"), runtimetypes.SchemaSQLite)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	svc := New(db)
	if err := svc.SetProviderConfig(ctx, ProviderTypeOllama, false, &runtimestate.ProviderConfig{
		APIKey: "ollama-test-key",
	}); err != nil {
		t.Fatal(err)
	}

	store := runtimetypes.New(db.WithoutTransaction())
	backend, err := store.GetBackend(ctx, ProviderTypeOllama)
	if err != nil {
		t.Fatal(err)
	}
	if backend.Type != ProviderTypeOllama {
		t.Fatalf("backend.Type = %q, want %q", backend.Type, ProviderTypeOllama)
	}
	if backend.BaseURL != "https://ollama.com/api" {
		t.Fatalf("backend.BaseURL = %q, want https://ollama.com/api", backend.BaseURL)
	}
}
