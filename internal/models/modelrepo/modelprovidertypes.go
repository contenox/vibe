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
	// Usage is the provider-reported token accounting for this non-streaming call; nil means unreported.
	Usage *TokenUsage
	// FinishReason is the provider's raw finish reason, verbatim, or empty if unreported.
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

// ImagePart is a binary image attachment carried beside a message's text; Data holds raw bytes, MimeType the media type.
type ImagePart struct {
	Data     []byte `json:"data"`
	MimeType string `json:"mime_type"`
}

// AudioPart is a binary audio attachment carried beside a message's text, shaped like ImagePart.
type AudioPart struct {
	Data     []byte `json:"data"`
	MimeType string `json:"mime_type"`
}

// Message is a chat turn; assistant messages may carry ToolCalls, tool
// messages carry ToolCallID (OpenAI/vLLM-compatible tool calling).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Images carries image attachments beside Content; routed only to providers reporting CanVision.
	Images []ImagePart `json:"images,omitempty"`
	// Audio carries audio attachments beside Content; routed only to providers reporting CanAudio.
	Audio []AudioPart `json:"audio,omitempty"`
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

// MessagesHaveAudio reports whether any message carries an audio attachment,
// for deriving the audio requirement at resolution time.
func MessagesHaveAudio(messages []Message) bool {
	for _, m := range messages {
		if len(m.Audio) > 0 {
			return true
		}
	}
	return false
}

type ChatArgument interface {
	Apply(config *ChatConfig)
}

// ToolCallDelta is one raw streamed tool-call fragment; fragments sharing an Index belong to the same call and assemble via StreamAssembler.
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

// TokenUsage is provider-reported token accounting; zero fields mean not reported, and PromptTokens is always the total prompt count including cached tokens.
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CacheReadTokens is prompt tokens served from the provider's prefix cache; zero where unsupported.
	CacheReadTokens int
	// CacheWriteTokens is prompt tokens written to the provider's cache this request; zero where unsupported.
	CacheWriteTokens int
}

// StreamTerminal is the typed terminal event of a stream: the provider's
// verbatim finish reason plus final usage when reported there.
type StreamTerminal struct {
	FinishReason string
	Usage        *TokenUsage
}

// StreamParcel is one raw provider-stream delta, with exactly one field populated per parcel; a stream ends with one Terminal parcel or one Error parcel.
type StreamParcel struct {
	// Data is a visible output-text delta.
	Data string
	// Thinking is a streamed reasoning delta, separate from Data; never sent back as conversation history.
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

// CacheHints tells a provider where a request's stable/volatile boundary lies for native cache controls; a hint must never change what the model sees, only cache metadata.
type CacheHints struct {
	// SessionKey is an opaque, already-hashed per-session cache-affinity key; never a raw internal ID.
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
	// TTL is an advisory cache lifetime for providers with explicit TTLs; zero means the provider default.
	TTL time.Duration
}

type ChatConfig struct {
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	Seed        *int     `json:"seed,omitempty"`
	Tools       []Tool   `json:"tools,omitempty"`
	// Think controls reasoning-model behaviour; nil uses the provider default (auto, off, minimal, low, medium, high, xhigh).
	Think *string `json:"think,omitempty"`
	// Shift instructs the provider to slide the context window on overflow
	// instead of returning a token-limit error.
	Shift *bool `json:"shift,omitempty"`
	// Truncate instructs the provider to truncate history on overflow.
	Truncate *bool `json:"truncate,omitempty"`
	// CacheHints declares this request's stable/volatile boundary for provider-side prompt caching; nil changes nothing, never serialized.
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
