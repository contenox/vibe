// config.go holds .contenox config types and resolution (load, backends, default provider/model).
package vibecli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// extraModelEntry is one entry under extra_models in config (name + context required; capabilities optional).
type extraModelEntry struct {
	Name      string `yaml:"name"`
	Context   int    `yaml:"context"`
	CanChat   *bool  `yaml:"can_chat"`
	CanPrompt *bool  `yaml:"can_prompt"`
	CanEmbed  *bool  `yaml:"can_embed"`
}

// backendEntry is one backend in .contenox/config.yaml (backends list).
type backendEntry struct {
	Name          string `yaml:"name"`
	Type          string `yaml:"type"` // ollama | openai | vllm | gemini
	BaseURL       string `yaml:"base_url"`
	APIKey        string `yaml:"api_key,omitempty"`
	APIKeyFromEnv string `yaml:"api_key_from_env,omitempty"`
}

// localConfig holds optional values from .contenox/config.yaml (flags override).
type localConfig struct {
	DefaultChain             string            `yaml:"default_chain"`
	DB                       string            `yaml:"db"`
	Ollama                   string            `yaml:"ollama"`
	Model                    string            `yaml:"model"`
	Backends                 []backendEntry    `yaml:"backends"`
	DefaultProvider          string            `yaml:"default_provider"`
	DefaultModel             string            `yaml:"default_model"`
	Context                  *int              `yaml:"context"`
	NoDeleteModels           *bool             `yaml:"no_delete_models"`
	EnableLocalExec          *bool             `yaml:"enable_local_exec"`
	LocalExecAllowedDir      string            `yaml:"local_exec_allowed_dir"`
	LocalExecAllowedCommands string            `yaml:"local_exec_allowed_commands"`
	LocalExecDeniedCommands  []string          `yaml:"local_exec_denied_commands"`
	ExtraModels              []extraModelEntry `yaml:"extra_models"`
	Tracing                  *bool             `yaml:"tracing"`
	Steps                    *bool             `yaml:"steps"`
	Raw                      *bool             `yaml:"raw"`
	TemplateVarsFromEnv      []string          `yaml:"template_vars_from_env"`
}

// resolvedBackend is the normalized backend spec used for ensure-backends (name, type, base_url, optional api_key for cloud).
type resolvedBackend struct {
	name    string
	typ     string
	baseURL string
	apiKey  string // for openai/gemini only
}

// resolveEffectiveBackends returns the list of backends to ensure and the default provider/model.
// Legacy: when cfg.Backends is empty, derives one ollama backend from effectiveOllama and effectiveModel.
func resolveEffectiveBackends(cfg localConfig, effectiveOllama, effectiveModel string) ([]resolvedBackend, string, string) {
	if len(cfg.Backends) == 0 {
		return []resolvedBackend{
			{name: "default", typ: "ollama", baseURL: effectiveOllama, apiKey: ""},
		}, "ollama", effectiveModel
	}
	out := make([]resolvedBackend, 0, len(cfg.Backends))
	for _, b := range cfg.Backends {
		typ := strings.ToLower(strings.TrimSpace(b.Type))
		if typ == "" {
			typ = "ollama"
		}
		name := strings.TrimSpace(b.Name)
		if name == "" {
			name = "backend-" + typ
		}
		baseURL := strings.TrimSpace(b.BaseURL)
		apiKey := strings.TrimSpace(b.APIKey)
		if apiKey == "" && b.APIKeyFromEnv != "" {
			apiKey = os.Getenv(strings.TrimSpace(b.APIKeyFromEnv))
		}
		out = append(out, resolvedBackend{name: name, typ: typ, baseURL: baseURL, apiKey: apiKey})
	}
	defaultProvider := strings.ToLower(strings.TrimSpace(cfg.DefaultProvider))
	defaultModel := strings.TrimSpace(cfg.DefaultModel)
	if defaultProvider == "" && len(out) > 0 {
		defaultProvider = out[0].typ
	}
	if defaultModel == "" {
		defaultModel = effectiveModel
	}
	return out, defaultProvider, defaultModel
}

// loadLocalConfig tries ./.contenox/config.yaml then ~/.contenox/config.yaml.
// Returns (config, pathToConfigFile, nil). If neither file exists, returns (empty, "", nil).
func loadLocalConfig() (localConfig, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return localConfig{}, "", err
	}
	try := []string{
		filepath.Join(cwd, ".contenox", "config.yaml"),
	}
	if home, err := os.UserHomeDir(); err == nil {
		try = append(try, filepath.Join(home, ".contenox", "config.yaml"))
	}
	for _, p := range try {
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return localConfig{}, "", err
		}
		var cfg localConfig
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return localConfig{}, "", fmt.Errorf("%s: %w", p, err)
		}
		return cfg, p, nil
	}
	return localConfig{}, "", nil
}
