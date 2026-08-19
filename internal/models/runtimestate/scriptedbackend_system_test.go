package runtimestate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/models/modelrepo/scriptedtest"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

const scriptedDialog = `{
  "turns": [
    {
      "text": "Reading the diff.",
      "tool_calls": [
        {"name": "git_diff", "arguments": {"path": "."}}
      ]
    },
    {"text": "Two files changed: README.md and main.go."}
  ]
}`

func scriptedBackendState(t *testing.T, dialog string) (context.Context, *State, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	scriptPath := filepath.Join(dir, "dialog.json")
	require.NoError(t, os.WriteFile(scriptPath, []byte(dialog), 0o600))

	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(dir, "scripted.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	bus := libbus.NewSQLite(db.WithoutTransaction())
	t.Cleanup(func() { _ = bus.Close() })

	state, err := New(ctx, db, bus, WithAutoDiscoverModels())
	require.NoError(t, err)

	require.NoError(t, runtimetypes.New(db.WithoutTransaction()).CreateBackend(ctx, &runtimetypes.Backend{
		ID:      "scripted-backend",
		Name:    "scripted",
		Type:    modelrepo.ScriptedTestBackendType,
		BaseURL: scriptPath,
	}))
	require.NoError(t, state.RunBackendCycle(ctx))
	return ctx, state, scriptPath
}

func scriptedProvider(t *testing.T, ctx context.Context, state *State) modelrepo.Provider {
	t.Helper()
	runtime := state.Get(ctx)
	require.Contains(t, runtime, "scripted-backend")
	require.Empty(t, runtime["scripted-backend"].Error,
		"a scripted backend must come up without an error; its script is right there on disk")
	require.Equal(t, []string{scriptedtest.DefaultModelName}, runtime["scripted-backend"].Models,
		"a script that names no model is exposed under a name that says test")

	providers, err := LocalProviderAdapter(ctx, libtracker.NoopTracker{}, runtime)(ctx, modelrepo.ScriptedTestBackendType)
	require.NoError(t, err)
	require.Len(t, providers, 1)
	require.Equal(t, modelrepo.ScriptedTestBackendType, providers[0].GetType(),
		"every surface that prints the provider type must read scripted-test")
	return providers[0]
}

// The scripted backend must reach the runtime the way any backend does: a
// registered row, a backend cycle, then a provider the resolver can pick.
func TestSystem_ScriptedBackendStreamsToolCallThenAnswer(t *testing.T) {
	ctx, state, scriptPath := scriptedBackendState(t, scriptedDialog)
	provider := scriptedProvider(t, ctx, state)
	backendID := provider.GetBackendIDs()[0]
	require.Equal(t, scriptPath, backendID, "the backend id is the script the dialog is replayed from")

	require.True(t, provider.CanStream())
	stream, err := provider.GetStreamConnection(ctx, backendID)
	require.NoError(t, err)

	parcels, err := stream.Stream(ctx, []modelrepo.Message{{Role: "user", Content: "what changed?"}})
	require.NoError(t, err)

	asm := modelrepo.NewStreamAssembler(provider.GetType(), provider.ModelName())
	for parcel := range parcels {
		require.NoError(t, asm.Consume(parcel))
	}
	first, err := asm.Result()
	require.NoError(t, err, "the scripted stream must satisfy the same assembler the real providers feed")
	require.Equal(t, "Reading the diff.", first.Content)
	require.Len(t, first.ToolCalls, 1)
	require.Equal(t, "git_diff", first.ToolCalls[0].Function.Name)
	require.JSONEq(t, `{"path":"."}`, first.ToolCalls[0].Function.Arguments)
	require.Equal(t, "tool_calls", first.FinishReason)

	chat, err := provider.GetChatConnection(ctx, backendID)
	require.NoError(t, err)
	second, err := chat.Chat(ctx, []modelrepo.Message{{Role: "tool", Content: "2 files changed"}})
	require.NoError(t, err, "turn 2 answers using the tool result, from the same script cursor the stream advanced")
	require.Equal(t, "Two files changed: README.md and main.go.", second.Message.Content)
	require.Empty(t, second.ToolCalls)
	require.Equal(t, "stop", second.FinishReason)
}

// Past the last turn the backend must accuse itself: no invented reply, and a
// message that names the script file and the turn nobody wrote.
func TestSystem_ScriptedBackendRefusesToRunPastTheScript(t *testing.T) {
	ctx, state, scriptPath := scriptedBackendState(t, scriptedDialog)
	provider := scriptedProvider(t, ctx, state)
	backendID := provider.GetBackendIDs()[0]

	chat, err := provider.GetChatConnection(ctx, backendID)
	require.NoError(t, err)
	for turn := 0; turn < 2; turn++ {
		_, err := chat.Chat(ctx, []modelrepo.Message{{Role: "user", Content: "go on"}})
		require.NoErrorf(t, err, "turn %d is scripted", turn)
	}

	_, err = chat.Chat(ctx, []modelrepo.Message{{Role: "user", Content: "go on"}})
	require.Error(t, err, "a fourth turn was never written; the run must fail rather than improvise")
	require.ErrorContains(t, err, scriptPath)
	require.ErrorContains(t, err, "turn 2")
	require.ErrorContains(t, err, "exhausted")
}
