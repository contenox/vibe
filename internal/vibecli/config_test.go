package vibecli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_resolveEffectiveBackends(t *testing.T) {
	tests := []struct {
		name             string
		cfg              localConfig
		effectiveOllama   string
		effectiveModel    string
		wantDefaultProv  string
		wantDefaultModel string
		checkBackends    func(t *testing.T, got []resolvedBackend)
	}{
		{
			name:             "empty backends legacy ollama",
			cfg:              localConfig{},
			effectiveOllama:  "http://localhost:11434",
			effectiveModel:   "phi3:3.8b",
			wantDefaultProv:  "ollama",
			wantDefaultModel: "phi3:3.8b",
			checkBackends: func(t *testing.T, got []resolvedBackend) {
				require.Len(t, got, 1)
				assert.Equal(t, "default", got[0].name)
				assert.Equal(t, "ollama", got[0].typ)
				assert.Equal(t, "http://localhost:11434", got[0].baseURL)
				assert.Empty(t, got[0].apiKey)
			},
		},
		{
			name: "single backend from config",
			cfg: localConfig{
				Backends:        []backendEntry{{Name: "my-ollama", Type: "ollama", BaseURL: "http://127.0.0.1:11434"}},
				DefaultProvider: "ollama",
				DefaultModel:    "phi3:3.8b",
			},
			effectiveOllama:  "http://ignored:11434",
			effectiveModel:   "ignored",
			wantDefaultProv:  "ollama",
			wantDefaultModel: "phi3:3.8b",
			checkBackends: func(t *testing.T, got []resolvedBackend) {
				require.Len(t, got, 1)
				assert.Equal(t, "my-ollama", got[0].name)
				assert.Equal(t, "ollama", got[0].typ)
				assert.Equal(t, "http://127.0.0.1:11434", got[0].baseURL)
			},
		},
		{
			name: "default provider from first backend when empty",
			cfg: localConfig{
				Backends: []backendEntry{
					{Name: "openai", Type: "openai", BaseURL: "https://api.openai.com", APIKey: "sk-x"},
				},
			},
			effectiveOllama:  "http://x:11434",
			effectiveModel:   "gpt-4",
			wantDefaultProv:  "openai",
			wantDefaultModel: "gpt-4",
			checkBackends: func(t *testing.T, got []resolvedBackend) {
				require.Len(t, got, 1)
				assert.Equal(t, "openai", got[0].name)
				assert.Equal(t, "openai", got[0].typ)
				assert.Equal(t, "sk-x", got[0].apiKey)
			},
		},
		{
			name: "type normalized to lowercase empty becomes ollama",
			cfg: localConfig{
				Backends: []backendEntry{
					{Name: "", Type: "OLLAMA", BaseURL: "http://a:11434"},
				},
			},
			effectiveOllama:  "http://x:11434",
			effectiveModel:   "m",
			wantDefaultProv:  "ollama",
			wantDefaultModel: "m",
			checkBackends: func(t *testing.T, got []resolvedBackend) {
				require.Len(t, got, 1)
				assert.Equal(t, "backend-ollama", got[0].name)
				assert.Equal(t, "ollama", got[0].typ)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, defaultProv, defaultModel := resolveEffectiveBackends(tt.cfg, tt.effectiveOllama, tt.effectiveModel)
			assert.Equal(t, tt.wantDefaultProv, defaultProv)
			assert.Equal(t, tt.wantDefaultModel, defaultModel)
			if tt.checkBackends != nil {
				tt.checkBackends(t, got)
			}
		})
	}
}

func Test_resolveEffectiveBackends_apiKeyFromEnv(t *testing.T) {
	const envKey = "VIBE_TEST_OPENAI_KEY"
	os.Setenv(envKey, "env-secret")
	t.Cleanup(func() { os.Unsetenv(envKey) })

	cfg := localConfig{
		Backends: []backendEntry{
			{Name: "openai", Type: "openai", BaseURL: "https://api.openai.com", APIKeyFromEnv: envKey},
		},
	}
	got, _, _ := resolveEffectiveBackends(cfg, "http://x:11434", "m")
	require.Len(t, got, 1)
	assert.Equal(t, "env-secret", got[0].apiKey)
}

func Test_loadLocalConfig_noFile(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	cfg, path, err := loadLocalConfig()
	require.NoError(t, err)
	assert.Empty(t, path)
	assert.Empty(t, cfg.DefaultChain)
}

func Test_loadLocalConfig_validYAMLInCwd(t *testing.T) {
	dir := t.TempDir()
	contenox := filepath.Join(dir, ".contenox")
	require.NoError(t, os.MkdirAll(contenox, 0750))
	configPath := filepath.Join(contenox, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
default_chain: my-chain.json
ollama: http://custom:11434
model: custom-model
default_provider: ollama
default_model: custom-model
`), 0644))

	orig, _ := os.Getwd()
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	cfg, path, err := loadLocalConfig()
	require.NoError(t, err)
	assert.Equal(t, configPath, path)
	assert.Equal(t, "my-chain.json", cfg.DefaultChain)
	assert.Equal(t, "http://custom:11434", cfg.Ollama)
	assert.Equal(t, "custom-model", cfg.Model)
	assert.Equal(t, "ollama", cfg.DefaultProvider)
	assert.Equal(t, "custom-model", cfg.DefaultModel)
}

func Test_loadLocalConfig_invalidYAML(t *testing.T) {
	dir := t.TempDir()
	contenox := filepath.Join(dir, ".contenox")
	require.NoError(t, os.MkdirAll(contenox, 0750))
	configPath := filepath.Join(contenox, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("not: valid: yaml: here"), 0644))

	orig, _ := os.Getwd()
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	_, _, err := loadLocalConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config.yaml")
}
