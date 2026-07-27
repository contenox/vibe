package gemini

import "github.com/contenox/beam/internal/models/modelrepo"

type geminiToolRequest struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations,omitempty"`
}

// --- Function calls & content parts (messages) ---

type geminiFunctionCall struct {
	Name             string                 `json:"name"`
	Args             map[string]interface{} `json:"args"`
	ThoughtSignature string                 `json:"thoughtSignature,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string                 `json:"name"`
	Response map[string]interface{} `json:"response"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	Thought          bool                    `json:"thought,omitempty"`
	ThoughtSignature string                  `json:"thoughtSignature,omitempty"`
	InlineData       *geminiInlineData       `json:"inlineData,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

// geminiInlineData is an inline binary blob part (e.g. an image) sent in a
// content part. The Gemini v1beta REST JSON is proto3-JSON and accepts the
// lowerCamelCase field names, matching the casing this package already uses for
// functionCall / functionResponse / thoughtSignature. encoding/json base64
// (StdEncoding) encodes the Data []byte on the wire.
type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     []byte `json:"data"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiSystemInstruction struct {
	Parts []geminiPart `json:"parts"`
}

// --- Responses ---

type geminiGenerateContentResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason,omitempty"`
	} `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason,omitempty"`
	} `json:"promptFeedback"`
	UsageMetadata *geminiUsageMetadata `json:"usageMetadata,omitempty"`
}

// geminiUsageMetadata is the API's token accounting, attached to (the last
// chunk of) a generateContent / streamGenerateContent response.
// promptTokenCount is already the TOTAL prompt count (cached included);
// cachedContentTokenCount breaks out the tokens served from Gemini's implicit
// (or explicit) cache.
type geminiUsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
}

// neutralUsage maps usageMetadata onto the neutral TokenUsage. Prompt caching
// on Gemini is IMPLICIT (enabled by default on 2.5+ models, ~90% discount on
// cached tokens, best-effort): nothing is sent on the wire to activate it —
// the client-side contract is byte-stable prefixes plus session affinity, and
// this counter is where hits become visible. Explicit cachedContents
// (guaranteed discount, storage billed per token-hour) is a recorded
// follow-up (blueprint S1b.6) gated on measured implicit hit rates.
func (u *geminiUsageMetadata) neutralUsage() *modelrepo.TokenUsage {
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

// geminiFunctionDeclaration matches Gemini API's FunctionDeclaration exactly
// https://ai.google.dev/gemini-api/docs/function-calling [[15]]
type geminiFunctionDeclaration struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Parameters  *geminiSchema `json:"parameters,omitempty"`
}

// geminiSchema matches Gemini API's Schema object exactly
// Only these fields are valid - anything else gets dropped on marshal [[15]]
type geminiSchema struct {
	Type        string         `json:"type"`
	Description string         `json:"description,omitempty"`
	Enum        []any          `json:"enum,omitempty"`
	Items       *geminiSchema  `json:"items,omitempty"`
	Properties  map[string]any `json:"properties,omitempty"`
	Required    []string       `json:"required,omitempty"`
	Nullable    *bool          `json:"nullable,omitempty"`
}
