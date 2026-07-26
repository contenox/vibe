package modelrepo

import (
	"context"
	"errors"
)

// ErrRefused is returned when the model refuses to generate a response
// (stop_reason == "refusal"), typically due to a safety filter.
var ErrRefused = errors.New("model refused the request")

type ChatResult struct {
	Message   Message
	ToolCalls []ToolCall
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

type StreamParcel struct {
	Data string
	// Thinking carries a streamed reasoning/thinking delta separate from the
	// visible output text. Like Message.Thinking, it is provider-facing output
	// and must never be sent back as conversation history.
	Thinking string
	// ToolCalls carries final structured tool-call output for providers that can
	// assemble tool calls from a stream. It is normally emitted on a terminal
	// parcel, not token-by-token.
	ToolCalls []ToolCall
	Error     error
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
