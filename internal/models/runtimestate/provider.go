package runtimestate

import "encoding/json"

const (
	ProviderKeyPrefix = "cloud-provider:"
	OllamaKey         = ProviderKeyPrefix + "ollama"
	OpenaiKey         = ProviderKeyPrefix + "openai"
	OpenRouterKey     = ProviderKeyPrefix + "openrouter"
	AnthropicKey      = ProviderKeyPrefix + "anthropic"
	MistralKey        = ProviderKeyPrefix + "mistral"
	BedrockKey        = ProviderKeyPrefix + "bedrock"
	GeminiKey         = ProviderKeyPrefix + "gemini"
	VertexGoogleKey   = ProviderKeyPrefix + "vertex-google"
)

type ProviderConfig struct {
	APIKey    string
	APIKeyEnv string
	Type      string
}

func (pc ProviderConfig) MarshalJSON() ([]byte, error) {
	type Alias ProviderConfig

	maskedConfig := struct {
		APIKey    string `json:"APIKey"`
		APIKeyEnv string `json:"APIKeyEnv,omitempty"`
		Type      string `json:"Type"`
	}{
		APIKey:    pc.APIKey, // Stored locally in SQLite; HTTP provider APIs return sanitized views.
		APIKeyEnv: pc.APIKeyEnv,
		Type:      pc.Type,
	}

	return json.Marshal(maskedConfig)
}
