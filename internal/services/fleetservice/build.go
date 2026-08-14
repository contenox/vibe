// build.go composes an in-process fleet: agent registry, agentinstance
// kernel, operator inbox, report router, and the fleet Service, so a host
// process can dispatch missions as subagents of itself.
package fleetservice

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/operatorinbox"
	"github.com/contenox/contenox/internal/services/reportrouter"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
)

// InProcessDeps are the collaborators BuildInProcess wires an embedded fleet from; DB, Bus, and Missions are required, and optional fields are narrow seams so fleetservice need not import a surface package.
type InProcessDeps struct {
	DB       libdb.DBManager
	Bus      libbus.Messenger
	Missions missionservice.Service

	// ProjectRoot is the working directory a dispatched mission defaults to when the request names none and no allowlist is configured (see service.resolveCwd).
	ProjectRoot string

	// WorkspaceRoots bounds the cwd a dispatch request may name; nil configures no allowlist, leaving ProjectRoot as default and any absolute cwd acceptable.
	WorkspaceRoots *vfs.Factory

	// WorkspaceID is the workspace the host publishes mission events under, forwarded to every chain-kind child; empty leaves the child to its own default.
	WorkspaceID string

	// Tracker degrades to a Noop when nil, exactly as New does.
	Tracker libtracker.ActivityTracker

	// PolicySource backs the creation-time HITL policy existence check; nil skips the check.
	PolicySource hitlservice.PolicySource

	// HITL judges a viewer-less unit's permission request against its mission's
	// envelope; nil leaves the hub cancelling every such request.
	HITL hitlservice.Service

	// DiscoverAgents optionally seeds the agent registry before the kernel is built; best-effort, so it should log and degrade rather than fail.
	DiscoverAgents func(ctx context.Context, agents agentregistryservice.Service)

	// SessionDeliverer optionally wraps the report router's live-parent delivery; nil means kernel-only delivery.
	SessionDeliverer func(kernel agentinstance.Manager) reportrouter.SessionDeliverer

	// AgentSupervisor is the router's optional autonomous edge; nil leaves every question to a human.
	AgentSupervisor reportrouter.AgentSupervisor

	// Stderr is where a dispatched unit's stderr lands; nil defaults to os.Stderr.
	Stderr io.Writer
}

// BuildInProcess embeds the fleet a host process dispatches missions through, returning the fleet Service, the agent registry, and one teardown func that stops the report router, closes the kernel, and reaps every dispatched child subprocess; the host must run it on shutdown.
func BuildInProcess(ctx context.Context, deps InProcessDeps) (Service, agentregistryservice.Service, func(), error) {
	agents := agentregistryservice.New(deps.DB)

	if deps.DiscoverAgents != nil {
		deps.DiscoverAgents(ctx, agents)
	}

	stderr := deps.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	kernelOpts := []agentinstance.Option{agentinstance.WithStderr(stderr)}
	if deps.HITL != nil {
		kernelOpts = append(kernelOpts, agentinstance.WithPermissionFallback(
			NewUnattendedPermissionAnswerer(UnattendedPermissionDeps{
				HITL:     deps.HITL,
				Missions: deps.Missions,
				Sink:     taskengine.NoopTaskEventSink{},
				Tracker:  deps.Tracker,
			})))
	}
	if deps.WorkspaceID != "" {
		kernelOpts = append(kernelOpts, agentinstance.WithWorkspaceID(deps.WorkspaceID))
	}
	kernel := agentinstance.New(agents, kernelOpts...)

	operatorInbox := operatorinbox.New(deps.DB, operatorinbox.WithEventPublisher(deps.Bus))

	var sessions reportrouter.SessionDeliverer = kernel
	if deps.SessionDeliverer != nil {
		sessions = deps.SessionDeliverer(kernel)
	}
	router, err := reportrouter.New(reportrouter.Deps{
		Bus:             deps.Bus,
		Sessions:        sessions,
		Inbox:           operatorInbox,
		Tracker:         deps.Tracker,
		AgentSupervisor: deps.AgentSupervisor,
	})
	if err != nil {
		_ = kernel.Close()
		return nil, nil, nil, fmt.Errorf("build report router: %w", err)
	}
	stopRouter, err := router.Start(ctx)
	if err != nil {
		_ = kernel.Close()
		return nil, nil, nil, fmt.Errorf("start report router: %w", err)
	}

	var opts []Option
	if deps.PolicySource != nil {
		opts = append(opts, WithPolicyValidator(hitlservice.NewPolicyValidator(deps.PolicySource, runtimetypes.LocalTenantID, "")))
		// Read-only over the same source: a nil KVReader leaves the approval
		// and checkpoint seams unbound, so this instance can only load and
		// parse a policy's compute half.
		if reader, ok := hitlservice.New(deps.PolicySource, runtimetypes.LocalTenantID, nil, deps.Tracker).(hitlservice.ComputeBoundsReader); ok {
			opts = append(opts, WithComputeBounds(reader))
		}
	}
	if raw := clikv.Read(ctx, runtimetypes.New(deps.DB.WithoutTransaction()), MaxParallelConfigKey); raw != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			opts = append(opts, WithMaxParallel(n))
		}
	}
	fleet := New(kernel, agents, deps.Missions, deps.WorkspaceRoots, deps.ProjectRoot, deps.Tracker, opts...)

	// mission stop from any process reaches this host via the shared bus; the
	// kernel hosting the unit reaps it (see stop.go).
	stopTeardown, err := runStatusTeardown(ctx, deps.Bus, deps.Missions, kernel)
	if err != nil {
		stopRouter()
		_ = kernel.Close()
		return nil, nil, nil, err
	}

	// Every unit this host opens dies with it, so a host coming up is the
	// moment to collect what a dead one left behind.
	sweepAbandonedMissions(ctx, deps.Missions, deps.Tracker)

	stop := func() {
		stopTeardown()
		stopRouter()
		_ = kernel.Close()
	}
	return fleet, agents, stop, nil
}

func sweepAbandonedMissions(ctx context.Context, missions missionservice.Service, tracker libtracker.ActivityTracker) {
	if missions == nil {
		return
	}
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	reclaimed, err := missions.SweepAbandoned(ctx)
	if err != nil {
		reportErr, _, end := tracker.Start(ctx, "sweep", "abandoned_missions")
		reportErr(fmt.Errorf("fleetservice: reclaiming abandoned missions failed; the fleet is up either way: %w", err))
		end()
		return
	}
	if reclaimed > 0 {
		_, reportChange, end := tracker.Start(ctx, "sweep", "abandoned_missions")
		reportChange("reclaimed", reclaimed)
		end()
	}
}
