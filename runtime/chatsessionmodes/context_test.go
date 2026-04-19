package chatsessionmodes

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/stretchr/testify/require"
)

func TestMapChainResolver(t *testing.T) {
	t.Parallel()
	r := &MapChainResolver{ModeToChain: DefaultChainByMode}

	id, err := r.Resolve("my-chain.json", "")
	require.NoError(t, err)
	require.Equal(t, "my-chain.json", id)

	id, err = r.Resolve("", "prompt")
	require.NoError(t, err)
	require.Equal(t, "default-chain.json", id)

	id, err = r.Resolve("explicit.json", "prompt")
	require.NoError(t, err)
	require.Equal(t, "explicit.json", id)

	_, err = r.Resolve("", "")
	require.Error(t, err)

	_, err = r.Resolve("", "unknown-mode-xyz")
	require.Error(t, err)
}

func TestBuildInjectedSystemMessages(t *testing.T) {
	t.Parallel()
	now := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	payload, err := json.Marshal(map[string]string{"text": "hello"})
	require.NoError(t, err)
	msgs, err := BuildInjectedSystemMessages(&ContextPayload{
		Artifacts: []ContextArtifact{
			{Kind: "file_excerpt", Payload: payload},
		},
	}, now)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "system", msgs[0].Role)
	require.Contains(t, msgs[0].Content, "[Context kind=file_excerpt]")
	require.Contains(t, msgs[0].Content, "hello")
	require.NotEmpty(t, msgs[0].ID)

	_, err = BuildInjectedSystemMessages(&ContextPayload{
		Artifacts: []ContextArtifact{{Kind: "bad kind!", Payload: json.RawMessage(`{}`)}},
	}, now)
	require.Error(t, err)
}

func TestPrependInjectionsBeforeLastUser(t *testing.T) {
	t.Parallel()
	u := taskengine.Message{Role: "user", Content: "hi"}
	prior := []taskengine.Message{{Role: "assistant", Content: "prev"}}
	thread, err := PrependInjectionsBeforeLastUser(append(prior, u), []taskengine.Message{
		{Role: "system", Content: "ctx", ID: "i1"},
	})
	require.NoError(t, err)
	require.Len(t, thread, 3)
	require.Equal(t, "assistant", thread[0].Role)
	require.Equal(t, "system", thread[1].Role)
	require.Equal(t, "ctx", thread[1].Content)
	require.Equal(t, "user", thread[2].Role)
}

func TestMergeChatHistoryPreservingInjections(t *testing.T) {
	t.Parallel()
	inj := []taskengine.Message{{ID: "inj1", Role: "system", Content: "[Context kind=x]\n{}"}}
	out := []taskengine.Message{
		{ID: "u1", Role: "user", Content: "q"},
		{ID: "a1", Role: "assistant", Content: "a"},
	}
	merged := MergeChatHistoryPreservingInjections(inj, out)
	require.Len(t, merged, 3)
	require.Equal(t, "inj1", merged[0].ID)
	require.Equal(t, "u1", merged[1].ID)

	complete := []taskengine.Message{
		{ID: "inj1", Role: "system", Content: "x"},
		{ID: "u1", Role: "user", Content: "q"},
		{ID: "a1", Role: "assistant", Content: "a"},
	}
	same := MergeChatHistoryPreservingInjections(inj, complete)
	require.Equal(t, complete, same)
}
