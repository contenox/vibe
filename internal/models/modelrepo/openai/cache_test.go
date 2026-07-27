package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/models/modelrepo"
)

func openaiCacheFixtureMessages() []modelrepo.Message {
	return []modelrepo.Message{
		{Role: "system", Content: "be terse"},
		{Role: "user", Content: "hello"},
	}
}

func TestUnit_OpenAICache_PromptCacheKeyFromSessionKey(t *testing.T) {
	args := []modelrepo.ChatArgument{
		modelrepo.WithCacheHints(modelrepo.CacheHints{SessionKey: "abc123", StableSystem: true, StableTools: true}),
	}
	req, _ := buildOpenAIRequestWithCapabilities("gpt-4o", openaiCacheFixtureMessages(), args, true)
	if req.PromptCacheKey != "abc123" {
		t.Fatalf("chat-completions request must carry prompt_cache_key, got %q", req.PromptCacheKey)
	}

	resp, _ := buildOpenAIResponsesRequestWithCapabilities("gpt-5-mini", openaiCacheFixtureMessages(), args, true)
	if resp.PromptCacheKey != "abc123" {
		t.Fatalf("responses request must carry prompt_cache_key, got %q", resp.PromptCacheKey)
	}
}

func TestUnit_OpenAICache_HintsAbsentByteIdentical(t *testing.T) {
	// Without hints (and with hints but no session key) the marshaled request
	// must be byte-identical to the pre-change shape: no prompt_cache_key and
	// nothing else moved.
	plain, _ := buildOpenAIRequestWithCapabilities("gpt-4o", openaiCacheFixtureMessages(), nil, true)
	noKey, _ := buildOpenAIRequestWithCapabilities("gpt-4o", openaiCacheFixtureMessages(), []modelrepo.ChatArgument{
		modelrepo.WithCacheHints(modelrepo.CacheHints{StableSystem: true, StableTools: true}),
	}, true)

	rawPlain, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	rawNoKey, err := json.Marshal(noKey)
	if err != nil {
		t.Fatal(err)
	}
	if string(rawPlain) != string(rawNoKey) {
		t.Fatalf("keyless hints must not change the request bytes:\n%s\n%s", rawPlain, rawNoKey)
	}
	if strings.Contains(string(rawPlain), "prompt_cache_key") {
		t.Fatalf("prompt_cache_key must be omitted when absent: %s", rawPlain)
	}

	// With a key, the request differs ONLY by the prompt_cache_key member.
	keyed, _ := buildOpenAIRequestWithCapabilities("gpt-4o", openaiCacheFixtureMessages(), []modelrepo.ChatArgument{
		modelrepo.WithCacheHints(modelrepo.CacheHints{SessionKey: "k"}),
	}, true)
	rawKeyed, err := json.Marshal(keyed)
	if err != nil {
		t.Fatal(err)
	}
	var mPlain, mKeyed map[string]any
	if err := json.Unmarshal(rawPlain, &mPlain); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawKeyed, &mKeyed); err != nil {
		t.Fatal(err)
	}
	delete(mKeyed, "prompt_cache_key")
	a, _ := json.Marshal(mPlain)
	b, _ := json.Marshal(mKeyed)
	if string(a) != string(b) {
		t.Fatalf("prompt_cache_key must be the only difference:\n%s\n%s", a, b)
	}
}

func TestUnit_OpenAICache_ChatCompletionsUsageIncludesCachedTokens(t *testing.T) {
	// OpenAI prompt_tokens already INCLUDES cached tokens: no summation, the
	// cached count is read from prompt_tokens_details.cached_tokens.
	body := `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1200,"completion_tokens":9,"total_tokens":1209,
		"prompt_tokens_details":{"cached_tokens":1024}}}`
	var resp openAIChatCompletionResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	u := resp.Usage.neutralUsage()
	if u.PromptTokens != 1200 || u.CacheReadTokens != 1024 || u.CacheWriteTokens != 0 {
		t.Fatalf("chat-completions usage extraction wrong: %+v", u)
	}
	if u.CompletionTokens != 9 || u.TotalTokens != 1209 {
		t.Fatalf("completion/total wrong: %+v", u)
	}
}

func TestUnit_OpenAICache_ResponsesUsageIncludesCachedAndWriteTokens(t *testing.T) {
	body := `{"input_tokens":1500,"output_tokens":20,"total_tokens":1520,
		"input_tokens_details":{"cached_tokens":1408},"cache_write_tokens":64}`
	var u openAIResponsesUsage
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatal(err)
	}
	nu := u.neutralUsage()
	if nu.PromptTokens != 1500 || nu.CacheReadTokens != 1408 || nu.CacheWriteTokens != 64 {
		t.Fatalf("responses usage extraction wrong: %+v", nu)
	}
}
