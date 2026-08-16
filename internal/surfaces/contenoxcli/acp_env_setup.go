package contenoxcli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/contenox/contenox/internal/models/backendservice"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
)

// Environment variables of the non-interactive ACP setup route.
const (
	envDefaultModel       = "CONTENOX_DEFAULT_MODEL"
	envDefaultProvider    = "CONTENOX_DEFAULT_PROVIDER"
	envDefaultAltModel    = "CONTENOX_DEFAULT_ALT_MODEL"
	envDefaultAltProvider = "CONTENOX_DEFAULT_ALT_PROVIDER"
	envDefaultMaxTokens   = "CONTENOX_DEFAULT_MAX_TOKENS"
	envDefaultThink       = "CONTENOX_DEFAULT_THINK"
	// envBaseURL supplies the endpoint URL for account-specific providers whose
	// URL cannot be defaulted.
	envBaseURL = "CONTENOX_BASE_URL"
)

// configValueWithEnv reads a global config value with environment-first
// precedence, without persisting anything.
func configValueWithEnv(ctx context.Context, db libdb.DBManager, key, envKey string) string {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v
	}
	return acpsvc.ReadConfigValue(ctx, db, key)
}

// acpEnvSetupVars is the variable list advertised via the ACP env_var auth
// method.
func acpEnvSetupVars() []libacp.AuthEnvVar {
	notSecret := false
	providerKeys := setupProviderKeys()
	vars := []libacp.AuthEnvVar{
		{Name: envDefaultProvider, Label: "Provider (" + strings.Join(providerKeys, ", ") + ")", Secret: &notSecret},
		{Name: envDefaultModel, Label: "Model (defaults to the provider's default model)", Secret: &notSecret, Optional: true},
	}
	for _, sp := range setupProviders {
		if sp.needsAPIKey {
			vars = append(vars, libacp.AuthEnvVar{Name: sp.envKey, Label: sp.label + " API key", Optional: true})
		}
	}
	// envBaseURL is honored by completeEnvSetup but not advertised here.
	return vars
}

// completeEnvSetup performs the setup wizard's effects non-interactively from
// the environment. Errors name the missing variable.
func completeEnvSetup(ctx context.Context, db libdb.DBManager) error {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv(envDefaultProvider)))
	model := strings.TrimSpace(os.Getenv(envDefaultModel))
	if provider == "" {
		if model == "" {
			return fmt.Errorf("set %s (and optionally %s)", envDefaultProvider, envDefaultModel)
		}
		return fmt.Errorf("%s is set but %s is missing", envDefaultModel, envDefaultProvider)
	}

	var sp *setupProvider
	for i := range setupProviders {
		if setupProviders[i].key == provider {
			sp = &setupProviders[i]
			break
		}
	}
	if sp == nil {
		return fmt.Errorf("unknown provider %q in %s (choose one of: %s)", provider, envDefaultProvider, strings.Join(setupProviderKeys(), ", "))
	}

	if model == "" {
		model = sp.defaultModel
	}
	if model == "" {
		return fmt.Errorf("set %s: provider %q has no default model", envDefaultModel, sp.key)
	}

	apiKey := ""
	if sp.needsAPIKey {
		apiKey = strings.TrimSpace(os.Getenv(sp.envKey))
		if apiKey == "" && !backendExists(ctx, db, sp.key) {
			return fmt.Errorf("set %s: provider %q needs an API key", sp.envKey, sp.key)
		}
	}

	baseURL := ""
	if sp.needsBaseURL {
		baseURL = strings.TrimSpace(os.Getenv(envBaseURL))
		if baseURL == "" && !backendExists(ctx, db, sp.key) {
			return fmt.Errorf("set %s: provider %q needs an endpoint URL", envBaseURL, sp.key)
		}
	}

	if err := registerSetupBackend(ctx, db, sp.key, apiKey, baseURL); err != nil {
		return err
	}

	store := runtimetypes.New(db.WithoutTransaction())
	if err := clikv.WriteConfig(ctx, store, "", "default-provider", sp.key); err != nil {
		return fmt.Errorf("persist default-provider: %w", err)
	}
	if model != "" {
		if err := clikv.WriteConfig(ctx, store, "", "default-model", model); err != nil {
			return fmt.Errorf("persist default-model: %w", err)
		}
	}
	return nil
}

// backendExists reports whether a backend of the given provider type is already
// registered.
func backendExists(ctx context.Context, db libdb.DBManager, providerType string) bool {
	svc := backendservice.New(db)
	backends, err := svc.List(ctx, nil, 100)
	if err != nil {
		return false
	}
	for _, b := range backends {
		if strings.EqualFold(b.Type, providerType) {
			return true
		}
	}
	return false
}
