package contenoxcli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/libbus"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/chainagents"
	"github.com/contenox/contenox/libtracker"
)

// discoverChainAgents runs one chain-agent discovery pass over the workspace
// .contenox/ and ~/.contenox/. Best effort: a failed pass leaves the registry as
// it was, and the outcome is reported via tracker. A nil tracker degrades to Noop.
func discoverChainAgents(ctx context.Context, agents agentregistryservice.Service, contenoxDir string, tracker libtracker.ActivityTracker, deps DiscoverDeps) {
	discoverChainAgentsReporting(ctx, agents, contenoxDir, tracker, deps)
}

// DiscoverDeps carries what registering a declaration's own tool sources needs.
// Without a Store the pass registers nothing; without a Bus the rows are written
// while worker startup waits for a host that has one.
type DiscoverDeps struct {
	Store runtimetypes.Store
	Bus   libbus.Messenger
}

// discoverChainAgentsReporting is discoverChainAgents plus the sync results a
// caller may want to show a human.
func discoverChainAgentsReporting(ctx context.Context, agents agentregistryservice.Service, contenoxDir string, tracker libtracker.ActivityTracker, deps DiscoverDeps) []agentdecl.SyncResult {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	var notable []agentdecl.SyncResult
	roots := []string{contenoxDir}
	homeDir, homeErr := globalContenoxDir()
	if homeErr == nil {
		// system/ is scanned last, so an operator copy at the top level wins.
		roots = append(roots, homeDir, systemDir(homeDir))
	}

	generated, results := syncDeclaredAgents(ctx, contenoxDir, homeDir, tracker)
	if generated != "" {
		roots = append(roots, generated)
	}
	// Registered before discovery: the emitted chain names these toolsets.
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

// ensureProfileChain makes chainFile resolvable under contenoxDir before a
// surface loads it. Only `contenox init` preseeds the declarations and only the
// fleet's discovery pass compiles them, and both run after the load, so a fresh
// contenox directory could never boot a surface. Preseed writes just the missing
// files and Sync is idempotent, so a populated directory is left as it is. A set
// chainEnv is honoured untouched: the operator named that exact file, and a
// missing one stays the hard error they asked for.
func ensureProfileChain(ctx context.Context, contenoxDir, chainFile, chainEnv string, tracker libtracker.ActivityTracker) error {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	if strings.TrimSpace(os.Getenv(chainEnv)) != "" {
		return nil
	}
	if acpsvc.ChainFileResolves(contenoxDir, chainFile) {
		return nil
	}
	reportErr, reportChange, end := tracker.Start(ctx, "ensure", "profile_chain", "file", chainFile)
	defer end()

	created, err := agentdecl.Preseed(contenoxDir)
	if err != nil {
		reportErr(err)
		return fmt.Errorf("seed agent declarations: %w", err)
	}
	homeDir, err := globalContenoxDir()
	if err != nil {
		reportErr(err)
		return err
	}
	syncDeclaredAgents(ctx, contenoxDir, homeDir, tracker)
	if !acpsvc.ChainFileResolves(contenoxDir, chainFile) {
		err := fmt.Errorf("seeded %s but no chain %q was compiled", contenoxDir, chainFile)
		reportErr(err)
		return err
	}
	reportChange(contenoxDir, map[string]any{"seeded": len(created)})
	return nil
}

// printSyncProblems shows what a discovery pass could not act on, on stderr.
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
// to discover.
func syncDeclaredAgents(ctx context.Context, contenoxDir, homeDir string, tracker libtracker.ActivityTracker) (string, []agentdecl.SyncResult) {
	reportErr, reportChange, end := tracker.Start(ctx, "sync", "declared_agents")
	defer end()

	contenoxDirs := []string{contenoxDir}
	if homeDir != "" {
		contenoxDirs = append(contenoxDirs, homeDir)
	}
	// The home directory is a root like any other.
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

	// Skills live beside the agents that use them, with the same nearest-wins
	// precedence, resolved relative to the project root.
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
