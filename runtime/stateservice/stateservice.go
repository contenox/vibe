package stateservice

import (
	"context"
	"fmt"
	"strings"

	"github.com/contenox/contenox/runtime/internal/clikv"
	"github.com/contenox/contenox/runtime/internal/runtimestate"
	"github.com/contenox/contenox/runtime/internal/setupcheck"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/contenox/contenox/runtime/statetype"
)

// Service exposes runtime backend state plus onboarding/setup evaluation (same inputs as GET /setup-status).
type Service interface {
	Get(ctx context.Context) ([]statetype.BackendRuntimeState, error)
	// SetupStatus returns readiness from KV defaults, registered backends, and current runtime state.
	SetupStatus(ctx context.Context) (setupcheck.Result, error)
	// SetCLIConfig updates CLI default keys (model, provider, chain, hitl-policy-name) in SQLite KV (same as contenox config set / PUT /cli-config).
	// Empty fields in the patch are left unchanged. At least one field must be non-empty after trim.
	SetCLIConfig(ctx context.Context, patch CLIConfigPatch) (CLIConfigSnapshot, error)
}

// CLIConfigPatch selects which CLI default keys to write; empty strings mean "do not change".
type CLIConfigPatch struct {
	DefaultModel    string
	DefaultProvider string
	DefaultChain    string
	HITLPolicyName  string
}

// CLIConfigSnapshot is the resolved KV values after an update.
type CLIConfigSnapshot struct {
	DefaultModel    string
	DefaultProvider string
	DefaultChain    string
	HITLPolicyName  string
	ResolvedFrom    map[string]string
}

type service struct {
	state       *runtimestate.State
	db          libdbexec.DBManager
	workspaceID string
}

// Get implements Service.
func (s *service) Get(ctx context.Context) ([]statetype.BackendRuntimeState, error) {
	m := s.state.Get(ctx)
	l := make([]statetype.BackendRuntimeState, 0, len(m))
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

// SetCLIConfig implements Service.
func (s *service) SetCLIConfig(ctx context.Context, patch CLIConfigPatch) (CLIConfigSnapshot, error) {
	if strings.TrimSpace(patch.DefaultModel) == "" &&
		strings.TrimSpace(patch.DefaultProvider) == "" &&
		strings.TrimSpace(patch.DefaultChain) == "" &&
		strings.TrimSpace(patch.HITLPolicyName) == "" {
		return CLIConfigSnapshot{}, fmt.Errorf("provide at least one of default-model, default-provider, default-chain, or hitl-policy-name")
	}
	store := runtimetypes.New(s.db.WithoutTransaction())
	if strings.TrimSpace(patch.DefaultModel) != "" {
		if err := clikv.SetString(ctx, store, "default-model", patch.DefaultModel); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set default-model: %w", err)
		}
	}
	if strings.TrimSpace(patch.DefaultProvider) != "" {
		if err := clikv.SetString(ctx, store, "default-provider", patch.DefaultProvider); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set default-provider: %w", err)
		}
	}
	if strings.TrimSpace(patch.DefaultChain) != "" {
		if err := clikv.WriteConfig(ctx, store, s.workspaceID, "default-chain", patch.DefaultChain); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set default-chain: %w", err)
		}
	}
	if strings.TrimSpace(patch.HITLPolicyName) != "" {
		if err := clikv.WriteConfig(ctx, store, s.workspaceID, "hitl-policy-name", patch.HITLPolicyName); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set hitl-policy-name: %w", err)
		}
	}
	defaultChain, chainFrom := clikv.ReadConfig(ctx, store, s.workspaceID, "default-chain")
	hitlPolicy, policyFrom := clikv.ReadConfig(ctx, store, s.workspaceID, "hitl-policy-name")
	return CLIConfigSnapshot{
		DefaultModel:    clikv.Read(ctx, store, "default-model"),
		DefaultProvider: clikv.Read(ctx, store, "default-provider"),
		DefaultChain:    defaultChain,
		HITLPolicyName:  hitlPolicy,
		ResolvedFrom: map[string]string{
			"defaultChain":   chainFrom,
			"hitlPolicyName": policyFrom,
		},
	}, nil
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
