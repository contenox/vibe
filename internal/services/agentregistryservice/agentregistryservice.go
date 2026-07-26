// Package agentregistryservice stores declared agent configurations — the
// "bots table" concept (see the mvp core/serverops/store/schema.sql `bots`
// table this generalizes) reborn as a polymorphic, kind-dispatched resource.
// Two kinds are implemented: "external_acp" (an agent the runtime
// spawns/drives as an external ACP peer via runtime/agenthost) and "chain"
// (one of the runtime's own task chains, addressable as an agent — the same
// spawn, pointed at this binary's own ACP server; see
// runtimetypes.ChainConfig).
//
// It stays the SINGLE source of truth for "what can I fire". Chain agents are
// SEEDED into it by convention-based discovery (runtime/chainagents) rather
// than resolved through a second lookup at spawn time, so ResolveForSpawn
// keeps one implementation for both kinds.
//
// This package intentionally mirrors runtime/mcpserverservice's shape
// (validated CRUD over a runtimetypes store, no HTTP routes) so the two
// declared-resource registries stay easy to compare.
package agentregistryservice

import (
	"context"
	"errors"
	"fmt"
	"time"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/google/uuid"
)

// Service exposes validated CRUD operations for persisted agent
// configurations.
type Service interface {
	Create(ctx context.Context, agent *runtimetypes.Agent) error
	Get(ctx context.Context, id string) (*runtimetypes.Agent, error)
	GetByName(ctx context.Context, name string) (*runtimetypes.Agent, error)
	Update(ctx context.Context, agent *runtimetypes.Agent) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.Agent, error)
}

type service struct {
	db libdb.DBManager
}

// New creates a new agent registry service backed by the given database
// manager.
func New(db libdb.DBManager) Service {
	return &service{db: db}
}

func (s *service) store() runtimetypes.Store {
	return runtimetypes.New(s.db.WithoutTransaction())
}

// Create validates agent (name/kind/per-kind config) and its name against
// existing agents, then persists it. A colliding name surfaces as
// libdb.ErrUniqueViolation (checked via errors.Is), the same sentinel a raw
// DB unique-constraint violation would translate to elsewhere in this
// codebase — checked here up front so the conflict is reported clearly
// instead of relying solely on the storage layer's constraint error.
func (s *service) Create(ctx context.Context, agent *runtimetypes.Agent) error {
	if err := validate(agent); err != nil {
		return err
	}
	if agent.ID == "" {
		agent.ID = uuid.NewString()
	}
	if err := s.checkNameAvailable(ctx, agent.Name, agent.ID); err != nil {
		return err
	}
	return s.store().CreateAgent(ctx, agent)
}

func (s *service) Get(ctx context.Context, id string) (*runtimetypes.Agent, error) {
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	return s.store().GetAgent(ctx, id)
}

func (s *service) GetByName(ctx context.Context, name string) (*runtimetypes.Agent, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	return s.store().GetAgentByName(ctx, name)
}

// Update validates agent the same way Create does, additionally requiring an
// ID, and re-checks name uniqueness against every other agent (excluding
// agent's own ID, so renaming an agent to its own current name is a no-op,
// not a conflict).
func (s *service) Update(ctx context.Context, agent *runtimetypes.Agent) error {
	if agent.ID == "" {
		return fmt.Errorf("id is required for update")
	}
	if err := validate(agent); err != nil {
		return err
	}
	if err := s.checkNameAvailable(ctx, agent.Name, agent.ID); err != nil {
		return err
	}
	return s.store().UpdateAgent(ctx, agent)
}

func (s *service) Delete(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("id is required")
	}
	return s.store().DeleteAgent(ctx, id)
}

func (s *service) List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.Agent, error) {
	return s.store().ListAgents(ctx, createdAtCursor, limit)
}

// ErrAgentDisabled is the sentinel identifying a ResolveForSpawn refusal
// caused by a declared agent that exists but is administratively disabled
// (Enabled == false). Callers branch on it via errors.Is to apply their own
// transport's "refused" mapping (apiframework.Conflict for a REST caller, a
// libacp ACP-level error for an ACP caller) while reusing the wrapping
// error's message, which already names the remedy — see ResolveForSpawn.
var ErrAgentDisabled = errors.New("agentregistryservice: agent is disabled")

// disabledAgentError pairs ErrAgentDisabled with a ready-to-display message
// that already names the remedy, so every caller shows the same wording
// instead of each reconstructing it — which is how the fleet-dispatch message
// and the acpsvc chat-path message drifted apart before ResolveForSpawn
// existed (docs/development/blueprints/acp/fleet-consolidation.md, slice C5).
type disabledAgentError struct{ name string }

func (e *disabledAgentError) Error() string {
	return fmt.Sprintf("agent %q is disabled; enable it with 'contenox agent enable %q'", e.name, e.name)
}

func (e *disabledAgentError) Unwrap() error { return ErrAgentDisabled }

// ResolveForSpawn resolves the declared agent named agentName via svc and
// refuses to hand it back when it is administratively disabled. It is the
// ONE judgment every agent-spawn path makes before bringing an instance up —
// fleetservice.Dispatch and acpsvc's external bring-up (bringUpExternal, via
// resolveExternalAgent) both call it, so "disabled" cannot drift into two
// different checks or two different messages between them again.
// agentinstance.Manager.Start deliberately stays unaware of Enabled (see its
// doc comment): this is the service-layer policy the kernel is not allowed
// to hold (fleet-consolidation.md's "kernel stays policy-free" invariant).
//
// A not-found or other resolution failure from svc.GetByName is returned
// wrapped (fmt.Errorf %w), so a sentinel check like errors.Is(err,
// libdb.ErrNotFound) still works through it.
func ResolveForSpawn(ctx context.Context, svc Service, agentName string) (*runtimetypes.Agent, error) {
	agent, err := svc.GetByName(ctx, agentName)
	if err != nil {
		return nil, fmt.Errorf("resolve agent %q: %w", agentName, err)
	}
	if !agent.Enabled {
		return nil, &disabledAgentError{name: agentName}
	}
	return agent, nil
}

// checkNameAvailable returns a libdb.ErrUniqueViolation-wrapping error if an
// agent with name already exists under a different ID than excludeID.
func (s *service) checkNameAvailable(ctx context.Context, name, excludeID string) error {
	existing, err := s.store().GetAgentByName(ctx, name)
	if err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return nil
		}
		return err
	}
	if existing.ID == excludeID {
		return nil
	}
	return fmt.Errorf("agent: name %q already exists: %w", name, libdb.ErrUniqueViolation)
}

// validate checks the agent-level fields (name, kind) and, for kinds this
// registry currently implements, the per-kind config's own Validate().
func validate(agent *runtimetypes.Agent) error {
	if agent.Name == "" {
		return fmt.Errorf("name is required")
	}
	switch agent.Kind {
	case runtimetypes.AgentKindExternalACP:
		cfg, err := agent.ExternalACPConfig()
		if err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
	case runtimetypes.AgentKindChain:
		cfg, err := agent.ChainConfig()
		if err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
	case "":
		return fmt.Errorf("kind is required")
	default:
		return fmt.Errorf("unknown agent kind %q: must be %q or %q",
			agent.Kind, runtimetypes.AgentKindExternalACP, runtimetypes.AgentKindChain)
	}
	return nil
}
