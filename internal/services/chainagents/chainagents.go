// Package chainagents seeds the declared-agent registry from the runtime's own
// task chains, so a chain an operator already wrote and reviewed can be fired
// as a fleet unit without a second registration step.
//
// # Convention, not declaration
//
// Requiring an operator to hand-register every chain would be ceremony over a
// file that already exists, so eligibility is decided by convention:
//
//   - the agent-shaped chains the runtime itself ships (see shippedAgentChains),
//     matched by the chain's own id so a renamed or relocated preset still
//     counts; and
//   - any chain FILE named "agent-*.json" in a walked directory. Naming the
//     file is the declaration — a shell verb, no JSON edit, no restart-time
//     registration ritual.
//
// # Seed, don't fork the resolution path
//
// Discovery UPSERTS each eligible chain as an ordinary registry row of kind
// "chain" with Source "discovered", and then gets out of the way. Everything
// downstream — the Enabled gate, agentregistryservice.ResolveForSpawn,
// fleetservice.Dispatch, the board — keeps working unchanged, against ONE
// source of truth for "what can I fire". Nothing in the spawn path consults a
// second lookup: two drifting implementations of one judgement is the defect
// this whole consolidation exists to repair, and a discovery pass that resolved
// chains at spawn time instead of seeding them would reintroduce it.
//
// # It owns its rows, and only its rows
//
// A row with Source "discovered" belongs to this package: it is re-upserted on
// every pass and disabled when its chain file goes away. A row with any other
// Source (or a name collision with one) is never written to.
package chainagents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/localfileservice"
	"github.com/contenox/beam/internal/services/taskchainservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
)

// AgentChainFilePrefix is the filename convention that makes a chain eligible:
// a chain file whose BASENAME starts with it is an agent template.
//
// The convention is on the FILENAME, not on the chain's id, for two reasons.
// It is the operator's declaration and it must be made the way an operator
// works — `mv review.json agent-review.json` is a shell verb; editing an id
// inside JSON is not, and it would break every reference that resolves that
// chain BY id (taskchainservice.Get accepts a path or an id) as a side effect
// of declaring an agent. And the walker this package reuses already hands back
// paths, so the filename is the fact in hand.
//
// The agent's NAME still comes from the chain's id (see agentName): a name has
// to survive the file being moved between the workspace and the home
// directory, and it is what an operator types at `contenox fleet dispatch`.
const AgentChainFilePrefix = "agent-"

// shippedAgentChains are the ids of the chains the runtime ships that are
// AGENT-shaped rather than utility, keyed by chain id because the on-disk
// filename of a shipped preset is not stable (the same chain ships as
// "default-chain.json" in one role and would be copied under any name in
// another) while the id inside it is written once and never rewritten.
//
// The line between the two is drawn on what the chain's INPUT is:
//
//   - Agent-shaped: it takes an open-ended human intent as its turn and works
//     on it, with at least one tool-executing task so it can ACT on that
//     intent rather than only answer it. These are the chains that already
//     back a conversational surface.
//   - Utility: the runtime invokes it on data the RUNTIME constructed — a
//     transcript to compact, a prefix/suffix to complete — with no tool loop
//     at all. Dispatched with an intent, such a chain would not do the
//     intent; it would try to summarize it. It is a subroutine, not a unit.
//
// The one-shot pipeline chain is deliberately excluded even though it does
// have a tool loop: it is written for a non-interactive caller, instructed to
// refuse and stop rather than proceed when a request is under-specified, which
// is right for a pipe and wrong for a supervised unit that can be asked a
// follow-up.
var shippedAgentChains = map[string]bool{
	"chain-contenox": true, // default interactive chat agent (default-chain.json)
	"chain-acp":      true, // the ACP agent surface (default-acp-chain.json)
	"chain-acpx":     true, // the headless/untrusted-driver ACP agent (headless-acp-chain.json)
}

// Result reports what one Discover pass did. Every slice holds agent NAMES.
type Result struct {
	// Created are rows that did not exist and now do, Enabled.
	Created []string
	// Updated are rows whose chain path or id moved and were rewritten.
	Updated []string
	// Unchanged are rows that already matched exactly — nothing was written
	// for them, which is what makes a repeated pass a no-op down to the
	// updated-at timestamp.
	Unchanged []string
	// Disabled are rows whose chain file is gone; see Discover.
	Disabled []string
	// Skipped are eligible chains whose name is taken by an agent this package
	// does not own, and which were therefore left entirely alone.
	Skipped []string
}

// Discover walks roots for eligible chains and reconciles the registry against
// them. roots are directories in PRECEDENCE order, highest first: a chain id
// found in an earlier root wins over the same id in a later one, matching how
// the rest of the runtime resolves a config file (workspace overrides home). A
// root that does not exist is skipped, not created.
//
// # What happens to an agent whose chain file vanished
//
// It is DISABLED, not deleted, and logged. Deleting would destroy the row that
// mission records and lifecycle telemetry already point at, and would make the
// unit disappear from the board with no trace of why. Disabling routes the
// refusal through the one shared judgement every spawn path already makes —
// ResolveForSpawn refuses a disabled agent with a message that names the
// remedy — instead of inventing a second "the chain file is missing" refusal
// somewhere down in the kernel.
//
// Re-appearance is deliberately NOT automatic re-enablement: this pass never
// moves Enabled upward on a row that already exists, so an agent an operator
// disabled by hand stays disabled across restarts. Restoring the file and
// running `contenox agent enable <name>` is the same one verb that governs
// every other agent.
//
// Discover is idempotent: a second pass over an unchanged tree writes nothing
// at all.
func Discover(ctx context.Context, agents agentregistryservice.Service, roots ...string) (Result, error) {
	var result Result
	if agents == nil {
		return result, fmt.Errorf("chainagents: agent registry is required")
	}

	found, err := scan(ctx, roots)
	if err != nil {
		return result, err
	}

	names := make(map[string]bool, len(found))
	for _, c := range found {
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

	disabled, err := disableVanished(ctx, agents, names)
	if err != nil {
		return result, err
	}
	result.Disabled = disabled
	return result, nil
}

// candidate is one eligible chain: the agent name it seeds, and the file it
// runs.
type candidate struct {
	name    string
	path    string
	chainID string
}

// scan walks each root with the SAME chain walker the rest of the runtime uses
// (taskchainservice over localfileservice), rather than a second directory
// walk with its own idea of what a chain file is. That walker already parses
// every .json in the directory and drops the ones without an id or without
// tasks, so an unparseable or half-written file is skipped here for free and
// for the same reason it is skipped everywhere else.
func scan(ctx context.Context, roots []string) ([]candidate, error) {
	var out []candidate
	claimed := map[string]bool{}
	// Roots are a PRECEDENCE list, so a root that appears twice adds nothing: the
	// first pass over it already claimed every agent it can provide, and by the
	// precedence rule the second pass would lose every one of those claims to
	// itself. Walking it again is therefore pure duplication — including of the
	// per-file diagnostics below, which is how it announced itself: an operator
	// launching a surface whose workspace directory IS ~/.contenox (beam, and
	// `contenox acp` outside a project) saw every "chain file fails validation"
	// warning printed exactly twice on every startup. Deduplicated HERE, on the
	// resolved absolute path, so it holds for every caller rather than for
	// whichever one remembered.
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
		// Skipped rather than created: discovery reads the operator's
		// directories, it does not conjure them.
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
				// taskchainservice.Get vets every chain it returns, so a chain
				// the linter refuses never seeds the registry. Say WHY, loudly:
				// the operator's next question is exactly this reason, and an
				// existing discovered row for the chain is disabled below by the
				// same vanished-file pass (a chain that cannot run and a chain
				// that is gone earn the same refusal at spawn).
				if errors.Is(err, taskengine.ErrChainLint) {
					slog.Warn("chain agent discovery: chain file fails validation and was skipped",
						"chain_path", filepath.Join(abs, filepath.FromSlash(rel)),
						"reason", err.Error(),
						"remedy", "fix the file (see 'contenox vet'), then run 'contenox agent enable <name>' if the agent was disabled")
				}
				continue
			}
			if !eligible(filepath.Base(rel), chain.ID) {
				continue
			}
			name := agentName(chain.ID)
			if name == "" || claimed[name] {
				// An earlier (higher-precedence) root already provides this
				// chain; the workspace copy shadows the home one exactly as it
				// does for every other config file.
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

// eligible applies the two conventions: a shipped agent-shaped chain (by id),
// or the agent-* filename convention.
func eligible(base, chainID string) bool {
	if shippedAgentChains[chainID] {
		return true
	}
	return strings.HasPrefix(strings.ToLower(base), AgentChainFilePrefix)
}

// agentName derives the registry name from the chain id. It is the chain's own
// id verbatim: the registry's uniqueness key is the name, the chain id is
// already unique among chains, and reusing it means the thing an operator
// dispatches is called what the chain is called.
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
	// Never write to a row this package does not own. An operator who
	// hand-registered an agent under this name, or seeded one from the agent
	// catalog, outranks a filename convention.
	if existing.Source == nil || *existing.Source != runtimetypes.AgentSourceDiscovered {
		return actionSkipped, nil
	}
	if existing.Kind == runtimetypes.AgentKindChain {
		if cfg, err := existing.ChainConfig(); err == nil && cfg.Path == c.path && cfg.ChainID == c.chainID {
			// Byte-identical run spec: write nothing, so a repeat pass does not
			// even move updated_at. Enabled is deliberately not part of this
			// comparison — see Discover.
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

// disableVanished disables every discovered agent that this pass did not find a
// chain file for. See Discover for why disable rather than delete.
func disableVanished(ctx context.Context, agents agentregistryservice.Service, found map[string]bool) ([]string, error) {
	all, err := agents.List(ctx, nil, runtimetypes.MAXLIMIT)
	if err != nil {
		return nil, fmt.Errorf("chainagents: list declared agents: %w", err)
	}
	var disabled []string
	for _, agent := range all {
		if agent.Source == nil || *agent.Source != runtimetypes.AgentSourceDiscovered {
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
		slog.Warn("chain agent disabled: its chain file is gone or fails validation (a validation reason was logged by the scan above)",
			"agent", agent.Name, "chain_path", path,
			"remedy", "restore or fix the file and run 'contenox agent enable "+agent.Name+"', or remove the agent")
		disabled = append(disabled, agent.Name)
	}
	return disabled, nil
}

func sourceDiscovered() *string {
	s := runtimetypes.AgentSourceDiscovered
	return &s
}
