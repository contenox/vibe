package gemini

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
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
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
