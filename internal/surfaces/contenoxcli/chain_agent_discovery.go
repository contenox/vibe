package contenoxcli

import (
	"context"
	"log/slog"

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
// will not start. The outcome is logged either way, because an agent silently
// failing to appear is exactly the kind of half-built surface this must not
// manufacture.
func discoverChainAgents(ctx context.Context, agents agentregistryservice.Service, contenoxDir string) {
	roots := []string{contenoxDir}
	if homeDir, err := globalContenoxDir(); err == nil {
		roots = append(roots, homeDir)
	}
	res, err := chainagents.Discover(ctx, agents, roots...)
	if err != nil {
		slog.Warn("contenox acp: chain-agent discovery failed; the fleet keeps the agents already declared",
			"error", err, "roots", roots)
		return
	}
	if len(res.Created) > 0 || len(res.Updated) > 0 || len(res.Disabled) > 0 || len(res.Skipped) > 0 {
		slog.Info("contenox acp: chain agents discovered",
			"created", res.Created, "updated", res.Updated,
			"disabled", res.Disabled, "skipped_name_taken", res.Skipped,
			"unchanged", len(res.Unchanged))
	}
}
