// Package chainagents seeds the declared-agent registry from the runtime's
// own task chains, so a reviewed chain can be fired as a fleet unit without
// a second registration step. Eligibility is by convention (shipped
// agent-shaped chains by id, or files named "chain-agent-*.json"); discovery
// upserts rows it owns (Source "discovered") and never writes to any other row.
package chainagents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/localfileservice"
	"github.com/contenox/contenox/internal/services/taskchainservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
)

// AgentChainFilePrefix: a chain file whose basename starts with it is an agent template.
const AgentChainFilePrefix = "chain-agent-"

// shippedAgentChains are the ids of the runtime's own agent-shaped (not utility) chains.
var shippedAgentChains = map[string]bool{
	"chain-contenox": true, // default interactive chat agent (chain-agent-contenox.json)
	"chain-acp":      true, // the ACP agent surface (chain-agent-acp.json)
	"chain-acpx":     true, // the headless/untrusted-driver ACP agent (chain-agent-acpx.json)
	"chain-beam":     true, // the beam TUI agent surface (chain-agent-beam.json)
	"chain-run":      true, // the one-shot `contenox run` agent (chain-agent-run.json)
}

// shippedPlannerAgent is the shipped default-mission planner's chain id and
// agent name (seeded as chain-planner-default.json), eligible by id so the
// planner role needs no chain-agent-* filename. The name is the chain id, so
// `mission fire agent-planner` is stable across file renames.
const shippedPlannerAgent = "agent-planner"

// StableAgentName reports whether a discovered-agent name is visible without
// the beta agent-roster opt-in: a shipped agent-shaped chain, or the shipped
// planner. User-authored chain-agent-* chains are not.
func StableAgentName(name string) bool {
	return shippedAgentChains[name] || name == shippedPlannerAgent
}

// Result reports what one Discover pass did. Every slice holds agent names.
type Result struct {
	Created   []string // rows that did not exist and now do, enabled
	Updated   []string // rows whose chain path or id moved and were rewritten
	Unchanged []string // rows that already matched exactly; nothing was written
	Disabled  []string // rows whose chain file is gone; see Discover
	Skipped   []string // eligible chains whose name is taken by an agent this package does not own
}

// Discover walks roots (precedence order) for eligible chains and
// reconciles the registry: a vanished chain file disables its agent
// (never deletes, never auto-re-enables). Idempotent; see DiscoverWithTracker.
func Discover(ctx context.Context, agents agentregistryservice.Service, roots ...string) (Result, error) {
	return DiscoverWithTracker(ctx, agents, nil, roots...)
}

// DiscoverWithTracker is Discover with diagnostics reported to tracker (nil degrades to a no-op).
func DiscoverWithTracker(ctx context.Context, agents agentregistryservice.Service, tracker libtracker.ActivityTracker, roots ...string) (Result, error) {
	return DiscoverKept(ctx, agents, tracker, nil, roots...)
}

// DiscoverKept is DiscoverWithTracker restricted to agent names keep allows.
// A name keep refuses is outside the pass entirely: neither registered nor
// reconciled — its existing row, if any, is left exactly as it stands. A nil
// keep keeps every candidate.
func DiscoverKept(ctx context.Context, agents agentregistryservice.Service, tracker libtracker.ActivityTracker, keep func(string) bool, roots ...string) (Result, error) {
	var result Result
	if agents == nil {
		return result, fmt.Errorf("chainagents: agent registry is required")
	}
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}

	found, err := scan(ctx, tracker, roots)
	if err != nil {
		return result, err
	}

	names := make(map[string]bool, len(found))
	for _, c := range found {
		if keep != nil && !keep(c.name) {
			continue
		}
		names[c.name] = true
		action, err := upsert(ctx, agents, c)
		if err != nil {
			return result, fmt.Errorf("chainagents: seed chain agent %q: %w", c.name, err)
		}
		switch action {
		case actionCreated:
			result.Created = append(result.Created, c.name)
		case actionUpdated:
			result.Updated = append(result.Updated, c.name)
		case actionUnchanged:
			result.Unchanged = append(result.Unchanged, c.name)
		case actionSkipped:
			result.Skipped = append(result.Skipped, c.name)
		}
	}

	disabled, err := disableVanished(ctx, agents, tracker, names, keep)
	if err != nil {
		return result, err
	}
	result.Disabled = disabled
	return result, nil
}

// candidate is one eligible chain: the agent name it seeds, and its file.
type candidate struct {
	name    string
	path    string
	chainID string
}

// scan walks each root with the same chain walker the rest of the runtime uses.
func scan(ctx context.Context, tracker libtracker.ActivityTracker, roots []string) ([]candidate, error) {
	var out []candidate
	claimed := map[string]bool{}
	// A repeated root would only duplicate per-file diagnostics.
	seenRoot := map[string]bool{}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("chainagents: resolve chain root %q: %w", root, err)
		}
		if seenRoot[abs] {
			continue
		}
		seenRoot[abs] = true
		// Discovery reads the operator's directories; it does not create them.
		if info, err := os.Stat(abs); err != nil || !info.IsDir() {
			continue
		}
		files, err := localfileservice.NewPrivileged(abs)
		if err != nil {
			return nil, fmt.Errorf("chainagents: open chain root %q: %w", abs, err)
		}
		chains := taskchainservice.NewLocal(files)
		paths, err := chains.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("chainagents: list chains in %q: %w", abs, err)
		}
		for _, rel := range paths {
			chain, err := chains.Get(ctx, rel)
			if err != nil {
				// A chain the linter refuses never seeds the registry.
				if errors.Is(err, taskengine.ErrChainLint) {
					reportErr, _, end := tracker.Start(ctx, "discover", "chain_agent",
						"chain_path", filepath.Join(abs, filepath.FromSlash(rel)),
						"remedy", "fix the file (see 'contenox vet'), then run 'contenox agent enable <name>' if the agent was disabled")
					reportErr(fmt.Errorf("chain agent discovery: chain file fails validation and was skipped: %w", err))
					end()
				}
				continue
			}
			if !eligible(filepath.Base(rel), chain.ID) {
				continue
			}
			name := agentName(chain.ID)
			if name == "" || claimed[name] {
				// An earlier (higher-precedence) root already provides this chain.
				continue
			}
			claimed[name] = true
			out = append(out, candidate{
				name:    name,
				path:    filepath.Join(abs, filepath.FromSlash(rel)),
				chainID: chain.ID,
			})
		}
	}
	return out, nil
}

// eligible applies the two conventions: shipped agent-shaped chain by id
// (incl. the planner), or the chain-agent-* filename.
func eligible(base, chainID string) bool {
	if shippedAgentChains[chainID] || chainID == shippedPlannerAgent {
		return true
	}
	return strings.HasPrefix(strings.ToLower(base), AgentChainFilePrefix)
}

// agentName derives the registry name from the chain id, verbatim.
func agentName(chainID string) string { return strings.TrimSpace(chainID) }

type action int

const (
	actionCreated action = iota
	actionUpdated
	actionUnchanged
	actionSkipped
)

func upsert(ctx context.Context, agents agentregistryservice.Service, c candidate) (action, error) {
	existing, err := agents.GetByName(ctx, c.name)
	if errors.Is(err, libdb.ErrNotFound) {
		fresh := &runtimetypes.Agent{
			Name:    c.name,
			Enabled: true,
			Source:  sourceDiscovered(),
		}
		if err := fresh.SetChainConfig(runtimetypes.ChainConfig{Path: c.path, ChainID: c.chainID}); err != nil {
			return actionSkipped, err
		}
		if err := agents.Create(ctx, fresh); err != nil {
			return actionSkipped, err
		}
		return actionCreated, nil
	}
	if err != nil {
		return actionSkipped, err
	}
	// Never write to a row this package does not own.
	if existing.Source == nil || *existing.Source != runtimetypes.AgentSourceDiscovered {
		return actionSkipped, nil
	}
	if existing.Kind == runtimetypes.AgentKindChain {
		if cfg, err := existing.ChainConfig(); err == nil && cfg.Path == c.path && cfg.ChainID == c.chainID {
			// Byte-identical: write nothing, not even updated_at.
			return actionUnchanged, nil
		}
	}
	if err := existing.SetChainConfig(runtimetypes.ChainConfig{Path: c.path, ChainID: c.chainID}); err != nil {
		return actionSkipped, err
	}
	if err := agents.Update(ctx, existing); err != nil {
		return actionSkipped, err
	}
	return actionUpdated, nil
}

// disableVanished disables every discovered agent with no matching chain file.
// An agent keep refuses was never scanned for, so its absence proves nothing;
// it is skipped, not disabled.
func disableVanished(ctx context.Context, agents agentregistryservice.Service, tracker libtracker.ActivityTracker, found map[string]bool, keep func(string) bool) ([]string, error) {
	all, err := agents.List(ctx, nil, runtimetypes.MAXLIMIT)
	if err != nil {
		return nil, fmt.Errorf("chainagents: list declared agents: %w", err)
	}
	var disabled []string
	for _, agent := range all {
		if agent.Source == nil || *agent.Source != runtimetypes.AgentSourceDiscovered {
			continue
		}
		if keep != nil && !keep(agent.Name) {
			continue
		}
		if found[agent.Name] || !agent.Enabled {
			continue
		}
		path := ""
		if cfg, err := agent.ChainConfig(); err == nil {
			path = cfg.Path
		}
		agent.Enabled = false
		if err := agents.Update(ctx, agent); err != nil {
			return nil, fmt.Errorf("chainagents: disable vanished chain agent %q: %w", agent.Name, err)
		}
		reportErr, _, end := tracker.Start(ctx, "disable", "chain_agent",
			"agent", agent.Name, "chain_path", path,
			"remedy", "restore or fix the file and run 'contenox agent enable "+agent.Name+"', or remove the agent")
		reportErr(fmt.Errorf("chain agent disabled: its chain file is gone or fails validation (a validation reason was reported by the scan above)"))
		end()
		disabled = append(disabled, agent.Name)
	}
	return disabled, nil
}

func sourceDiscovered() *string {
	s := runtimetypes.AgentSourceDiscovered
	return &s
}
