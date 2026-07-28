package stateservice

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/contenox/beam/internal/kernel/reasoning"
	"github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/models/runtimestate"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/services/setupcheck"
	"github.com/contenox/beam/internal/store/runtimetypes"
)

// Service exposes runtime backend state plus onboarding/setup evaluation (same inputs as GET /setup-status).
type Service interface {
	Get(ctx context.Context) ([]runtimestate.BackendRuntimeState, error)
	// SetupStatus returns readiness from KV defaults, registered backends, and current runtime state.
	SetupStatus(ctx context.Context) (setupcheck.Result, error)
	// Refresh reconciles registered backends/models, then returns the updated setup status.
	Refresh(ctx context.Context) (setupcheck.Result, error)
	// CLIConfig returns the current resolved CLI config without mutating it.
	CLIConfig(ctx context.Context) (CLIConfigSnapshot, error)
	// SetCLIConfig updates CLI default keys in SQLite KV (same as contenox config set / PUT /cli-config).
	// Nil fields in the patch are left unchanged. Empty string fields are written and can clear a resolved setting.
	SetCLIConfig(ctx context.Context, patch CLIConfigPatch) (CLIConfigSnapshot, error)
}

// CLIConfigPatch selects which CLI default keys to write; nil means "do not change".
type CLIConfigPatch struct {
	DefaultModel                *string
	DefaultProvider             *string
	DefaultAltModel             *string
	DefaultAltProvider          *string
	DefaultAutocompleteModel    *string
	DefaultAutocompleteProvider *string
	DefaultMaxTokens            *string
	DefaultThink                *string
	DefaultChain                *string
	HITLPolicyName              *string
	TelemetryEnabled            *string
	UpdateCheck                 *string
	// DefaultMissionAgent and DefaultMissionPolicy are the two halves of a
	// fireable `/mission <intent>`: which agent runs it, and the policy that
	// bounds it. Both are global keys; both must be set before /mission works.
	DefaultMissionAgent  *string
	DefaultMissionPolicy *string
}

// CLIConfigSnapshot is the resolved KV values after an update.
type CLIConfigSnapshot struct {
	DefaultModel                string
	DefaultProvider             string
	DefaultAltModel             string
	DefaultAltProvider          string
	DefaultAutocompleteModel    string
	DefaultAutocompleteProvider string
	DefaultMaxTokens            string
	DefaultThink                string
	DefaultChain                string
	HITLPolicyName              string
	TelemetryEnabled            string
	UpdateCheck                 string
	DefaultMissionAgent         string
	DefaultMissionPolicy        string
	ResolvedFrom                map[string]string
	Present                     map[string]bool
}

type service struct {
	state       *runtimestate.State
	db          libdbexec.DBManager
	workspaceID string
}

// Get implements Service.
func (s *service) Get(ctx context.Context) ([]runtimestate.BackendRuntimeState, error) {
	// serve reconciles at startup and on explicit refresh only; this debounced
	// reconcile lets /state and /setup-status self-heal a backend that came up
	// later. Best-effort: serve the existing snapshot if it fails.
	_ = s.state.ReconcileIfStale(ctx)
	m := s.state.Get(ctx)
	l := make([]runtimestate.BackendRuntimeState, 0, len(m))
	for _, e := range m {
		l = append(l, e)
	}
	return l, nil
}

// SetupStatus implements Service.
func (s *service) SetupStatus(ctx context.Context) (setupcheck.Result, error) {
	states, err := s.Get(ctx)
	if err != nil {
		return setupcheck.Result{}, err
	}
	in, err := setupcheck.GatherInput(ctx, s.db, states, s.workspaceID)
	if err != nil {
		return setupcheck.Result{}, err
	}
	return setupcheck.Evaluate(in), nil
}

// Refresh implements Service.
func (s *service) Refresh(ctx context.Context) (setupcheck.Result, error) {
	if err := s.state.RunBackendCycle(ctx); err != nil {
		return setupcheck.Result{}, err
	}
	return s.SetupStatus(ctx)
}

// CLIConfig implements Service.
func (s *service) CLIConfig(ctx context.Context) (CLIConfigSnapshot, error) {
	store := runtimetypes.New(s.db.WithoutTransaction())
	return s.cliConfigSnapshot(ctx, store), nil
}

// SetCLIConfig implements Service.
func (s *service) SetCLIConfig(ctx context.Context, patch CLIConfigPatch) (CLIConfigSnapshot, error) {
	if patch.DefaultModel == nil &&
		patch.DefaultProvider == nil &&
		patch.DefaultAltModel == nil &&
		patch.DefaultAltProvider == nil &&
		patch.DefaultAutocompleteModel == nil &&
		patch.DefaultAutocompleteProvider == nil &&
		patch.DefaultMaxTokens == nil &&
		patch.DefaultThink == nil &&
		patch.DefaultChain == nil &&
		patch.HITLPolicyName == nil &&
		patch.TelemetryEnabled == nil &&
		patch.UpdateCheck == nil &&
		patch.DefaultMissionAgent == nil &&
		patch.DefaultMissionPolicy == nil {
		return CLIConfigSnapshot{}, fmt.Errorf("provide at least one CLI config key")
	}
	store := runtimetypes.New(s.db.WithoutTransaction())
	if patch.DefaultModel != nil {
		if err := clikv.SetString(ctx, store, "default-model", strings.TrimSpace(*patch.DefaultModel)); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set default-model: %w", err)
		}
	}
	if patch.DefaultProvider != nil {
		if err := clikv.SetString(ctx, store, "default-provider", strings.TrimSpace(*patch.DefaultProvider)); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set default-provider: %w", err)
		}
	}
	if patch.DefaultAltModel != nil {
		if err := clikv.SetString(ctx, store, "default-alt-model", strings.TrimSpace(*patch.DefaultAltModel)); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set default-alt-model: %w", err)
		}
	}
	if patch.DefaultAltProvider != nil {
		if err := clikv.SetString(ctx, store, "default-alt-provider", strings.TrimSpace(*patch.DefaultAltProvider)); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set default-alt-provider: %w", err)
		}
	}
	if patch.DefaultAutocompleteModel != nil {
		if err := clikv.SetString(ctx, store, "default-autocomplete-model", strings.TrimSpace(*patch.DefaultAutocompleteModel)); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set default-autocomplete-model: %w", err)
		}
	}
	if patch.DefaultAutocompleteProvider != nil {
		if err := clikv.SetString(ctx, store, "default-autocomplete-provider", strings.TrimSpace(*patch.DefaultAutocompleteProvider)); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set default-autocomplete-provider: %w", err)
		}
	}
	if patch.DefaultMaxTokens != nil {
		maxTokens, err := normalizeDefaultMaxTokens(*patch.DefaultMaxTokens)
		if err != nil {
			return CLIConfigSnapshot{}, err
		}
		if err := clikv.SetString(ctx, store, "default-max-tokens", maxTokens); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set default-max-tokens: %w", err)
		}
	}
	if patch.DefaultThink != nil {
		think, err := normalizeDefaultThink(*patch.DefaultThink)
		if err != nil {
			return CLIConfigSnapshot{}, err
		}
		if err := clikv.SetString(ctx, store, "default-think", think); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set default-think: %w", err)
		}
	}
	if patch.DefaultChain != nil {
		if err := clikv.WriteConfig(ctx, store, s.workspaceID, "default-chain", strings.TrimSpace(*patch.DefaultChain)); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set default-chain: %w", err)
		}
	}
	if patch.HITLPolicyName != nil {
		if err := clikv.WriteConfig(ctx, store, s.workspaceID, "hitl-policy-name", strings.TrimSpace(*patch.HITLPolicyName)); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set hitl-policy-name: %w", err)
		}
	}
	if patch.TelemetryEnabled != nil {
		if err := clikv.SetString(ctx, store, "telemetry-enabled", strings.TrimSpace(*patch.TelemetryEnabled)); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set telemetry-enabled: %w", err)
		}
	}
	if patch.UpdateCheck != nil {
		if err := clikv.SetString(ctx, store, "update-check", strings.TrimSpace(*patch.UpdateCheck)); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set update-check: %w", err)
		}
	}
	// Global, like the model defaults: a mission fires at the fleet, not a
	// project. Written verbatim; names are validated where used (dispatch),
	// so setting one before its agent exists is a stale pointer, not corruption.
	if patch.DefaultMissionAgent != nil {
		if err := clikv.SetString(ctx, store, "default-mission-agent", strings.TrimSpace(*patch.DefaultMissionAgent)); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set default-mission-agent: %w", err)
		}
	}
	if patch.DefaultMissionPolicy != nil {
		if err := clikv.SetString(ctx, store, "default-mission-policy", strings.TrimSpace(*patch.DefaultMissionPolicy)); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set default-mission-policy: %w", err)
		}
	}
	return s.cliConfigSnapshot(ctx, store), nil
}

func (s *service) cliConfigSnapshot(ctx context.Context, store runtimetypes.Store) CLIConfigSnapshot {
	defaultModel, defaultModelPresent := readCLIConfigValue(ctx, store, "default-model")
	defaultProvider, defaultProviderPresent := readCLIConfigValue(ctx, store, "default-provider")
	defaultAltModel, defaultAltModelPresent := readCLIConfigValue(ctx, store, "default-alt-model")
	defaultAltProvider, defaultAltProviderPresent := readCLIConfigValue(ctx, store, "default-alt-provider")
	defaultAutocompleteModel, defaultAutocompleteModelPresent := readCLIConfigValue(ctx, store, "default-autocomplete-model")
	defaultAutocompleteProvider, defaultAutocompleteProviderPresent := readCLIConfigValue(ctx, store, "default-autocomplete-provider")
	defaultMaxTokens, defaultMaxTokensPresent := readCLIConfigValue(ctx, store, "default-max-tokens")
	defaultThink, defaultThinkPresent := readCLIConfigValue(ctx, store, "default-think")
	defaultChain, chainFrom, defaultChainPresent := readWorkspaceCLIConfigValue(ctx, store, s.workspaceID, "default-chain")
	hitlPolicy, policyFrom, hitlPolicyPresent := readWorkspaceCLIConfigValue(ctx, store, s.workspaceID, "hitl-policy-name")
	telemetryEnabled, telemetryEnabledPresent := readCLIConfigValue(ctx, store, "telemetry-enabled")
	updateCheck, updateCheckPresent := readCLIConfigValue(ctx, store, "update-check")
	missionAgent, missionAgentPresent := readCLIConfigValue(ctx, store, "default-mission-agent")
	missionPolicy, missionPolicyPresent := readCLIConfigValue(ctx, store, "default-mission-policy")
	return CLIConfigSnapshot{
		DefaultModel:                defaultModel,
		DefaultProvider:             defaultProvider,
		DefaultAltModel:             defaultAltModel,
		DefaultAltProvider:          defaultAltProvider,
		DefaultAutocompleteModel:    defaultAutocompleteModel,
		DefaultAutocompleteProvider: defaultAutocompleteProvider,
		DefaultMaxTokens:            defaultMaxTokens,
		DefaultThink:                defaultThink,
		DefaultChain:                defaultChain,
		HITLPolicyName:              hitlPolicy,
		TelemetryEnabled:            telemetryEnabled,
		UpdateCheck:                 updateCheck,
		DefaultMissionAgent:         missionAgent,
		DefaultMissionPolicy:        missionPolicy,
		ResolvedFrom: map[string]string{
			"defaultChain":   chainFrom,
			"hitlPolicyName": policyFrom,
		},
		Present: map[string]bool{
			"default-model":                 defaultModelPresent,
			"default-provider":              defaultProviderPresent,
			"default-alt-model":             defaultAltModelPresent,
			"default-alt-provider":          defaultAltProviderPresent,
			"default-autocomplete-model":    defaultAutocompleteModelPresent,
			"default-autocomplete-provider": defaultAutocompleteProviderPresent,
			"default-max-tokens":            defaultMaxTokensPresent,
			"default-think":                 defaultThinkPresent,
			"default-chain":                 defaultChainPresent,
			"hitl-policy-name":              hitlPolicyPresent,
			"telemetry-enabled":             telemetryEnabledPresent,
			"update-check":                  updateCheckPresent,
			"default-mission-agent":         missionAgentPresent,
			"default-mission-policy":        missionPolicyPresent,
		},
	}
}

func readCLIConfigValue(ctx context.Context, store runtimetypes.Store, key string) (string, bool) {
	var val string
	if err := store.GetKV(ctx, clikv.Prefix+key, &val); err != nil {
		return "", false
	}
	return strings.TrimSpace(val), true
}

func readWorkspaceCLIConfigValue(ctx context.Context, store runtimetypes.Store, workspaceID, key string) (string, string, bool) {
	if workspaceID != "" {
		var val string
		if err := store.GetWorkspaceKV(ctx, workspaceID, clikv.Prefix+key, &val); err == nil {
			return strings.TrimSpace(val), "workspace", true
		}
	}
	val, present := readCLIConfigValue(ctx, store, key)
	return val, "global", present
}

// normalizeDefaultThink validates/normalizes a default-think value the same
// way `contenox config set default-think` does (see reasoning.Normalize).
// Empty clears the override, letting the runtime's own "high" fallback apply.
func normalizeDefaultThink(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	return reasoning.Normalize(value)
}

func normalizeDefaultMaxTokens(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return "", fmt.Errorf("default-max-tokens must be a non-negative integer, got %q", value)
	}
	if n < 0 {
		return "", fmt.Errorf("default-max-tokens must be non-negative, got %d", n)
	}
	return strconv.Itoa(n), nil
}

// New returns a state service backed by runtime state and the same DB used for backends + CLI KV.
// workspaceID scopes workspace-specific config (default-chain, hitl-policy-name) with global fallback.
func New(state *runtimestate.State, db libdbexec.DBManager, workspaceID string) Service {
	return &service{
		state:       state,
		db:          db,
		workspaceID: workspaceID,
	}
}
