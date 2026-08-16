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

// extMethodAutocomplete is the fill-in-the-middle extension method.
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
			// A superseded keystroke: answer cleanly rather than as an error.
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

// autocomplete runs one FIM completion, falling back to the chat-default model
// once if an auto-routed guess failed.
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

// autocompleteAgent returns the session-less agentservice.Agent used for FIM
// completions.
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

// promptAutocomplete runs the FIM chain once, bounded by a 20s timeout on top of
// ctx.
func (t *Transport) promptAutocomplete(
	ctx context.Context,
	ag agentservice.Agent,
	prompt string,
	vars map[string]string,
) (*agentservice.PromptResponse, error) {
	execCtx, cancel := context.WithTimeout(libtracker.WithNewRequestID(ctx), 20*time.Second)
	defer cancel()
	// SessionID stays empty: agentservice.Prompt only persists to the transcript
	// when it is set. No session and no ToolsAllowlist, so nothing triggers HITL.
	return ag.Prompt(execCtx, agentservice.PromptRequest{
		Input:        prompt,
		Chain:        t.deps.FIMChainRegistry.Default(),
		TemplateVars: vars,
	})
}

// autocompleteTemplateVars resolves the completion model, in priority order:
// request params, the configured default, then an auto-routed guess. autoRouted
// reports the last case, whose failure is worth one retry.
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

func (t *Transport) defaultAutocompleteTemplateVars(maxTokens int) map[string]string {
	vars := t.chainTemplateVars(nil)
	vars["max_tokens"] = fmt.Sprintf("%d", maxTokens)
	return vars
}

func (t *Transport) configuredAutocompleteModel(ctx context.Context) (string, string, bool) {
	if t.deps.DB == nil {
		return "", "", false
	}
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())
	provider := strings.TrimSpace(clikv.Read(ctx, store, "default-autocomplete-provider"))
	model := strings.TrimSpace(clikv.Read(ctx, store, "default-autocomplete-model"))
	return provider, model, provider != "" || model != ""
}

// preferredAutocompleteModel guesses a completion model when none is configured.
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

func isCodeAutocompleteModel(provider, model string) bool {
	if !strings.EqualFold(strings.TrimSpace(provider), "mistral") {
		return false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(model, "fim") || strings.Contains(model, "codestral") || strings.Contains(model, "code")
}

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
