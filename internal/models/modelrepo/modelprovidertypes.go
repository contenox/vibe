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
	// call; nil means unreported. Streaming reports usage on StreamParcel.Terminal instead.
	Usage *TokenUsage
	// FinishReason is the provider's raw finish reason, verbatim (openai
	// "length", gemini "MAX_TOKENS", …), or empty if unreported. Normalized
	// downstream by agentservice.InferStopReason.
	FinishReason string
}

type ToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"` // only "function" for now
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	// ProviderMeta carries opaque provider data that must round-trip on the
	// next turn (e.g. Gemini thought_signature).
	ProviderMeta map[string]string `json:"provider_meta,omitempty"`
}

// ImagePart is a binary image attachment carried beside a message's text.
// Data holds the raw bytes; MimeType is the media type, e.g. "image/png".
type ImagePart struct {
	Data     []byte `json:"data"`
	MimeType string `json:"mime_type"`
}

// Message is a chat turn; assistant messages may carry ToolCalls, tool
// messages carry ToolCallID (OpenAI/vLLM-compatible tool calling).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Images carries image attachments beside Content. The resolver routes
	// image-bearing requests only to providers reporting CanVision.
	Images []ImagePart `json:"images,omitempty"`
	// Thinking is the model's reasoning trace, populated only when thinking
	// is enabled, and never sent back to the model.
	Thinking string `json:"thinking,omitempty"`

	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// MessagesHaveImages reports whether any message carries an image
// attachment, for deriving the vision requirement at resolution time.
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
// their wire format into fragments and never assemble them; assembly happens
// exactly once, in StreamAssembler.
//
// Index groups the fragments of one call: every fragment of the same call
// shares an Index, and distinct calls carry distinct Indexes. Providers whose
// wire format delivers each call whole assign sequential indexes in arrival
// order. ID, Type, and Name are atomic: each is set on at most one fragment
// per index. ArgsFragment pieces concatenate in arrival order.
type ToolCallDelta struct {
	Index        int
	ID           string
	Type         string
	Name         string
	ArgsFragment string
	// ProviderMeta carries opaque provider data that must round-trip on the
	// next turn (e.g. Gemini thought_signature).
	ProviderMeta map[string]string
}

// TokenUsage is provider-reported token accounting. Zero fields mean "not
// reported"; the assembler merges later reports over earlier ones field-wise.
//
// Normalization rule: PromptTokens is the total prompt token count on every
// provider, cached or not. Providers reporting only the uncached remainder
// (Anthropic input_tokens, Bedrock inputTokens) must add their cache
// read/write counts back in; providers whose count already includes cached
// tokens (OpenAI, Gemini/Vertex, ollama) pass it through unchanged.
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CacheReadTokens is prompt tokens served from the provider's prefix
	// cache (anthropic cache_read_input_tokens, openai cached_tokens,
	// bedrock cacheReadInputTokens, gemini/vertex cachedContentTokenCount).
	// ollama/vllm report no cache dimension and leave it zero.
	CacheReadTokens int
	// CacheWriteTokens is prompt tokens written to the provider's cache this
	// request (anthropic cache_creation_input_tokens, bedrock
	// cacheWriteInputTokens, openai gpt-5.6+ cache_write_tokens). Zero elsewhere.
	CacheWriteTokens int
}

// StreamTerminal is the typed terminal event of a stream: the provider's
// verbatim finish reason plus final usage when reported there.
type StreamTerminal struct {
	FinishReason string
	Usage        *TokenUsage
}

// StreamParcel is one raw delta from a provider stream:
//
//   - Exactly one of Data, Thinking, ToolCall, Usage, Terminal, or Error is
//     populated per parcel.
//   - Providers emit raw deltas only — never accumulate content, assemble
//     tool calls, or reorder events; assembly is StreamAssembler's job.
//   - A successful stream ends with exactly one Terminal parcel, after which
//     the channel closes.
//   - A failed stream ends with an Error parcel and no Terminal.
type StreamParcel struct {
	// Data is a visible output-text delta.
	Data string
	// Thinking is a streamed reasoning delta, separate from Data. Like
	// Message.Thinking, it must never be sent back as conversation history.
	Thinking string
	ToolCall *ToolCallDelta
	// Usage is a provider usage report emitted mid-stream (some providers
	// report prompt tokens at stream start, completion tokens at the end).
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

// CacheHints tells a provider where the stable/volatile boundary of a
// request lies so it can place native cache controls (anthropic
// cache_control, bedrock cachePoint, openai prompt_cache_key). Providers map
// what they can and ignore the rest — a hint must never change what the
// model sees, only cache metadata. A provider that would have to reorder or
// rewrite content to honor a hint must drop it instead.
type CacheHints struct {
	// SessionKey is an opaque, already-hashed per-session cache-affinity key
	// (never a raw internal ID). OpenAI sends it as prompt_cache_key; vLLM
	// keys on prefix bytes server-side and ignores it.
	SessionKey string
	// StableSystem asserts the system instruction is byte-stable across the
	// session, i.e. safe to place a cache breakpoint after it.
	StableSystem bool
	// StableTools asserts the tool list (order and encoding) is byte-stable
	// across the session.
	StableTools bool
	// StableHistoryLen is the number of leading history messages asserted
	// unchanged since the previous request (0 = no assertion).
	StableHistoryLen int
	// TTL is an advisory cache lifetime for providers with explicit TTLs
	// (anthropic/bedrock support 5m and 1h). Zero means the provider default.
	TTL time.Duration
}

type ChatConfig struct {
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	Seed        *int     `json:"seed,omitempty"`
	Tools       []Tool   `json:"tools,omitempty"`
	// Think controls reasoning-model behaviour; nil uses the provider
	// default. Normalized values: auto, off, minimal, low, medium, high, xhigh.
	Think *string `json:"think,omitempty"`
	// Shift instructs the provider to slide the context window on overflow
	// instead of returning a token-limit error.
	Shift *bool `json:"shift,omitempty"`
	// Truncate instructs the provider to truncate history on overflow.
	Truncate *bool `json:"truncate,omitempty"`
	// CacheHints declares this request's stable/volatile boundary for
	// provider-side prompt caching; nil changes nothing. Never serialized —
	// each adapter maps it onto its own wire format.
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
	// Prompt returns the completion text plus the provider-reported token
	// accounting; nil usage means the provider did not report any.
	Prompt(ctx context.Context, systemInstruction string, temperature float32, prompt string) (string, *TokenUsage, error)
}
