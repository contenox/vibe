package modelrepo

import (
	"context"
	"errors"
	"time"
)

// ErrRefused is returned when the model refuses to generate a response
// (stop_reason == "refusal"), typically due to a safety filter.
var ErrRefused = errors.New("model refused the request")

type ChatResult struct {
	Message   Message
	ToolCalls []ToolCall
	// Usage is the provider-reported token accounting for this non-streaming
	// call (streaming reports usage on the terminal StreamParcel instead).
	// nil means the provider did not report usage.
	Usage *TokenUsage
}

type ToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"` // only "function" for now
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	// ProviderMeta carries opaque provider-specific data that must be
	// round-tripped back on the next turn (e.g. Gemini thought_signature).
	ProviderMeta map[string]string `json:"provider_meta,omitempty"`
}

// ImagePart is a binary image attachment carried beside a message's text
// content. Data holds the raw image bytes (encoding/json base64-encodes
// []byte on the wire); MimeType is the image media type, e.g. "image/png".
type ImagePart struct {
	Data     []byte `json:"data"`
	MimeType string `json:"mime_type"`
}

// Message now supports OpenAI-style tool calling:
// - assistant messages can carry tool_calls
// - tool messages can carry tool_call_id
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Images carries image attachments beside Content. The resolver routes
	// image-bearing requests only to providers reporting CanVision; providers
	// without vision support never receive messages with images.
	Images []ImagePart `json:"images,omitempty"`
	// Thinking contains the model's internal reasoning trace (thinking tokens).
	// Only populated when thinking is enabled. Never sent back to the model.
	Thinking string `json:"thinking,omitempty"`

	// For tool calling (OpenAI / vLLM compatible).
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// MessagesHaveImages reports whether any message carries an image attachment.
// Callers use it to derive the vision requirement for resolution instead of
// setting a flag by hand.
func MessagesHaveImages(messages []Message) bool {
	for _, m := range messages {
		if len(m.Images) > 0 {
			return true
		}
	}
	return false
}

type ChatArgument interface {
	Apply(config *ChatConfig)
}

// ToolCallDelta is one raw streamed tool-call fragment. Providers translate
// their wire format into these fragments and never assemble them; assembly is
// engine policy and happens exactly once, in StreamAssembler.
//
// Index groups the fragments of one call: every fragment of the same call
// carries the same Index, and distinct calls in one response carry distinct
// Indexes. Providers whose wire format has no index (they deliver each call
// whole) assign sequential indexes in arrival order.
//
// ID, Type, and Name are atomic fields: each is set on at most one fragment
// per index (conventionally the first). ArgsFragment carries a piece of the
// argument JSON; fragments are concatenated in arrival order.
type ToolCallDelta struct {
	Index        int
	ID           string
	Type         string
	Name         string
	ArgsFragment string
	// ProviderMeta carries opaque provider-specific data that must round-trip
	// on the next turn (e.g. Gemini thought_signature).
	ProviderMeta map[string]string
}

// TokenUsage is provider-reported token accounting. Zero fields mean
// "not reported"; the assembler merges later reports over earlier ones
// field-wise.
//
// Normalization rule (provider-kv-cache blueprint §4.4): PromptTokens is the
// TOTAL prompt token count on every provider, cached or not. Providers whose
// wire format reports only the uncached remainder (Anthropic input_tokens,
// Bedrock inputTokens) must add their cache read/write counts back in before
// populating this struct; providers whose prompt count already includes cached
// tokens (OpenAI prompt_tokens, Gemini/Vertex promptTokenCount, ollama
// prompt_eval_count) pass it through unchanged.
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CacheReadTokens is the number of prompt tokens served from the
	// provider's prompt/prefix cache (anthropic cache_read_input_tokens,
	// openai prompt_tokens_details.cached_tokens / input_tokens_details
	// .cached_tokens, bedrock cacheReadInputTokens, gemini/vertex
	// usageMetadata.cachedContentTokenCount). ollama/vllm report no cache
	// dimension today and leave it zero.
	CacheReadTokens int
	// CacheWriteTokens is the number of prompt tokens written to the
	// provider's cache this request (anthropic cache_creation_input_tokens,
	// bedrock cacheWriteInputTokens, openai gpt-5.6+ cache_write_tokens).
	// Zero elsewhere.
	CacheWriteTokens int
}

// StreamTerminal is the typed terminal event of a stream: the provider's
// finish reason (verbatim wire value, e.g. "stop", "tool_calls", "length",
// "content_filter", "MAX_TOKENS") plus final usage when the provider reports
// it there.
type StreamTerminal struct {
	FinishReason string
	Usage        *TokenUsage
}

// StreamParcel is ONE raw delta from a provider stream. The contract is exact:
//
//   - Exactly one of Data, Thinking, ToolCall, Usage, Terminal, or Error is
//     populated per parcel.
//   - Providers emit raw deltas only. They never accumulate content, never
//     assemble tool calls, and never reorder events; assembly is engine
//     policy (StreamAssembler).
//   - A successful stream ends with exactly one Terminal parcel, after which
//     the channel closes. Nothing follows a Terminal parcel.
//   - A failed stream ends with an Error parcel (in-stream provider error
//     events surface here too) and no Terminal.
type StreamParcel struct {
	// Data is a visible output-text delta.
	Data string
	// Thinking carries a streamed reasoning/thinking delta separate from the
	// visible output text. Like Message.Thinking, it is provider-facing output
	// and must never be sent back as conversation history.
	Thinking string
	// ToolCall is one raw tool-call fragment (see ToolCallDelta).
	ToolCall *ToolCallDelta
	// Usage is a provider usage report emitted mid-stream (some providers
	// report prompt tokens at stream start and completion tokens at the end).
	Usage *TokenUsage
	// Terminal is the typed end-of-stream event: finish reason + final usage.
	Terminal *StreamTerminal
	Error    error
}

type Tool struct {
	Type     string        `json:"type"`
	Function *FunctionTool `json:"function,omitempty"`
}

type FunctionTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
}

// CacheHints tells a provider where the stable/volatile boundary of a request
// lies so it can place native cache controls (anthropic cache_control,
// bedrock cachePoint, openai prompt_cache_key). Providers map what they can
// and ignore the rest — omission changes nothing, and a hint may NEVER change
// what the model sees: only cache metadata differs between a hinted and an
// unhinted request. Any provider that would have to reorder or rewrite
// content to honor a hint must drop the hint instead.
type CacheHints struct {
	// SessionKey is an opaque per-session cache-affinity key (already hashed;
	// never a raw internal ID). OpenAI sends it as prompt_cache_key; other
	// providers ignore it (vLLM keys on prefix bytes server-side, so the key
	// is deliberately not sent there).
	SessionKey string
	// StableSystem asserts the system instruction is byte-stable across the
	// session, i.e. it is safe to place a cache breakpoint after it.
	StableSystem bool
	// StableTools asserts the tool list (order and encoding) is byte-stable
	// across the session.
	StableTools bool
	// StableHistoryLen is the number of leading history messages asserted
	// unchanged since the previous request of this session (0 = no
	// assertion). Providers with explicit breakpoints may mark the last of
	// these.
	StableHistoryLen int
	// TTL is an advisory cache lifetime for providers with explicit TTLs
	// (anthropic/bedrock support 5m and 1h tiers). Zero means the provider
	// default.
	TTL time.Duration
}

type ChatConfig struct {
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	Seed        *int     `json:"seed,omitempty"`
	Tools       []Tool   `json:"tools,omitempty"`
	// Think controls reasoning-model behaviour. nil = use provider default.
	// Normalized values are auto, off, minimal, low, medium, high, and xhigh.
	Think *string `json:"think,omitempty"`
	// Shift instructs the provider to slide the context window on overflow
	// instead of returning a token-limit error.
	Shift *bool `json:"shift,omitempty"`
	// Truncate instructs the provider to truncate history on overflow.
	Truncate *bool `json:"truncate,omitempty"`
	// CacheHints declares the stable/volatile boundary of this request for
	// provider-side prompt caching. nil changes nothing. Never serialized:
	// each adapter maps it onto its own wire format's cache metadata.
	CacheHints *CacheHints `json:"-"`
}

// WithThink is a ChatArgument that enables/controls reasoning mode.
type WithThink string

func (w WithThink) Apply(cfg *ChatConfig) {
	s := string(w)
	cfg.Think = &s
}

// WithShift is a ChatArgument that enables context shift on overflow.
type WithShift struct{}

func (WithShift) Apply(cfg *ChatConfig) {
	t := true
	cfg.Shift = &t
}

// Client interfaces
type LLMChatClient interface {
	Chat(ctx context.Context, messages []Message, args ...ChatArgument) (ChatResult, error)
}

type LLMEmbedClient interface {
	Embed(ctx context.Context, prompt string) ([]float64, error)
}

type LLMStreamClient interface {
	Stream(ctx context.Context, messages []Message, args ...ChatArgument) (<-chan *StreamParcel, error)
}

type LLMPromptExecClient interface {
	Prompt(ctx context.Context, systemInstruction string, temperature float32, prompt string) (string, error)
}
