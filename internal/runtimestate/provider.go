package runtimestate

import "encoding/json"

const (
	ProviderKeyPrefix = "cloud-provider:"
	OllamaKey         = ProviderKeyPrefix + "ollama"
	OpenaiKey         = ProviderKeyPrefix + "openai"
	GeminiKey         = ProviderKeyPrefix + "gemini"
)

type ProviderConfig struct {
	APIKey string
	Type   string
}

func (pc ProviderConfig) MarshalJSON() ([]byte, error) {
	type Alias ProviderConfig

	maskedConfig := struct {
		APIKey string `json:"APIKey"`
		Type   string `json:"Type"`
	}{
		APIKey: pc.APIKey, // TODO: Implement encryption here
		Type:   pc.Type,
	}

	return json.Marshal(maskedConfig)
}
