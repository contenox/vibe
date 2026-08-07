package modelcapability

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

func newTestService(t *testing.T) (context.Context, Service) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "capability.db")
	db, err := libdb.NewSQLiteDBManager(ctx, path, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, New(runtimetypes.New(db.WithoutTransaction()))
}

func TestUnit_Service_SetGetUnsetThink(t *testing.T) {
	ctx, svc := newTestService(t)

	override, err := svc.SetThink(ctx, "OpenAI", "gpt-5-mini", true)
	require.NoError(t, err)
	require.Equal(t, "openai", override.Provider)
	require.Equal(t, "gpt-5-mini", override.Model)
	require.NotNil(t, override.CanThink)
	require.True(t, *override.CanThink)

	got, ok, err := svc.Get(ctx, "openai", "gpt-5-mini")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, got.CanThink)
	require.True(t, *got.CanThink)

	removed, err := svc.Unset(ctx, "openai", "gpt-5-mini")
	require.NoError(t, err)
	require.True(t, removed)

	_, ok, err = svc.Get(ctx, "openai", "gpt-5-mini")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestUnit_Service_FalseOverrideIsPreserved(t *testing.T) {
	ctx, svc := newTestService(t)

	_, err := svc.SetThink(ctx, "vllm", "Qwen/Qwen3-32B", false)
	require.NoError(t, err)

	got, ok, err := svc.Get(ctx, "VLLM", "Qwen/Qwen3-32B")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, got.CanThink)
	require.False(t, *got.CanThink)
}

func TestUnit_Service_SetGetUnsetVision(t *testing.T) {
	ctx, svc := newTestService(t)

	override, err := svc.SetVision(ctx, "Ollama", "gemma3:12b", true)
	require.NoError(t, err)
	require.Equal(t, "ollama", override.Provider)
	require.Equal(t, "gemma3:12b", override.Model)
	require.NotNil(t, override.CanVision)
	require.True(t, *override.CanVision)
	require.Nil(t, override.CanThink)

	got, ok, err := svc.Get(ctx, "ollama", "gemma3:12b")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, got.CanVision)
	require.True(t, *got.CanVision)

	removed, err := svc.Unset(ctx, "ollama", "gemma3:12b")
	require.NoError(t, err)
	require.True(t, removed)

	_, ok, err = svc.Get(ctx, "ollama", "gemma3:12b")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestUnit_Service_ThinkAndVisionOverridesMerge(t *testing.T) {
	ctx, svc := newTestService(t)

	_, err := svc.SetThink(ctx, "openai", "gpt-5-mini", true)
	require.NoError(t, err)
	_, err = svc.SetVision(ctx, "openai", "gpt-5-mini", false)
	require.NoError(t, err)

	got, ok, err := svc.Get(ctx, "openai", "gpt-5-mini")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, got.CanThink, "setting vision must not clobber the think override")
	require.True(t, *got.CanThink)
	require.NotNil(t, got.CanVision)
	require.False(t, *got.CanVision)

	// And the other direction: re-setting think keeps the vision override.
	_, err = svc.SetThink(ctx, "openai", "gpt-5-mini", false)
	require.NoError(t, err)
	got, ok, err = svc.Get(ctx, "openai", "gpt-5-mini")
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, *got.CanThink)
	require.NotNil(t, got.CanVision, "setting think must not clobber the vision override")
	require.False(t, *got.CanVision)
}

func TestUnit_Service_RejectsEmptyProviderOrModel(t *testing.T) {
	ctx, svc := newTestService(t)

	_, err := svc.SetThink(ctx, "", "gpt-5", true)
	require.ErrorContains(t, err, "provider is required")

	_, err = svc.SetThink(ctx, "openai", "  ", true)
	require.ErrorContains(t, err, "model is required")
}
