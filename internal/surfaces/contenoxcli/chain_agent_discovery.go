package contenoxcli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libbus"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/chainagents"
	"github.com/contenox/contenox/libtracker"
)

// discoverChainAgents runs one chain-agent discovery pass over the workspace
// .contenox/ and ~/.contenox/, declaring chain-agent-* chains (and the shipped
// chains by id) as fleet-dispatchable agents. Best effort: a failed pass leaves
// the registry as it was, and the outcome is reported via tracker (not
// stderr) since this runs unattended. A nil tracker degrades to Noop.
// Markdown declarations are transpiled first into a generated directory that is
// then discovered as an ordinary chain root, ordered last so a hand-written
// chain of the same name wins.
func discoverChainAgents(ctx context.Context, agents agentregistryservice.Service, contenoxDir string, tracker libtracker.ActivityTracker, deps DiscoverDeps) {
	discoverChainAgentsReporting(ctx, agents, contenoxDir, tracker, deps)
}

// DiscoverDeps carries what registering a declaration's own tool sources needs.
// Both fields are optional and degrade in a defined way: without a Store the
// pass reads declarations and writes chains but registers nothing, and without
// a Bus the rows are written while worker startup waits for a host that has
// one. A command that only inspects the roster supplies neither.
type DiscoverDeps struct {
	Store runtimetypes.Store
	Bus   libbus.Messenger
}

// discoverChainAgentsReporting is discoverChainAgents plus the sync results a
// caller may want to show a human. The tracker records everything; this returns
// only what an operator has to act on — a declaration that was refused, and
// configuration that named something which does not exist.
func discoverChainAgentsReporting(ctx context.Context, agents agentregistryservice.Service, contenoxDir string, tracker libtracker.ActivityTracker, deps DiscoverDeps) []agentdecl.SyncResult {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	var notable []agentdecl.SyncResult
	roots := []string{contenoxDir}
	homeDir, homeErr := globalContenoxDir()
	if homeErr == nil {
		// The shipped chains live under system/ and are scanned last of the
		// two, so an operator copy at the top level still claims the name.
		roots = append(roots, homeDir, systemDir(homeDir))
	}

	generated, results := syncDeclaredAgents(ctx, contenoxDir, homeDir, tracker)
	if generated != "" {
		roots = append(roots, generated)
	}
	// Registered before discovery: the emitted chain names these toolsets, so
	// they must exist by the time the agent it belongs to can be dispatched.
	if deps.Store != nil {
		results = append(results, reconcileDeclaredTools(ctx, deps.Store, deps.Bus, results)...)
	}
	for _, r := range results {
		if r.Action == agentdecl.ActionRefused || r.Action == agentdecl.ActionIgnored || len(r.Unmapped) > 0 {
			notable = append(notable, r)
		}
	}

	reportErr, reportChange, end := tracker.Start(ctx, "discover", "chain_agents", "roots", roots)
	defer end()

	// DiscoverKept (not Discover) reports refused files and vanished agents through the tracker.
	res, err := chainagents.DiscoverKept(ctx, agents, tracker, nil, roots...)
	if err != nil {
		reportErr(err)
		return notable
	}
	if len(res.Created) > 0 || len(res.Updated) > 0 || len(res.Disabled) > 0 || len(res.Skipped) > 0 {
		// reportChange only fires when the registry actually changed.
		reportChange(contenoxDir, map[string]any{
			"created":            res.Created,
			"updated":            res.Updated,
			"disabled":           res.Disabled,
			"skipped_name_taken": res.Skipped,
			"unchanged":          len(res.Unchanged),
		})
	}
	return notable
}

// printSyncProblems shows what a discovery pass could not act on. Written to
// stderr so it never contaminates a roster someone is parsing.
func printSyncProblems(w io.Writer, results []agentdecl.SyncResult) {
	for _, r := range results {
		switch r.Action {
		case agentdecl.ActionRefused:
			fmt.Fprintf(w, "refused  %s: %s\n", r.Source, r.Reason)
		case agentdecl.ActionIgnored:
			fmt.Fprintf(w, "ignored  %s: %s\n", r.Source, r.Reason)
		}
		for _, u := range r.Unmapped {
			fmt.Fprintf(w, "not carried  %s: %s — %s\n", r.Name, u.Field, u.Reason)
		}
	}
}

// syncDeclaredAgents transpiles every Markdown declaration into
// contenoxDir/generated and returns that directory, empty when there is nothing
// to discover. An untranspilable source is reported and skipped.
func syncDeclaredAgents(ctx context.Context, contenoxDir, homeDir string, tracker libtracker.ActivityTracker) (string, []agentdecl.SyncResult) {
	reportErr, reportChange, end := tracker.Start(ctx, "sync", "declared_agents")
	defer end()

	contenoxDirs := []string{contenoxDir}
	if homeDir != "" {
		contenoxDirs = append(contenoxDirs, homeDir)
	}
	// The home directory is a root like any other: Claude Code reads both
	// .claude/agents/ (project) and ~/.claude/agents/ (user), and an agent kept
	// in the second one is still "read where it is".
	roots := workspaceRootsForSync(contenoxDir)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, home)
	}
	sourceDirs := agentdecl.DiscoverSourceDirs(contenoxDirs, roots)
	generated := filepath.Join(contenoxDir, agentdecl.GeneratedDirName)
	if len(sourceDirs) == 0 {
		if _, err := os.Stat(generated); err != nil {
			return "", nil
		}
	}

	cfg, err := agentdecl.Load(homeDir, contenoxDir)
	if err != nil {
		reportErr(err)
		return "", nil
	}

	// Skills live beside the agents that use them, in the same roots, with the
	// same nearest-wins precedence.
	// Relative to the project root, because that is where the agent's file tool
	// is rooted; a skill it cannot address is left out.
	var workspaceRoot string
	if roots := workspaceRootsForSync(contenoxDir); len(roots) > 0 {
		workspaceRoot = roots[0]
	}
	skills := agentdecl.DiscoverSkills(contenoxDirs, workspaceRoot)

	results, err := agentdecl.Sync(sourceDirs, generated, cfg, agentdecl.WithSkills(skills))
	if err != nil {
		reportErr(err)
		return "", nil
	}

	changed := map[string]any{}
	for _, r := range results {
		switch r.Action {
		case agentdecl.ActionRefused:
			changed["refused:"+r.Source] = r.Reason
		case agentdecl.ActionIgnored:
			changed["ignored:"+r.Source] = r.Reason
		case agentdecl.ActionCreated, agentdecl.ActionUpdated:
			changed[string(r.Action)+":"+r.Name] = r.Source
		}
		for _, u := range r.Unmapped {
			changed["unmapped:"+r.Name+"."+u.Field] = u.Reason
		}
	}
	if len(changed) > 0 {
		reportChange(generated, changed)
	}
	if _, err := os.Stat(generated); err != nil {
		return "", results
	}
	return generated, results
}

// workspaceRootsForSync is the project a contenox directory belongs to.
func workspaceRootsForSync(contenoxDir string) []string {
	parent := filepath.Dir(filepath.Clean(contenoxDir))
	if parent == "." || parent == string(filepath.Separator) {
		return nil
	}
	return []string{parent}
}
