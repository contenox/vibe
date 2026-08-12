package vertex

import "github.com/contenox/contenox/internal/models/modelrepo"

type vertexRequest struct {
	SystemInstruction *vertexContent          `json:"system_instruction,omitempty"`
	Contents          []vertexContent         `json:"contents"`
	GenerationConfig  *vertexGenerationConfig `json:"generationConfig,omitempty"`
	Tools             []vertexToolRequest     `json:"tools,omitempty"`
}

type vertexGenerationConfig struct {
	Temperature     *float64              `json:"temperature,omitempty"`
	TopP            *float64              `json:"topP,omitempty"`
	MaxOutputTokens *int                  `json:"maxOutputTokens,omitempty"`
	Seed            *int                  `json:"seed,omitempty"`
	ThinkingConfig  *vertexThinkingConfig `json:"thinkingConfig,omitempty"`
}

type vertexThinkingConfig struct {
	ThinkingBudget *int   `json:"thinkingBudget,omitempty"`
	ThinkingLevel  string `json:"thinkingLevel,omitempty"`
}

type vertexToolRequest struct {
	FunctionDeclarations []vertexFunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type vertexFunctionDeclaration struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Parameters  *vertexSchema `json:"parameters,omitempty"`
}

type vertexSchema struct {
	Type        string         `json:"type"`
	Description string         `json:"description,omitempty"`
	Enum        []any          `json:"enum,omitempty"`
	Items       *vertexSchema  `json:"items,omitempty"`
	Properties  map[string]any `json:"properties,omitempty"`
	Required    []string       `json:"required,omitempty"`
	Nullable    *bool          `json:"nullable,omitempty"`
}

type vertexContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []vertexPart `json:"parts"`
}

type vertexPart struct {
	Text             string                  `json:"text,omitempty"`
	Thought          bool                    `json:"thought,omitempty"`
	ThoughtSignature string                  `json:"thoughtSignature,omitempty"`
	InlineData       *vertexInlineData       `json:"inlineData,omitempty"`
	FunctionCall     *vertexFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *vertexFunctionResponse `json:"functionResponse,omitempty"`
}

type vertexInlineData struct {
	MimeType string `json:"mimeType"`
	Data     []byte `json:"data"`
}

type vertexFunctionCall struct {
	Name             string                 `json:"name"`
	Args             map[string]interface{} `json:"args"`
	ThoughtSignature string                 `json:"thoughtSignature,omitempty"`
}

type vertexFunctionResponse struct {
	Name     string                 `json:"name"`
	Response map[string]interface{} `json:"response"`
}

type vertexResponse struct {
	Candidates []struct {
		Content      vertexContent `json:"content"`
		FinishReason string        `json:"finishReason,omitempty"`
	} `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason,omitempty"`
	} `json:"promptFeedback"`
	UsageMetadata *vertexUsageMetadata `json:"usageMetadata,omitempty"`
}

type vertexUsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
}

func (u *vertexUsageMetadata) neutralUsage() *modelrepo.TokenUsage {
	if u == nil {
		return nil
	}
	total := u.TotalTokenCount
	if total == 0 {
		total = u.PromptTokenCount + u.CandidatesTokenCount
	}
	return &modelrepo.TokenUsage{
		PromptTokens:     u.PromptTokenCount,
		CompletionTokens: u.CandidatesTokenCount,
		TotalTokens:      total,
		CacheReadTokens:  u.CachedContentTokenCount,
	}
}

type vertexErrorResponse struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}
