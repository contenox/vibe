package statetype

import (
	"time"

	"github.com/contenox/vibe/runtimetypes"
	"github.com/ollama/ollama/api"
)

// BackendRuntimeState represents the observed state of a single LLM backend.
type BackendRuntimeState struct {
	ID           string               `json:"id" example:"b7d9e1a3-8f0c-4a7d-9b1e-2f3a4b5c6d7e"`
	Name         string               `json:"name" example:"ollama-production"`
	Models       []string             `json:"models" example:"[\"mistral:instruct\", \"llama2:7b\", \"nomic-embed-text:latest\"]"`
	PulledModels []ModelPullStatus    `json:"pulledModels" openapi_include_type:"statetype.ModelPullStatus"`
	Backend      runtimetypes.Backend `json:"backend"`
	// Error stores a description of the last encountered error when
	// interacting with or reconciling this backend's state, if any.
	Error string `json:"error,omitempty" example:"connection timeout: context deadline exceeded"`
	// APIKey stores the API key used for authentication with the backend.
	apiKey string
}

type ModelPullStatus struct {
	Name          string       `json:"name" example:"Mistral 7B Instruct"`
	Model         string       `json:"model" example:"mistral:instruct"`
	ModifiedAt    time.Time    `json:"modifiedAt" example:"2023-11-15T14:30:45Z"`
	Size          int64        `json:"size" example:"4709611008"`
	Digest        string       `json:"digest" example:"sha256:9e3a6c0d3b5e7f8a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a"`
	Details       ModelDetails `json:"details" openapi_include_type:"statetype.ModelDetails"`
	ContextLength int          `json:"contextLength" example:"4096"`
	CanChat       bool         `json:"canChat" example:"true"`
	CanEmbed      bool         `json:"canEmbed" example:"false"`
	CanPrompt     bool         `json:"canPrompt" example:"true"`
	CanStream     bool         `json:"canStream" example:"true"`
}

type ModelDetails struct {
	ParentModel       string   `json:"parentModel" example:"mistral:7b"`
	Format            string   `json:"format" example:"gguf"`
	Family            string   `json:"family" example:"Mistral"`
	Families          []string `json:"families" example:"[\"Mistral\", \"7B\"]"`
	ParameterSize     string   `json:"parameterSize" example:"7B"`
	QuantizationLevel string   `json:"quantizationLevel" example:"Q4_K_M"`
}

func (s *BackendRuntimeState) GetAPIKey() string {
	return s.apiKey
}

func (s *BackendRuntimeState) SetAPIKey(key string) {
	s.apiKey = key
}

func ConvertOllamaModelResponse(model *api.ListModelResponse) *ModelPullStatus {
	list := &ModelPullStatus{
		Name:       model.Name,
		Model:      model.Model,
		ModifiedAt: model.ModifiedAt,
		Size:       model.Size,
		Digest:     model.Digest,
		Details: ModelDetails{
			ParentModel:       model.Details.ParentModel,
			Format:            model.Details.Format,
			Family:            model.Details.Family,
			Families:          model.Details.Families,
			ParameterSize:     model.Details.ParameterSize,
			QuantizationLevel: model.Details.QuantizationLevel,
		},
	}
	return list
}
