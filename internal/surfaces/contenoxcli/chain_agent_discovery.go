package contenoxcli

import (
	"context"

	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/chainagents"
	"github.com/contenox/contenox/libtracker"
)

// discoverChainAgents runs one chain-agent discovery pass over the workspace
// .contenox/ and ~/.contenox/, declaring chain-agent-* chains (and the shipped
// chains by id) as fleet-dispatchable agents. Best effort: a failed pass leaves
// the registry as it was, and the outcome is reported via tracker (not
// stderr) since this runs unattended. A nil tracker degrades to Noop.
func discoverChainAgents(ctx context.Context, agents agentregistryservice.Service, contenoxDir string, tracker libtracker.ActivityTracker) {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	roots := []string{contenoxDir}
	if homeDir, err := globalContenoxDir(); err == nil {
		roots = append(roots, homeDir)
	}
	reportErr, reportChange, end := tracker.Start(ctx, "discover", "chain_agents", "roots", roots)
	defer end()

	// DiscoverKept (not Discover) reports refused files and vanished agents through the tracker.
	res, err := chainagents.DiscoverKept(ctx, agents, tracker, nil, roots...)
	if err != nil {
		reportErr(err)
		return
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
}
