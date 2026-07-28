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
	"github.com/contenox/contenox/internal/libbus"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/operatorinbox"
	"github.com/contenox/contenox/internal/services/reportrouter"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// InProcessDeps are the collaborators BuildInProcess wires an embedded fleet
// from. DB, Bus, and Missions are required and must be the same handles the
// host process already opened. Optional fields are narrow seams so
// fleetservice need not import a surface package.
type InProcessDeps struct {
	DB       libdb.DBManager
	Bus      libbus.Messenger
	Missions missionservice.Service

	// ProjectRoot is the working directory a dispatched mission defaults to
	// when the request names none. See service.resolveCwd.
	ProjectRoot string

	// Tracker degrades to a Noop when nil, exactly as New does.
	Tracker libtracker.ActivityTracker

	// PolicySource backs the creation-time HITL policy existence check. Nil
	// skips the check.
	PolicySource hitlservice.PolicySource

	// DiscoverAgents optionally seeds the agent registry before the kernel
	// is built. Best-effort: should log and degrade rather than fail.
	DiscoverAgents func(ctx context.Context, agents agentregistryservice.Service)

	// SessionDeliverer optionally wraps the report router's live-parent
	// delivery. Nil means kernel-only delivery.
	SessionDeliverer func(kernel agentinstance.Manager) reportrouter.SessionDeliverer

	// AgentSupervisor is the router's optional autonomous edge. Nil leaves
	// every question to a human.
	AgentSupervisor reportrouter.AgentSupervisor

	// Stderr is where a dispatched unit's stderr lands. Nil defaults to
	// os.Stderr.
	Stderr io.Writer
}

// BuildInProcess embeds the fleet a host process dispatches missions
// through, mirroring serve's composition over the db and bus the host
// already opened. It returns the fleet Service, the agent registry, and one
// teardown that stops the report router and closes the kernel, reaping
// every dispatched child subprocess; the host must run it on shutdown.
func BuildInProcess(ctx context.Context, deps InProcessDeps) (Service, agentregistryservice.Service, func(), error) {
	agents := agentregistryservice.New(deps.DB)

	if deps.DiscoverAgents != nil {
		deps.DiscoverAgents(ctx, agents)
	}

	// No unattended permission answerer is wired here (unlike serve): a
	// dispatched unit runs bounded/ungated work or --auto.
	stderr := deps.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	kernel := agentinstance.New(agents, agentinstance.WithStderr(stderr))

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
	}
	if raw := clikv.Read(ctx, runtimetypes.New(deps.DB.WithoutTransaction()), MaxParallelConfigKey); raw != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			opts = append(opts, WithMaxParallel(n))
		}
	}
	fleet := New(kernel, agents, deps.Missions, nil, deps.ProjectRoot, deps.Tracker, opts...)

	// mission stop from any process reaches this host via the shared bus; the
	// kernel hosting the unit reaps it (see stop.go).
	stopTeardown, err := runStatusTeardown(ctx, deps.Bus, deps.Missions, kernel)
	if err != nil {
		stopRouter()
		_ = kernel.Close()
		return nil, nil, nil, err
	}

	stop := func() {
		stopTeardown()
		stopRouter()
		_ = kernel.Close()
	}
	return fleet, agents, stop, nil
}
