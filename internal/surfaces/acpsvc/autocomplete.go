package acpsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/libtracker"
)

// extMethodAutocomplete is the FIM (fill-in-the-middle) extension method:
// ported from vscodeagent/chat.go's autocomplete. It is read-only (a single
// completion, no tools), so it never enters the HITL/permission path, and it
// never sets PromptRequest.SessionID, so it never touches the session
// transcript.
const extMethodAutocomplete = "_contenox/autocomplete"

type autocompleteParams struct {
	Prefix     string `json:"prefix"`
	Suffix     string `json:"suffix,omitempty"`
	LanguageID string `json:"languageId,omitempty"`
	URI        string `json:"uri,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	MaxTokens  int    `json:"maxTokens,omitempty"`
}

type autocompleteResult struct {
	Completion string `json:"completion"`
}

// handleAutocomplete answers a `_contenox/autocomplete` request. ctx is the
// per-request context libacp derives for every inbound request (see
// conn.go's dispatch); a "$/cancel_request" for this request's id cancels it
// the same way it would any other extension request, so a superseded
// keystroke aborts the in-flight completion without extra bookkeeping here.
func (t *Transport) handleAutocomplete(ctx context.Context, params json.RawMessage) (json.RawMessage, *libacp.Error) {
	if t.deps.Engine == nil {
		if lerr, ok := errSetupRequired().(*libacp.Error); ok {
			return nil, lerr
		}
		return nil, libacp.InternalError("engine is not configured")
	}
	if t.deps.FIMChainRegistry == nil || t.deps.FIMChainRegistry.Default() == nil {
		return nil, libacp.NewError(libacp.ErrMethodNotFound, "autocomplete is not configured on this server (no FIM chain)")
	}
	var p autocompleteParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, libacp.NewErrorf(libacp.ErrInvalidParams, "invalid params: %v", err)
		}
	}
	if strings.TrimSpace(p.Prefix) == "" && strings.TrimSpace(p.Suffix) == "" {
		return nil, libacp.NewError(libacp.ErrInvalidParams, "prefix or suffix is required")
	}

	res, err := t.autocomplete(ctx, p)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			// A superseded keystroke: the client already stopped waiting via
			// $/cancel_request. Answer cleanly rather than as an error.
			out, _ := json.Marshal(autocompleteResult{})
			return out, nil
		}
		return nil, libacp.InternalError(err.Error())
	}
	out, mErr := json.Marshal(res)
	if mErr != nil {
		return nil, libacp.InternalError(mErr.Error())
	}
	return out, nil
}

// autocomplete runs one FIM completion, mirroring vscodeagent's
// (*Server).autocomplete: build the fim_prefix/fim_suffix/fim_middle prompt,
// resolve the completion model (independent of the chat model), run it
// through the FIM chain, and fall back to the chat-default model once if an
// auto-routed guess failed.
func (t *Transport) autocomplete(ctx context.Context, p autocompleteParams) (autocompleteResult, error) {
	maxTokens := p.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 128
	}
	ag := t.autocompleteAgent()
	vars, autoRouted := t.autocompleteTemplateVars(ctx, p, maxTokens)
	prompt := "<fim_prefix>" + p.Prefix + "<fim_suffix>" + p.Suffix + "<fim_middle>"
	resp, err := t.promptAutocomplete(ctx, ag, prompt, vars)
	if err != nil && autoRouted {
		resp, err = t.promptAutocomplete(ctx, ag, prompt, t.defaultAutocompleteTemplateVars(maxTokens))
	}
	if err != nil {
		return autocompleteResult{}, err
	}
	return autocompleteResult{Completion: strings.TrimRightFunc(extractAssistantText(resp.Output), unicode.IsSpace)}, nil
}

// autocompleteAgent returns the agentservice.Agent used for FIM completions.
// Production builds a fresh, session-less agent scoped to no contenox
// session (so PromptRequest.SessionID stays empty and nothing is persisted);
// acAgent lets tests substitute a fake without a real engine/DB.
func (t *Transport) autocompleteAgent() agentservice.Agent {
	if t.acAgent != nil {
		return t.acAgent
	}
	return agentservice.New(agentservice.Deps{
		Engine:      t.deps.Engine,
		DB:          t.deps.DB,
		WorkspaceID: t.workspaceID(),
		Identity:    "acp-client",
	})
}

// promptAutocomplete runs the FIM chain once. Bounded by a 20s timeout
// (matching vscodeagent's promptAutocomplete) on top of ctx, so a stuck
// completion cannot outlive a reasonable keystroke-to-suggestion budget; ctx
// cancellation (a superseded request) still wins immediately either way.
func (t *Transport) promptAutocomplete(
	ctx context.Context,
	ag agentservice.Agent,
	prompt string,
	vars map[string]string,
) (*agentservice.PromptResponse, error) {
	execCtx, cancel := context.WithTimeout(libtracker.WithNewRequestID(ctx), 20*time.Second)
	defer cancel()
	// SessionID is intentionally left empty: agentservice.Prompt only persists
	// to the chat transcript when it is set (agent.go's `if req.SessionID != ""`
	// guards). No session, no ToolsAllowlist: nothing here can trigger HITL.
	return ag.Prompt(execCtx, agentservice.PromptRequest{
		Input:        prompt,
		Chain:        t.deps.FIMChainRegistry.Default(),
		TemplateVars: vars,
	})
}

// autocompleteTemplateVars resolves the completion model, in priority order:
// explicit request params, the operator-configured default (clikv), then an
// auto-routed guess (the chat model if it already looks like a FIM/code
// model, else a discovered mistral backend). autoRouted reports the last
// case, so the caller knows a failure is worth retrying against the plain
// chat-default vars rather than surfacing immediately.
func (t *Transport) autocompleteTemplateVars(
	ctx context.Context,
	p autocompleteParams,
	maxTokens int,
) (map[string]string, bool) {
	vars := t.defaultAutocompleteTemplateVars(maxTokens)
	if provider := strings.TrimSpace(p.Provider); provider != "" {
		vars["provider"] = provider
	}
	if model := strings.TrimSpace(p.Model); model != "" {
		vars["model"] = model
	}
	if strings.TrimSpace(p.Provider) != "" || strings.TrimSpace(p.Model) != "" {
		return vars, false
	}
	if provider, model, ok := t.configuredAutocompleteModel(ctx); ok {
		if provider != "" {
			vars["provider"] = provider
		}
		if model != "" {
			vars["model"] = model
		}
		return vars, false
	}
	if provider, model, ok := t.preferredAutocompleteModel(ctx); ok {
		vars["provider"] = provider
		vars["model"] = model
		return vars, true
	}
	return vars, false
}

// defaultAutocompleteTemplateVars is the chat-default vars (model/provider
// come from the transport's live /model /provider selection, not any
// session) with max_tokens pinned to this request's budget.
func (t *Transport) defaultAutocompleteTemplateVars(maxTokens int) map[string]string {
	vars := t.chainTemplateVars(nil)
	vars["max_tokens"] = fmt.Sprintf("%d", maxTokens)
	return vars
}

// configuredAutocompleteModel reads the operator-set autocomplete
// model/provider, the same clikv keys vscodeagent's /autocomplete-model and
// /autocomplete-provider commands write, so a setting made on one surface
// carries over to the other.
func (t *Transport) configuredAutocompleteModel(ctx context.Context) (string, string, bool) {
	if t.deps.DB == nil {
		return "", "", false
	}
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())
	provider := strings.TrimSpace(clikv.Read(ctx, store, "default-autocomplete-provider"))
	model := strings.TrimSpace(clikv.Read(ctx, store, "default-autocomplete-model"))
	return provider, model, provider != "" || model != ""
}

// preferredAutocompleteModel guesses a completion model when none is
// configured: the chat-default model if it already looks like a FIM/code
// model, else the first registered mistral backend's FIM model.
func (t *Transport) preferredAutocompleteModel(ctx context.Context) (string, string, bool) {
	if isCodeAutocompleteModel(t.provider(), t.model()) {
		return t.provider(), t.model(), true
	}
	if t.deps.DB == nil {
		return "", "", false
	}
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())
	backends, err := store.ListAllBackends(ctx)
	if err != nil {
		return "", "", false
	}
	for _, backend := range backends {
		if backend != nil && strings.EqualFold(strings.TrimSpace(backend.Type), "mistral") {
			return "mistral", "mistral-code-fim-latest", true
		}
	}
	return "", "", false
}

// isCodeAutocompleteModel reports whether provider/model already looks like
// a FIM/code-completion model, so the chat default can double as the
// autocomplete default without a separate configured one.
func isCodeAutocompleteModel(provider, model string) bool {
	if !strings.EqualFold(strings.TrimSpace(provider), "mistral") {
		return false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(model, "fim") || strings.Contains(model, "codestral") || strings.Contains(model, "code")
}

// extractAssistantText pulls the completion text out of a chain's final
// output: a bare string, or the last assistant message of a ChatHistory.
func extractAssistantText(output any) string {
	switch v := output.(type) {
	case string:
		return v
	case taskengine.ChatHistory:
		for i := len(v.Messages) - 1; i >= 0; i-- {
			if v.Messages[i].Role == "assistant" && v.Messages[i].Content != "" {
				return v.Messages[i].Content
			}
		}
	case *taskengine.ChatHistory:
		if v != nil {
			return extractAssistantText(*v)
		}
	}
	return ""
}
