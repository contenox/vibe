package contenoxcli

import (
	"context"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/chainagents"
)

// discoverChainAgents runs one chain-agent discovery pass over the two
// directories every other contenox config file is resolved through — the
// workspace .contenox/ first, then ~/.contenox/ — so a chain named by the
// agent-* convention in either, and the agent-shaped chains `contenox init`
// ships, are declared as fleet-dispatchable agents without an operator
// registering anything by hand.
//
// It is BEST EFFORT by design. Discovery only seeds the registry; the registry
// is what the fleet actually resolves against, so a failed pass degrades to
// "the fleet has whatever was already declared" rather than to an editor that
// will not start. The outcome is REPORTED either way, because an agent silently
// failing to appear is exactly the kind of half-built surface this must not
// manufacture.
//
// The outcome is telemetry, not a message: this runs unattended during editor
// and fleet bootstrap, behind a DiscoverAgents callback nobody is watching, and
// it asks the operator for nothing — the registry either gained agents or kept
// the ones it had. So it goes to the tracker, which is where an operator who
// wants to know reads it (`--trace`, or the telemetry log), and never to stderr,
// which under `contenox acp` belongs to the editor's protocol pane.
//
// A nil tracker degrades to Noop, the same nil-tolerance reportrouter.New gives
// its Deps.Tracker, so a caller that has no tracker is not forced to invent one.
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

	// WithTracker, not the plain Discover: the service reports a refused chain
	// file and a vanished agent through the tracker, and the plain form passes
	// a Noop — which would send both diagnostics nowhere.
	res, err := chainagents.DiscoverWithTracker(ctx, agents, tracker, roots...)
	if err != nil {
		reportErr(err)
		return
	}
	if len(res.Created) > 0 || len(res.Updated) > 0 || len(res.Disabled) > 0 || len(res.Skipped) > 0 {
		// reportChange, not a second start/end: the registry actually changed,
		// and that is the audit-worthy half of this pass. An all-unchanged pass
		// reports nothing beyond the operation itself, as before.
		reportChange(contenoxDir, map[string]any{
			"created":            res.Created,
			"updated":            res.Updated,
			"disabled":           res.Disabled,
			"skipped_name_taken": res.Skipped,
			"unchanged":          len(res.Unchanged),
		})
	}
}
