package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/beam/internal/models/modelrepo"
	msgcodec "github.com/contenox/beam/internal/models/modelrepo/codec/messages"
	"github.com/stretchr/testify/require"
)

func TestUnit_AnthropicChat_RequestShapeAndResponse(t *testing.T) {
	var gotPath, gotAPIKey, gotVersion string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"hi there"}]}`))
	}))
	defer srv.Close()

	p := NewAnthropicProvider("secret-key", "claude-sonnet-4-5", []string{srv.URL},
		modelrepo.CapabilityConfig{CanChat: true}, srv.Client(), nil)
	chat, err := p.GetChatConnection(context.Background(), "")
	require.NoError(t, err)

	res, err := chat.Chat(context.Background(), []modelrepo.Message{{Role: "user", Content: "hi"}})
	require.NoError(t, err)

	require.Equal(t, "/v1/messages", gotPath)
	require.Equal(t, "secret-key", gotAPIKey, "direct Anthropic must auth via x-api-key")
	require.Equal(t, anthropicAPIVersion, gotVersion, "direct Anthropic must send anthropic-version header")
	require.Equal(t, "claude-sonnet-4-5", gotBody["model"], "direct Anthropic puts model in the body")
	require.Nil(t, gotBody["anthropic_version"], "anthropic_version is a Vertex-only body field")
	require.Equal(t, "hi there", res.Message.Content)
}

func TestUnit_AnthropicChat_OmitsThinkingWhenCapabilityIsFalse(t *testing.T) {
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"hi there"}]}`))
	}))
	defer srv.Close()

	p := NewAnthropicProvider("secret-key", "claude-sonnet-4-5", []string{srv.URL},
		modelrepo.CapabilityConfig{CanChat: true}, srv.Client(), nil)
	chat, err := p.GetChatConnection(context.Background(), "")
	require.NoError(t, err)

	_, err = chat.Chat(context.Background(), []modelrepo.Message{{Role: "user", Content: "hi"}}, modelrepo.WithThink("high"))
	require.NoError(t, err)
	require.NotNil(t, gotBody)
	require.Nil(t, gotBody["thinking"], "provider with CanThink=false must not send Anthropic thinking controls")
	require.Nil(t, gotBody["output_config"], "provider with CanThink=false must not send Anthropic effort controls")
}

func TestUnit_AnthropicCatalog_RegisteredAndChatCapable(t *testing.T) {
	cp, err := modelrepo.NewCatalogProvider(modelrepo.BackendSpec{Type: "anthropic", APIKey: "k"})
	require.NoError(t, err, "anthropic must be registered in the catalog registry")
	require.Equal(t, "anthropic", cp.Type())

	prov := cp.ProviderFor(modelrepo.ObservedModel{
		Name:             "claude-sonnet-4-5",
		CapabilityConfig: modelrepo.CapabilityConfig{CanChat: true, CanStream: true, CanPrompt: true},
	})
	require.Equal(t, "anthropic", prov.GetType())
	require.True(t, prov.CanChat())
	require.False(t, prov.CanEmbed())
}

func TestUnit_AnthropicCatalog_DetectsThinkingFromCapabilities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/models", r.URL.Path)
		require.Empty(t, r.URL.RawQuery)
		require.Equal(t, "secret-key", r.Header.Get("x-api-key"))
		require.Equal(t, anthropicAPIVersion, r.Header.Get("anthropic-version"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [{
				"id": "claude-sonnet-4-5",
				"created_at": "2026-02-19T00:00:00Z",
				"max_input_tokens": 200000,
				"capabilities": {
					"thinking": {"supported": true, "types": {"enabled": {"supported": true}, "adaptive": {"supported": false}}},
					"effort": {"supported": false}
				}
			}]
		}`))
	}))
	defer srv.Close()

	catalog, err := modelrepo.NewCatalogProvider(modelrepo.BackendSpec{
		Type:    "anthropic",
		BaseURL: srv.URL,
		APIKey:  "secret-key",
	})
	require.NoError(t, err)

	models, err := catalog.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, "claude-sonnet-4-5", models[0].Name)
	require.Equal(t, 200000, models[0].ContextLength)
	require.True(t, models[0].CanThink)

	provider := catalog.ProviderFor(models[0])
	require.True(t, provider.CanThink())
}

func TestUnit_AnthropicCatalog_DoesNotInferThinkingWhenCapabilitiesAreMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.Equal(t, "/v1/models", r.URL.Path)
		require.Empty(t, r.URL.RawQuery)
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-4-5","type":"model","max_output_tokens":64000}]}`))
	}))
	defer srv.Close()

	catalog, err := modelrepo.NewCatalogProvider(modelrepo.BackendSpec{Type: "anthropic", BaseURL: srv.URL})
	require.NoError(t, err)

	models, err := catalog.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, "claude-sonnet-4-5", models[0].Name)
	require.Equal(t, 64000, models[0].MaxOutputTokens)
	require.False(t, models[0].CanThink, "missing capability metadata must not fall back to model-name inference")
}

func TestUnit_AnthropicThinking_ManualBudgetAndAdaptiveEffort(t *testing.T) {
	manualCfg := &modelrepo.ChatConfig{}
	modelrepo.WithThink("medium").Apply(manualCfg)
	manualReq := msgcodec.Request{MaxTokens: 1500}
	applyAnthropicThinking(&manualReq, "claude-3-7-sonnet-latest", manualCfg)
	require.NotNil(t, manualReq.Thinking)
	require.Equal(t, "enabled", manualReq.Thinking.Type)
	require.Equal(t, 1499, manualReq.Thinking.BudgetTokens, "budget must stay below max_tokens")
	require.Nil(t, manualReq.OutputConfig)

	adaptiveCfg := &modelrepo.ChatConfig{}
	modelrepo.WithThink("xhigh").Apply(adaptiveCfg)
	adaptiveReq := msgcodec.Request{MaxTokens: 4096}
	applyAnthropicThinking(&adaptiveReq, "claude-opus-4-7", adaptiveCfg)
	require.NotNil(t, adaptiveReq.Thinking)
	require.Equal(t, "adaptive", adaptiveReq.Thinking.Type)
	require.Equal(t, "summarized", adaptiveReq.Thinking.Display)
	require.NotNil(t, adaptiveReq.OutputConfig)
	require.Equal(t, "xhigh", adaptiveReq.OutputConfig.Effort)
}

func TestUnit_AnthropicThinking_OffDisablesWhenSupported(t *testing.T) {
	cfg := &modelrepo.ChatConfig{}
	modelrepo.WithThink("off").Apply(cfg)
	req := msgcodec.Request{MaxTokens: 4096}
	applyAnthropicThinking(&req, "claude-3-7-sonnet-latest", cfg)
	require.NotNil(t, req.Thinking)
	require.Equal(t, "disabled", req.Thinking.Type)
}

func TestUnit_AnthropicFable5_UsesAdaptiveThinking(t *testing.T) {
	cfg := &modelrepo.ChatConfig{}
	modelrepo.WithThink("high").Apply(cfg)
	req := msgcodec.Request{MaxTokens: 4096}
	applyAnthropicThinking(&req, "claude-fable-5", cfg)
	require.NotNil(t, req.Thinking)
	require.Equal(t, "adaptive", req.Thinking.Type, "fable-5 must use adaptive thinking")
	require.Equal(t, "summarized", req.Thinking.Display)
}

func TestUnit_AnthropicFable5_ThinkOffIsNoOp(t *testing.T) {
	cfg := &modelrepo.ChatConfig{}
	modelrepo.WithThink("off").Apply(cfg)
	req := msgcodec.Request{MaxTokens: 4096}
	applyAnthropicThinking(&req, "claude-fable-5", cfg)
	require.Nil(t, req.Thinking, "fable-5 with Think=off must omit the thinking param (requires adaptive, disabled is rejected)")
}

func TestUnit_AnthropicFable5_SupportsXHighEffort(t *testing.T) {
	cfg := &modelrepo.ChatConfig{}
	modelrepo.WithThink("xhigh").Apply(cfg)
	req := msgcodec.Request{MaxTokens: 4096}
	applyAnthropicThinking(&req, "claude-fable-5", cfg)
	require.NotNil(t, req.OutputConfig)
	require.Equal(t, "xhigh", req.OutputConfig.Effort)
}

func TestUnit_AnthropicChat_StripsTemperatureForFable5(t *testing.T) {
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer srv.Close()

	p := NewAnthropicProvider("key", "claude-fable-5", []string{srv.URL},
		modelrepo.CapabilityConfig{CanChat: true}, srv.Client(), nil)
	chat, err := p.GetChatConnection(context.Background(), "")
	require.NoError(t, err)

	temp := 0.7
	_, err = chat.Chat(context.Background(), []modelrepo.Message{{Role: "user", Content: "hi"}},
		modelrepo.WithTemperature(temp))
	require.NoError(t, err)
	require.Nil(t, gotBody["temperature"], "fable-5 must not send temperature")
	require.Nil(t, gotBody["top_p"], "fable-5 must not send top_p")
}

func TestUnit_AnthropicChat_ClampsBeforeThinkingBudget(t *testing.T) {
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer srv.Close()

	p := NewAnthropicProvider("key", "claude-3-7-sonnet-latest", []string{srv.URL},
		modelrepo.CapabilityConfig{CanChat: true, CanThink: true, MaxOutputTokens: 1500}, srv.Client(), nil)
	chat, err := p.GetChatConnection(context.Background(), "")
	require.NoError(t, err)

	_, err = chat.Chat(context.Background(), []modelrepo.Message{{Role: "user", Content: "hi"}},
		modelrepo.WithMaxTokens(9000),
		modelrepo.WithThink("medium"),
	)
	require.NoError(t, err)
	require.Equal(t, float64(1500), gotBody["max_tokens"])
	thinking := gotBody["thinking"].(map[string]any)
	require.Equal(t, "enabled", thinking["type"])
	require.Equal(t, float64(1499), thinking["budget_tokens"])
}

func TestUnit_AnthropicCatalog_PaginatesListModels(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after_id") == "" {
			_, _ = w.Write([]byte(`{
				"data": [{"id":"claude-fable-5","created_at":"2026-01-01T00:00:00Z","max_input_tokens":200000}],
				"has_more": true,
				"last_id": "claude-fable-5"
			}`))
		} else {
			_, _ = w.Write([]byte(`{
				"data": [{"id":"claude-sonnet-4-6","created_at":"2026-01-01T00:00:00Z","max_input_tokens":200000}],
				"has_more": false,
				"last_id": "claude-sonnet-4-6"
			}`))
		}
	}))
	defer srv.Close()

	catalog, err := modelrepo.NewCatalogProvider(modelrepo.BackendSpec{
		Type:    "anthropic",
		BaseURL: srv.URL,
		APIKey:  "key",
	})
	require.NoError(t, err)

	models, err := catalog.ListModels(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, callCount, "must have made 2 requests for 2 pages")
	require.Len(t, models, 2)
	names := []string{models[0].Name, models[1].Name}
	require.Contains(t, names, "claude-fable-5")
	require.Contains(t, names, "claude-sonnet-4-6")
}

// claude-sonnet-5 rejects thinking.type=enabled and deprecates temperature, same as fable-5.
func TestUnit_AnthropicSonnet5_UsesAdaptiveThinkingAndStripsTemperature(t *testing.T) {
	cfg := &modelrepo.ChatConfig{}
	modelrepo.WithThink("high").Apply(cfg)
	req := msgcodec.Request{MaxTokens: 4096}
	applyAnthropicThinking(&req, "claude-sonnet-5", cfg)
	require.NotNil(t, req.Thinking)
	require.Equal(t, "adaptive", req.Thinking.Type, "sonnet-5 must use adaptive thinking")

	offCfg := &modelrepo.ChatConfig{}
	modelrepo.WithThink("off").Apply(offCfg)
	offReq := msgcodec.Request{MaxTokens: 4096}
	applyAnthropicThinking(&offReq, "claude-sonnet-5", offCfg)
	require.Nil(t, offReq.Thinking, "sonnet-5 with Think=off must omit the thinking param")

	require.True(t, anthropicStripsTemperatureParams("claude-sonnet-5"), "sonnet-5 deprecates temperature")
	require.False(t, anthropicStripsTemperatureParams("claude-sonnet-4-5-20250929"), "sonnet-4.5 still accepts temperature")
}
