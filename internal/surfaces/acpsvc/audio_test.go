package acpsvc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

func TestUnit_ExtractAudioParts_SplitsAudioFromText(t *testing.T) {
	wav := []byte{'R', 'I', 'F', 'F', 0x10, 0x00, 0x00, 0x00}
	blocks := []libacp.ContentBlock{
		libacp.NewTextContent("what does this say?"),
		libacp.NewAudioContent(base64.StdEncoding.EncodeToString(wav), "audio/wav"),
		libacp.NewTextContent("second line"),
	}

	audio, rest := extractAudioParts(blocks)

	require.Len(t, audio, 1)
	require.Equal(t, wav, audio[0].Data)
	require.Equal(t, "audio/wav", audio[0].MimeType)
	// Text blocks pass through untouched and in order, so FlattenContent's
	// projection of the remaining blocks is unchanged by the extraction.
	require.Len(t, rest, 2)
	text, dropped := libacp.FlattenContent(rest)
	require.Equal(t, "what does this say?\nsecond line", text)
	require.Empty(t, dropped)
}

func TestUnit_ExtractAudioParts_InvalidBase64StaysDroppedVisible(t *testing.T) {
	blocks := []libacp.ContentBlock{
		libacp.NewAudioContent("not-base64!!", "audio/wav"),
	}

	audio, rest := extractAudioParts(blocks)

	require.Empty(t, audio)
	// The broken block flows on to FlattenContent, which reports it dropped —
	// a visible degradation, never a silent one.
	require.Len(t, rest, 1)
	_, dropped := libacp.FlattenContent(rest)
	require.Equal(t, []string{string(libacp.ContentKindAudio)}, dropped)
}

func TestUnit_ExtractAudioParts_UnsupportedMimeStaysDroppedVisible(t *testing.T) {
	// Matching is exact and canonical (see modelrepo.SupportedAudioMimeTypes):
	// "audio/x-wav" is the classic non-canonical spelling a client might send.
	blocks := []libacp.ContentBlock{
		libacp.NewAudioContent(base64.StdEncoding.EncodeToString([]byte{1, 2, 3}), "audio/x-wav"),
	}

	audio, rest := extractAudioParts(blocks)

	require.Empty(t, audio)
	require.Len(t, rest, 1)
	_, dropped := libacp.FlattenContent(rest)
	require.Equal(t, []string{string(libacp.ContentKindAudio)}, dropped)
}

func TestUnit_ExtractAudioParts_OversizeStaysDroppedVisible(t *testing.T) {
	over := bytes.Repeat([]byte{0xA5}, modelrepo.MaxInlineAudioBytes+1)
	blocks := []libacp.ContentBlock{
		libacp.NewAudioContent(base64.StdEncoding.EncodeToString(over), "audio/mpeg"),
	}

	audio, rest := extractAudioParts(blocks)

	require.Empty(t, audio)
	require.Len(t, rest, 1)
	_, dropped := libacp.FlattenContent(rest)
	require.Equal(t, []string{string(libacp.ContentKindAudio)}, dropped)
}

func TestUnit_ExtractAudioParts_SizeCapIsABudgetAcrossBlocks(t *testing.T) {
	half := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5A}, modelrepo.MaxInlineAudioBytes/2+1))
	blocks := []libacp.ContentBlock{
		libacp.NewAudioContent(half, "audio/flac"),
		libacp.NewAudioContent(half, "audio/flac"),
	}

	audio, rest := extractAudioParts(blocks)

	// The cap is the prompt-wide raw-byte budget the audio-capable providers
	// enforce (modelrepo.ValidateAudioParts): the first block fits, the second
	// would cross it and is refused per block — forwarding it would fail the
	// whole turn at provider request building, first block included.
	require.Len(t, audio, 1)
	require.Len(t, rest, 1)
	_, dropped := libacp.FlattenContent(rest)
	require.Equal(t, []string{string(libacp.ContentKindAudio)}, dropped)
}

func TestUnit_ExtractAudioParts_AudioOnlyPromptYieldsNoText(t *testing.T) {
	blocks := []libacp.ContentBlock{
		libacp.NewAudioContent(base64.StdEncoding.EncodeToString([]byte{1, 2, 3}), "audio/ogg"),
	}

	audio, rest := extractAudioParts(blocks)

	require.Len(t, audio, 1)
	require.Empty(t, rest)
}

// TestUnit_Initialize_AdvertisesAudioPromptCapability pins the capability to
// the consumption path: audio blocks ride to CanAudio providers now (see
// extractAudioParts), and a compliant client reads Audio: false as "never send
// audio", which would make the whole path unreachable.
func TestUnit_Initialize_AdvertisesAudioPromptCapability(t *testing.T) {
	tr := &Transport{
		sessions:        make(map[libacp.SessionID]*sessionEntry),
		contenoxToACPID: make(map[string]libacp.SessionID),
	}
	resp, err := tr.Initialize(context.Background(), libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	require.True(t, resp.AgentCapabilities.PromptCapabilities.Image)
	require.True(t, resp.AgentCapabilities.PromptCapabilities.Audio)
}

// TestUnit_ExplainDroppedContent_AudioNamesItsBounds pins the honesty of the
// audio refusal: a dropped audio block is almost always a bounds refusal, and
// the report must name the accepted types and the size cap — verbatim from the
// modelrepo constants — instead of leaving the operator to guess. A report
// that dropped no audio carries none of it.
func TestUnit_ExplainDroppedContent_AudioNamesItsBounds(t *testing.T) {
	report, ok := explainDroppedContent([]string{string(libacp.ContentKindAudio)}, "")
	require.True(t, ok)
	require.Contains(t, report.Explanation, "audio/flac, audio/mp4, audio/mpeg, audio/ogg, audio/wav")
	require.Contains(t, report.Explanation, "14 MiB")
	require.Contains(t, report.Explanation, "default-audio-model",
		"the generic audio sentence must name the remedy too")

	imageOnly, ok := explainDroppedContent([]string{string(libacp.ContentKindImage)}, "")
	require.True(t, ok)
	require.NotContains(t, imageOnly.Explanation, "audio/",
		"the audio bounds have no place in a report that dropped no audio")
}

// TestLoopback_AudioBlock_RidesTheUserMessage drives a valid audio block
// through a real ACP wire and pins the happy path: the block becomes a
// taskengine.AudioPart on the turn's prompt request — bytes and media type
// preserved exactly — and the response carries no dropped-content marker.
func TestLoopback_AudioBlock_RidesTheUserMessage(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	wav := []byte{'R', 'I', 'F', 'F', 0x10, 0x00, 0x00, 0x00}
	var got []taskengine.AudioPart
	h.swapAgent(newResp.SessionID, &loopbackAgent{
		promptFunc: func(_ context.Context, req agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
			got = req.Audio
			return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
		},
	})

	resp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt: []libacp.ContentBlock{
			libacp.NewTextContent("transcribe this"),
			libacp.NewAudioContent(base64.StdEncoding.EncodeToString(wav), "audio/wav"),
		},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, resp.StopReason)
	require.Empty(t, resp.Meta, "a prompt that lost nothing carries no dropped-content marker")
	require.Len(t, got, 1)
	require.Equal(t, wav, got[0].Data)
	require.Equal(t, "audio/wav", got[0].MimeType)
}

// TestLoopback_RefusedAudio_ReachesTheClientOnTheWire mirrors the dropped-image
// regression for audio: a block outside the inline-audio bounds must produce a
// turn that says so — the envelope on the response and the agent message every
// editor renders — never a successful answer written as if nothing was sent.
func TestLoopback_RefusedAudio_ReachesTheClientOnTheWire(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	h.swapAgent(newResp.SessionID, &loopbackAgent{
		promptFunc: func(context.Context, agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
			return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
		},
	})

	resp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt: []libacp.ContentBlock{
			libacp.NewTextContent("what does this recording say?"),
			libacp.NewAudioContent(base64.StdEncoding.EncodeToString([]byte{1, 2, 3}), "audio/x-wav"),
		},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, resp.StopReason)

	require.NotEmpty(t, resp.Meta, "a lossy turn must not answer like an intact one")
	var envelope map[string]droppedContentReport
	require.NoError(t, json.Unmarshal(resp.Meta, &envelope))
	require.Equal(t, []string{string(libacp.ContentKindAudio)}, envelope[droppedContentMetaKey].Kinds)
	require.Contains(t, envelope[droppedContentMetaKey].Explanation, "audio/wav",
		"the refusal must name the accepted media types")
	require.Contains(t, envelope[droppedContentMetaKey].Explanation, "14 MiB",
		"and the inline size cap")

	waitForAgentMessage(t, h, newResp.SessionID, string(libacp.ContentKindAudio))
}

// TestUnit_AudioCapabilityRefusal_Verdicts pins the pre-flight gate's decision
// table: refusal only on positive knowledge (a pinned model the fleet reports
// as non-audio, or an observed fleet with no audio-capable model), and every
// unknown — no state, no observed models, an unseen pin — forwards so the
// resolver keeps the final word.
func TestUnit_AudioCapabilityRefusal_Verdicts(t *testing.T) {
	audioModel := runtimestate.ModelPullStatus{Model: "gemini-2.5-flash", CanChat: true, CanAudio: true}
	textModel := runtimestate.ModelPullStatus{Model: "qwen3-4b", CanChat: true}
	fleet := func(models ...runtimestate.ModelPullStatus) []runtimestate.BackendRuntimeState {
		return []runtimestate.BackendRuntimeState{{PulledModels: models}}
	}

	require.Empty(t, audioCapabilityRefusal(nil, "qwen3-4b"), "no state is unknown, not incapable")
	require.Empty(t, audioCapabilityRefusal(fleet(), "qwen3-4b"), "a fleet with no observed models decides nothing")
	require.Empty(t, audioCapabilityRefusal(fleet(textModel, audioModel), ""), "an audio-capable fleet forwards")
	require.Empty(t, audioCapabilityRefusal(fleet(audioModel), "gemini-2.5-flash"), "an audio-capable pin forwards")
	require.Empty(t, audioCapabilityRefusal(fleet(textModel, audioModel), "unseen-model"),
		"an unseen pin over a capable fleet stays with the resolver")

	pinned := audioCapabilityRefusal(fleet(textModel, audioModel), "qwen3-4b")
	require.Contains(t, pinned, `"qwen3-4b"`, "the pinned refusal names the model")
	require.Contains(t, pinned, "default-audio-model")

	none := audioCapabilityRefusal(fleet(textModel), "")
	require.Contains(t, none, "No configured model accepts audio input")
	require.Contains(t, none, "default-audio-model")
}

// installNonAudioRuntimeState wires a real runtimestate.State over the
// harness's DB and bus, backed by a fake OpenAI-compatible backend serving one
// chat model with no audio capability — so the pre-flight gate reads a KNOWN
// fleet without an audio-capable model, from the same state the model
// dropdown reads. Mirrors the config-options tests' setup.
func installNonAudioRuntimeState(t *testing.T, h *loopbackHarness) {
	t.Helper()
	ctx := context.Background()

	state, err := runtimestate.New(ctx, h.tr.deps.DB, h.bus, runtimestate.WithAutoDiscoverModels())
	require.NoError(t, err)

	original := runtimestate.ReconcileDebounceInterval
	runtimestate.ReconcileDebounceInterval = 0
	t.Cleanup(func() { runtimestate.ReconcileDebounceInterval = original })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "gpt-5"}},
		})
	}))
	t.Cleanup(server.Close)

	store := runtimetypes.New(h.tr.deps.DB.WithoutTransaction())
	require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
		ID: "openai-backend", Name: "openai-backend", Type: "openai", BaseURL: server.URL,
	}))
	keyData, err := json.Marshal(runtimestate.ProviderConfig{APIKey: "test-key", Type: "openai"})
	require.NoError(t, err)
	require.NoError(t, store.SetKV(ctx, runtimestate.OpenaiKey, keyData))

	h.tr.deps.Engine.State = state
}

// TestLoopback_AudioWithoutAudioCapableModel_RefusedBeforeDispatch is the
// bricked-session regression: before the pre-flight gate, a voice note on a
// fleet with no audio-capable model rode to the resolver, failed the whole
// turn as an RPC error, and — persisted into history — re-imposed the audio
// requirement on every later turn, text-only ones included. The gate must
// instead refuse the audio at the surface: the turn runs on the rest of the
// prompt, nothing audio-bearing reaches the agent (so nothing can persist),
// and the next turn works.
func TestLoopback_AudioWithoutAudioCapableModel_RefusedBeforeDispatch(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	installNonAudioRuntimeState(t, h)

	var captured []agentservice.PromptRequest
	h.swapAgent(newResp.SessionID, &loopbackAgent{
		promptFunc: func(_ context.Context, req agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
			captured = append(captured, req)
			return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
		},
	})

	resp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt: []libacp.ContentBlock{
			libacp.NewTextContent("transcribe this recording"),
			libacp.NewAudioContent(base64.StdEncoding.EncodeToString([]byte{'R', 'I', 'F', 'F'}), "audio/wav"),
		},
	})
	require.NoError(t, err, "the turn must not die of the doomed attachment")
	require.Equal(t, libacp.StopReasonEndTurn, resp.StopReason)

	var envelope map[string]droppedContentReport
	require.NotEmpty(t, resp.Meta)
	require.NoError(t, json.Unmarshal(resp.Meta, &envelope))
	require.Equal(t, []string{string(libacp.ContentKindAudio)}, envelope[droppedContentMetaKey].Kinds)

	require.Len(t, captured, 1)
	require.Empty(t, captured[0].Audio, "refused audio must never reach the user message")
	require.Equal(t, "transcribe this recording", captured[0].Input, "the turn runs on the rest of the prompt")

	notice := waitForAgentMessage(t, h, newResp.SessionID, "default-audio-model")
	require.Contains(t, notice, "No configured model accepts audio input")

	// The session survives: a following text-only turn is intact — no marker,
	// no audio requirement inherited from a poisoned history.
	next, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("and now just text")},
	})
	require.NoError(t, err)
	require.Empty(t, next.Meta, "the refusal must leave no trace on later turns")
	require.Len(t, captured, 2)
	require.Empty(t, captured[1].Audio)
}

// TestLoopback_PinnedNonAudioModel_RefusalNamesTheModel pins the pinned
// variant of the pre-flight gate: a session whose selected model the fleet
// reports as non-audio gets the refusal that names that model — mirroring the
// resolver's own pin refusal, which never silently swaps a pinned model.
func TestLoopback_PinnedNonAudioModel_RefusalNamesTheModel(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	installNonAudioRuntimeState(t, h)

	h.tr.sessionMu.Lock()
	sess := h.tr.sessions[newResp.SessionID]
	h.tr.sessionMu.Unlock()
	sess.mu.Lock()
	sess.Model = "gpt-5"
	sess.mu.Unlock()

	h.swapAgent(newResp.SessionID, &loopbackAgent{
		promptFunc: func(context.Context, agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
			return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
		},
	})

	resp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt: []libacp.ContentBlock{
			libacp.NewTextContent("what does this say?"),
			libacp.NewAudioContent(base64.StdEncoding.EncodeToString([]byte{1, 2, 3}), "audio/ogg"),
		},
	})
	require.NoError(t, err)

	var envelope map[string]droppedContentReport
	require.NotEmpty(t, resp.Meta)
	require.NoError(t, json.Unmarshal(resp.Meta, &envelope))
	require.Equal(t, []string{string(libacp.ContentKindAudio)}, envelope[droppedContentMetaKey].Kinds)

	notice := waitForAgentMessage(t, h, newResp.SessionID, `"gpt-5"`)
	require.Contains(t, notice, "does not accept audio input")
	require.Contains(t, notice, "default-audio-model")
}
