package acpsvc

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// recordingAgent captures the last PromptRequest it saw and answers with a
// fixed response/error, optionally failing the first N calls (to exercise
// the auto-routed retry).
type recordingAgent struct {
	mu      sync.Mutex
	failN   int
	calls   int
	lastReq agentservice.PromptRequest
	allReqs []agentservice.PromptRequest
	resp    *agentservice.PromptResponse
	failErr error
}

func (a *recordingAgent) Capabilities(context.Context) (*agentservice.AgentCapabilities, error) {
	return nil, nil
}
func (a *recordingAgent) SessionNew(context.Context, string) (string, error) { return "", nil }
func (a *recordingAgent) SessionList(context.Context) ([]*agentservice.SessionInfo, error) {
	return nil, nil
}
func (a *recordingAgent) SessionLoad(context.Context, string) (string, []taskengine.Message, error) {
	return "", nil, nil
}
func (a *recordingAgent) SessionResume(context.Context, string) (string, error) { return "", nil }
func (a *recordingAgent) SessionDelete(context.Context, string) error           { return nil }
func (a *recordingAgent) SessionEnsureDefault(context.Context) (string, error)  { return "", nil }

func (a *recordingAgent) Prompt(_ context.Context, req agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	a.lastReq = req
	a.allReqs = append(a.allReqs, req)
	if a.calls <= a.failN {
		return nil, a.failErr
	}
	if a.resp != nil {
		return a.resp, nil
	}
	return &agentservice.PromptResponse{Output: "completion"}, nil
}

func (a *recordingAgent) requestCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func (a *recordingAgent) requestAt(i int) agentservice.PromptRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.allReqs[i]
}

// autocompleteTestTransport builds a Transport with a configured FIM chain
// and engine, ready for handleAutocomplete; agent replaces the production
// agentservice.Agent via the acAgent test seam.
func autocompleteTestTransport(agent agentservice.Agent) *Transport {
	return &Transport{
		deps: Deps{
			Engine:           &enginesvc.Engine{},
			FIMChainRegistry: &ChainRegistry{defaultChain: &taskengine.TaskChainDefinition{ID: "fim"}},
		},
		acAgent: agent,
	}
}

func callAutocomplete(t *testing.T, ctx context.Context, tr *Transport, p autocompleteParams) (autocompleteResult, *libacp.Error) {
	t.Helper()
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	out, rpcErr := tr.handleAutocomplete(ctx, raw)
	if rpcErr != nil {
		return autocompleteResult{}, rpcErr
	}
	var res autocompleteResult
	require.NoError(t, json.Unmarshal(out, &res))
	return res, nil
}

// TestUnit_ExtRequest_DispatchesAutocomplete pins that handleExtRequest routes
// _contenox/autocomplete to handleAutocomplete rather than answering
// MethodNotFound, matching how _contenox/terminal/run is dispatched.
func TestUnit_ExtRequest_DispatchesAutocomplete(t *testing.T) {
	agent := &recordingAgent{}
	tr := autocompleteTestTransport(agent)

	raw, err := json.Marshal(autocompleteParams{Prefix: "func f() {", Suffix: "}"})
	require.NoError(t, err)
	out, rpcErr := tr.handleExtRequest(context.Background(), extMethodAutocomplete, raw)
	require.Nil(t, rpcErr)
	var res autocompleteResult
	require.NoError(t, json.Unmarshal(out, &res))
	require.Equal(t, "completion", res.Completion)
	require.Equal(t, 1, agent.requestCount())
}

// TestUnit_Autocomplete_ParamsDecode_BuildsFIMPrompt pins that prefix/suffix
// decode into the fim_prefix/fim_suffix/fim_middle prompt shape and reach the
// chain unmodified.
func TestUnit_Autocomplete_ParamsDecode_BuildsFIMPrompt(t *testing.T) {
	agent := &recordingAgent{}
	tr := autocompleteTestTransport(agent)

	res, rpcErr := callAutocomplete(t, context.Background(), tr, autocompleteParams{
		Prefix: "func add(a, b int) int {\n\treturn ",
		Suffix: "\n}",
	})
	require.Nil(t, rpcErr)
	require.Equal(t, "completion", res.Completion)

	req := agent.requestAt(0)
	require.Equal(t, "<fim_prefix>func add(a, b int) int {\n\treturn <fim_suffix>\n}<fim_middle>", req.Input)
	require.Empty(t, req.SessionID, "autocomplete must never set SessionID, or the response would persist to a transcript")
	require.Same(t, tr.deps.FIMChainRegistry.Default(), req.Chain)
}

// TestUnit_Autocomplete_DefaultMaxTokens pins the 128-token default when the
// client omits maxTokens.
func TestUnit_Autocomplete_DefaultMaxTokens(t *testing.T) {
	agent := &recordingAgent{}
	tr := autocompleteTestTransport(agent)

	_, rpcErr := callAutocomplete(t, context.Background(), tr, autocompleteParams{Prefix: "x"})
	require.Nil(t, rpcErr)
	require.Equal(t, "128", agent.requestAt(0).TemplateVars["max_tokens"])
}

// TestUnit_Autocomplete_ExplicitMaxTokensOverridesDefault pins that a
// client-supplied maxTokens rides through to the chain's template vars.
func TestUnit_Autocomplete_ExplicitMaxTokensOverridesDefault(t *testing.T) {
	agent := &recordingAgent{}
	tr := autocompleteTestTransport(agent)

	_, rpcErr := callAutocomplete(t, context.Background(), tr, autocompleteParams{Prefix: "x", MaxTokens: 64})
	require.Nil(t, rpcErr)
	require.Equal(t, "64", agent.requestAt(0).TemplateVars["max_tokens"])
}

// TestUnit_Autocomplete_EmptyPrefixAndSuffix_InvalidParams pins that an
// empty request is rejected before any chain/agent work happens.
func TestUnit_Autocomplete_EmptyPrefixAndSuffix_InvalidParams(t *testing.T) {
	agent := &recordingAgent{}
	tr := autocompleteTestTransport(agent)

	_, rpcErr := callAutocomplete(t, context.Background(), tr, autocompleteParams{})
	require.NotNil(t, rpcErr)
	require.Equal(t, libacp.ErrInvalidParams, rpcErr.Code)
	require.Zero(t, agent.requestCount(), "an invalid request must never reach the agent")
}

// TestUnit_Autocomplete_MalformedJSON_InvalidParams pins that unparseable
// params answer InvalidParams, not a panic.
func TestUnit_Autocomplete_MalformedJSON_InvalidParams(t *testing.T) {
	tr := autocompleteTestTransport(&recordingAgent{})
	_, rpcErr := tr.handleAutocomplete(context.Background(), json.RawMessage(`{not json`))
	require.NotNil(t, rpcErr)
	require.Equal(t, libacp.ErrInvalidParams, rpcErr.Code)
}

// TestUnit_Autocomplete_NoFIMChain_CleanError pins that an absent FIM chain
// (feature not configured) answers a clean, actionable error rather than
// panicking on a nil chain.
func TestUnit_Autocomplete_NoFIMChain_CleanError(t *testing.T) {
	tr := &Transport{deps: Deps{Engine: &enginesvc.Engine{}}, acAgent: &recordingAgent{}}
	_, rpcErr := callAutocomplete(t, context.Background(), tr, autocompleteParams{Prefix: "x"})
	require.NotNil(t, rpcErr)
	require.Equal(t, libacp.ErrMethodNotFound, rpcErr.Code)
}

// TestUnit_Autocomplete_FIMChainRegistryWithoutDefault_CleanError pins the
// same clean error when a registry is configured but has no default chain
// loaded (an "unknown" chain, not merely an absent Deps field).
func TestUnit_Autocomplete_FIMChainRegistryWithoutDefault_CleanError(t *testing.T) {
	tr := &Transport{
		deps:    Deps{Engine: &enginesvc.Engine{}, FIMChainRegistry: &ChainRegistry{}},
		acAgent: &recordingAgent{},
	}
	_, rpcErr := callAutocomplete(t, context.Background(), tr, autocompleteParams{Prefix: "x"})
	require.NotNil(t, rpcErr)
	require.Equal(t, libacp.ErrMethodNotFound, rpcErr.Code)
}

// TestUnit_Autocomplete_EngineNil_SetupRequiredError pins that a setup-only
// transport (no engine configured) answers the same auth-required error the
// rest of acpsvc uses, not a nil-pointer panic on Engine.TaskService.
func TestUnit_Autocomplete_EngineNil_SetupRequiredError(t *testing.T) {
	tr := &Transport{
		deps: Deps{FIMChainRegistry: &ChainRegistry{defaultChain: &taskengine.TaskChainDefinition{ID: "fim"}}},
	}
	_, rpcErr := callAutocomplete(t, context.Background(), tr, autocompleteParams{Prefix: "x"})
	require.NotNil(t, rpcErr)
	require.Equal(t, libacp.ErrAuthRequired, rpcErr.Code)
}

// TestUnit_Autocomplete_CancellationAbortsPromptly pins that a superseded
// keystroke (ctx cancelled while the completion is in flight) unblocks the
// handler immediately instead of running to the 20s timeout, and answers
// cleanly rather than as a JSON-RPC error.
func TestUnit_Autocomplete_CancellationAbortsPromptly(t *testing.T) {
	agent := newBlockingAgent()
	tr := autocompleteTestTransport(agent)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		res    autocompleteResult
		rpcErr *libacp.Error
	}, 1)
	go func() {
		res, rpcErr := callAutocomplete(t, ctx, tr, autocompleteParams{Prefix: "x"})
		done <- struct {
			res    autocompleteResult
			rpcErr *libacp.Error
		}{res, rpcErr}
	}()

	select {
	case <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("agent.Prompt was never called")
	}
	cancel()

	select {
	case out := <-done:
		require.Nil(t, out.rpcErr, "cancellation must not surface as a JSON-RPC error")
		require.Empty(t, out.res.Completion)
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled autocomplete must abort promptly, not run to its internal timeout")
	}
}

// TestUnit_Autocomplete_ProviderModelParamsOverrideChatDefault pins that an
// explicit request provider/model wins over the transport's chat model,
// proving completion model selection is independent of the chat model.
func TestUnit_Autocomplete_ProviderModelParamsOverrideChatDefault(t *testing.T) {
	agent := &recordingAgent{}
	tr := autocompleteTestTransport(agent)
	tr.defaultModel = "chat-model"
	tr.defaultProvider = "chat-provider"

	_, rpcErr := callAutocomplete(t, context.Background(), tr, autocompleteParams{
		Prefix: "x", Provider: "mistral", Model: "mistral-code-fim-latest",
	})
	require.Nil(t, rpcErr)
	req := agent.requestAt(0)
	require.Equal(t, "mistral", req.TemplateVars["provider"])
	require.Equal(t, "mistral-code-fim-latest", req.TemplateVars["model"])
}

// TestUnit_Autocomplete_AutoRoutedCodeModel_FallsBackToChatDefaultOnFailure
// pins the auto-routed retry: with no explicit params and no configured
// autocomplete model, a chat model that already looks like a FIM/code model
// is tried first; if that attempt fails, a second attempt runs with the
// plain chat-default vars rather than surfacing the failure immediately.
func TestUnit_Autocomplete_AutoRoutedCodeModel_FallsBackToChatDefaultOnFailure(t *testing.T) {
	agent := &recordingAgent{failN: 1, failErr: context.DeadlineExceeded}
	tr := autocompleteTestTransport(agent)
	tr.defaultProvider = "mistral"
	tr.defaultModel = "codestral-latest"

	res, rpcErr := callAutocomplete(t, context.Background(), tr, autocompleteParams{Prefix: "x"})
	require.Nil(t, rpcErr)
	require.Equal(t, "completion", res.Completion)
	require.Equal(t, 2, agent.requestCount(), "an auto-routed failure must retry once against the chat default")

	first := agent.requestAt(0)
	require.Equal(t, "mistral", first.TemplateVars["provider"])
	require.Equal(t, "codestral-latest", first.TemplateVars["model"])
}
