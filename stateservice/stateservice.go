package stateservice

import (
	"context"
	"fmt"
	"strings"

	"github.com/contenox/contenox/internal/clikv"
	"github.com/contenox/contenox/internal/runtimestate"
	"github.com/contenox/contenox/internal/setupcheck"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/statetype"
)

// Service exposes runtime backend state plus onboarding/setup evaluation (same inputs as GET /setup-status).
type Service interface {
	Get(ctx context.Context) ([]statetype.BackendRuntimeState, error)
	// SetupStatus returns readiness from KV defaults, registered backends, and current runtime state.
	SetupStatus(ctx context.Context) (setupcheck.Result, error)
	// SetCLIConfig updates CLI default model, provider, and optional default-chain keys in SQLite KV (same as contenox config set / PUT /cli-config).
	// Empty fields in the patch are left unchanged. At least one field must be non-empty after trim.
	SetCLIConfig(ctx context.Context, patch CLIConfigPatch) (CLIConfigSnapshot, error)
}

// CLIConfigPatch selects which CLI default keys to write; empty strings mean "do not change".
type CLIConfigPatch struct {
	DefaultModel    string
	DefaultProvider string
	DefaultChain    string
}

// CLIConfigSnapshot is the resolved KV values after an update.
type CLIConfigSnapshot struct {
	DefaultModel    string
	DefaultProvider string
	DefaultChain    string
}

type service struct {
	state *runtimestate.State
	db    libdbexec.DBManager
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
	in, err := setupcheck.GatherInput(ctx, s.db, states)
	if err != nil {
		return setupcheck.Result{}, err
	}
	return setupcheck.Evaluate(in), nil
}

// SetCLIConfig implements Service.
func (s *service) SetCLIConfig(ctx context.Context, patch CLIConfigPatch) (CLIConfigSnapshot, error) {
	if strings.TrimSpace(patch.DefaultModel) == "" && strings.TrimSpace(patch.DefaultProvider) == "" && strings.TrimSpace(patch.DefaultChain) == "" {
		return CLIConfigSnapshot{}, fmt.Errorf("provide at least one of default-model, default-provider, or default-chain")
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
		if err := clikv.SetString(ctx, store, "default-chain", patch.DefaultChain); err != nil {
			return CLIConfigSnapshot{}, fmt.Errorf("set default-chain: %w", err)
		}
	}
	return CLIConfigSnapshot{
		DefaultModel:    clikv.Read(ctx, store, "default-model"),
		DefaultProvider: clikv.Read(ctx, store, "default-provider"),
		DefaultChain:    clikv.Read(ctx, store, "default-chain"),
	}, nil
}

// New returns a state service backed by runtime state and the same DB used for backends + CLI KV.
func New(state *runtimestate.State, db libdbexec.DBManager) Service {
	return &service{
		state: state,
		db:    db,
	}
}
