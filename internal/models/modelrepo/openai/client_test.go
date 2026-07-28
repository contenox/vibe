package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/models/modelrepo"
)

func TestUnit_OpenAIReasoningEffort(t *testing.T) {
	t.Parallel()

	ptr := func(s string) *string { return &s }
	cases := []struct {
		model string
		think *string
		want  string
	}{
		{"gpt-5", ptr("true"), "high"},
		{"gpt-5", ptr("minimal"), "low"},
		{"gpt-5", ptr("none"), ""},
		{"gpt-5.1", ptr("none"), "none"},
		{"gpt-5.1", ptr("xhigh"), "high"},
		{"gpt-5.4", ptr("none"), "none"},
		{"gpt-5.4", ptr("minimal"), "minimal"},
		{"gpt-5.4", ptr("xhigh"), "xhigh"},
		{"gpt-5-pro", ptr("low"), "high"},
		{"o3-mini", ptr("false"), ""},
		{"o3-mini", ptr("xhigh"), "high"},
	}

	for _, tc := range cases {
		if got := openAIReasoningEffort(tc.model, tc.think); got != tc.want {
			t.Errorf("openAIReasoningEffort(%q, %q) = %q, want %q", tc.model, *tc.think, got, tc.want)
		}
	}
}

func TestUnit_OpenAIGPT5AllowsSamplingParams(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model     string
		reasoning string
		want      bool
	}{
		{"gpt-5", "", false},
		{"openai/gpt-5", "", false},
		{"gpt-5.1", "", true},
		{"gpt-5.4", "none", true},
		{"gpt-5.4", "high", false},
		{"gpt-5-pro", "high", false},
		{"gpt-4o", "", true},
	}
	for _, tc := range cases {
		if got := openAIGPT5AllowsSamplingParams(tc.model, tc.reasoning); got != tc.want {
			t.Errorf("openAIGPT5AllowsSamplingParams(%q, %q) = %v, want %v", tc.model, tc.reasoning, got, tc.want)
		}
	}
}

func TestUnit_BuildOpenAIRequest_GPT5OmitsTemperature(t *testing.T) {
	t.Parallel()
	msgs := []modelrepo.Message{{Role: "user", Content: "hi"}}
	req, _ := buildOpenAIRequest("gpt-5", msgs, []modelrepo.ChatArgument{
		modelrepo.WithTemperature(0.7),
	})
	if req.Temperature != nil {
		t.Fatalf("expected temperature omitted for gpt-5, got %v", req.Temperature)
	}
}

func TestUnit_BuildOpenAIRequest_GPT5NamespacedOmitsTemperature(t *testing.T) {
	t.Parallel()
	msgs := []modelrepo.Message{{Role: "user", Content: "hi"}}
	req, _ := buildOpenAIRequest("openai/gpt-5", msgs, []modelrepo.ChatArgument{
		modelrepo.WithTemperature(0.1),
	})
	if req.Temperature != nil {
		t.Fatalf("expected temperature omitted for namespaced gpt-5, got %v", req.Temperature)
	}
}

func TestUnit_BuildOpenAIRequest_GPT54NoneKeepsTemperature(t *testing.T) {
	t.Parallel()
	msgs := []modelrepo.Message{{Role: "user", Content: "hi"}}
	req, _ := buildOpenAIRequest("gpt-5.4", msgs, []modelrepo.ChatArgument{
		modelrepo.WithTemperature(0.7),
		modelrepo.WithThink("none"),
	})
	if req.Temperature == nil || *req.Temperature != 0.7 {
		t.Fatalf("expected temperature preserved for gpt-5.4 reasoning=none, got %v", req.Temperature)
	}
	if req.ReasoningEffort != "none" {
		t.Fatalf("reasoning_effort = %q, want none", req.ReasoningEffort)
	}
}

func TestUnit_BuildOpenAIRequest_GPT54HighOmitsTemperature(t *testing.T) {
	t.Parallel()
	msgs := []modelrepo.Message{{Role: "user", Content: "hi"}}
	req, _ := buildOpenAIRequest("gpt-5.4", msgs, []modelrepo.ChatArgument{
		modelrepo.WithTemperature(0.7),
		modelrepo.WithThink("high"),
	})
	if req.Temperature != nil {
		t.Fatalf("expected temperature omitted for gpt-5.4 reasoning=high, got %v", req.Temperature)
	}
	if req.ReasoningEffort != "high" {
		t.Fatalf("reasoning_effort = %q, want high", req.ReasoningEffort)
	}
}

func TestUnit_BuildOpenAIRequest_UsesMaxCompletionTokensJSON(t *testing.T) {
	t.Parallel()
	msgs := []modelrepo.Message{{Role: "user", Content: "hi"}}
	req, _ := buildOpenAIRequest("gpt-4o", msgs, []modelrepo.ChatArgument{
		modelrepo.WithMaxTokens(42),
	})
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, `"max_completion_tokens":42`) {
		t.Fatalf("expected max_completion_tokens in payload, got %s", s)
	}
	if strings.Contains(s, `"max_tokens"`) {
		t.Fatalf("did not expect deprecated max_tokens in payload, got %s", s)
	}
}

func TestUnit_OpenAIClient_ClampsChatAndResponsesMaxOutputTokens(t *testing.T) {
	t.Parallel()
	msgs := []modelrepo.Message{{Role: "user", Content: "hi"}}
	client := &openAIClient{maxOutputTokens: 64}

	chatReq, _ := buildOpenAIRequest("gpt-4o", msgs, []modelrepo.ChatArgument{
		modelrepo.WithMaxTokens(128),
	})
	client.clampChatMaxOutputTokens(&chatReq)
	if chatReq.MaxCompletionTokens == nil || *chatReq.MaxCompletionTokens != 64 {
		t.Fatalf("chat max_completion_tokens = %v, want 64", chatReq.MaxCompletionTokens)
	}

	responsesReq, _ := buildOpenAIResponsesRequestWithCapabilities("gpt-5", msgs, []modelrepo.ChatArgument{
		modelrepo.WithMaxTokens(128),
	}, true)
	client.clampResponsesMaxOutputTokens(&responsesReq)
	if responsesReq.MaxOutputTokens == nil || *responsesReq.MaxOutputTokens != 64 {
		t.Fatalf("responses max_output_tokens = %v, want 64", responsesReq.MaxOutputTokens)
	}
}

func TestUnit_BuildOpenAIRequest_GPT4KeepsTemperature(t *testing.T) {
	t.Parallel()
	msgs := []modelrepo.Message{{Role: "user", Content: "hi"}}
	req, _ := buildOpenAIRequest("gpt-4o", msgs, []modelrepo.ChatArgument{
		modelrepo.WithTemperature(0.7),
	})
	if req.Temperature == nil {
		t.Fatal("expected temperature set for gpt-4o")
	}
	if *req.Temperature != 0.7 {
		t.Fatalf("temperature = %v, want 0.7", *req.Temperature)
	}
}

func TestUnit_BuildOpenAIRequest_OmitsReasoningWhenCanThinkFalse(t *testing.T) {
	t.Parallel()
	msgs := []modelrepo.Message{{Role: "user", Content: "hi"}}
	req, _ := buildOpenAIRequestWithCapabilities("gpt-5", msgs, []modelrepo.ChatArgument{
		modelrepo.WithThink("high"),
	}, false)
	if req.ReasoningEffort != "" {
		t.Fatalf("reasoning_effort = %q, want empty when CanThink=false", req.ReasoningEffort)
	}
}

func TestUnit_BuildOpenAIRequest_EmitsReasoningWhenCanThinkTrue(t *testing.T) {
	t.Parallel()
	msgs := []modelrepo.Message{{Role: "user", Content: "hi"}}
	req, _ := buildOpenAIRequestWithCapabilities("gpt-5", msgs, []modelrepo.ChatArgument{
		modelrepo.WithThink("high"),
	}, true)
	if req.ReasoningEffort != "high" {
		t.Fatalf("reasoning_effort = %q, want high", req.ReasoningEffort)
	}
}
