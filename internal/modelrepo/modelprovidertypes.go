package modelrepo

import "context"

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

// Message now supports OpenAI-style tool calling:
// - assistant messages can carry tool_calls
// - tool messages can carry tool_call_id
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Thinking contains the model's internal reasoning trace (thinking tokens).
	// Only populated when thinking is enabled. Never sent back to the model.
	Thinking string `json:"thinking,omitempty"`

	// For tool calling (OpenAI / vLLM compatible).
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
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

type ChatConfig struct {
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	Seed        *int     `json:"seed,omitempty"`
	Tools       []Tool   `json:"tools,omitempty"`
	// Think controls reasoning-model behaviour. nil = use provider default (off).
	// Accepts provider-specific levels such as "none", "minimal", "low",
	// "medium", "high", and "xhigh" where supported.
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
