// build.go composes an IN-PROCESS fleet — the whole embeddable stack a host
// process needs to dispatch missions as subagents of ITSELF: the agent
// registry, the agentinstance kernel, the operator inbox, the report router,
// and the fleet Service over them. It exists because that composition is a
// SERVICE concern, not a surface one: the ACP editor and the `contenox mission
// fire` verb both embed the same fleet, and before this constructor existed
// the wiring lived in the CLI surface (acp_cmd.go) — exactly the
// business-logic-in-a-surface violation the build-on-services rule names.
// Surfaces now call BuildInProcess and keep only their surface-specific
// adapters (live-session delivery, agent-answer offers), passed in through the
// narrow seams below.
package fleetservice

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	"github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/operatorinbox"
	"github.com/contenox/beam/internal/services/reportrouter"
	"github.com/contenox/beam/internal/store/runtimetypes"
)

// InProcessDeps are the collaborators BuildInProcess wires an embedded fleet
// from. DB, Bus, and Missions are required and MUST be the same handles the
// host process already opened — sharing them is what makes the embedded
// report router see a dispatched unit's cross-process ReportAddedEvent (the
// unit publishes on the SQLite bus over the same database file).
//
// The optional fields are the seams that keep the dependency direction clean:
// fleetservice must not import a surface (contenoxcli, acpsvc), so anything a
// surface knows — how to reach a live editor session, how to offer a unit's
// question to the supervising agent, which directories to discover chain
// agents from — arrives as a narrow function or interface, or stays absent.
type InProcessDeps struct {
	DB       libdb.DBManager
	Bus      libbus.Messenger
	Missions missionservice.Service

	// ProjectRoot is the working directory a dispatched mission defaults to when
	// the request names none (the host's cwd, typically). See service.resolveCwd
	// for how it interacts with a configured workspace-root allowlist.
	ProjectRoot string

	// Tracker degrades to a Noop when nil, exactly as New does.
	Tracker libtracker.ActivityTracker

	// PolicySource backs the creation-time envelope existence check
	// (WithPolicyValidator): a dispatch naming a nonexistent HITL policy is
	// refused at the door instead of silently running under the default gate.
	// Nil skips the check — the same meaning as building without the option.
	PolicySource hitlservice.PolicySource

	// DiscoverAgents optionally seeds the agent registry before the kernel is
	// built — the chain-agent discovery pass that declares the operator's
	// agent-*.json chains dispatchable. It is a hook, not an import, because
	// discovery roots are the HOST's knowledge (its .contenox dirs). Best-effort
	// by convention: implementations log and degrade rather than failing the
	// build, so a failed pass leaves the fleet whatever was already declared.
	DiscoverAgents func(ctx context.Context, agents agentregistryservice.Service)

	// SessionDeliverer optionally wraps the report router's live-parent
	// delivery. It receives the freshly built kernel so a host can compose
	// "try my own chat surface first, the kernel second" (the editor's
	// missionReportDeliverer). Nil means kernel-only delivery — right for a
	// host with no chat surface of its own (the CLI fire path), where every
	// parentless report falls through to the operator inbox anyway.
	SessionDeliverer func(kernel agentinstance.Manager) reportrouter.SessionDeliverer

	// AgentSupervisor is the router's optional autonomous edge: offer a unit's
	// question to the agent driving the firing session, when the envelope
	// allows. Nil leaves every question to a human (the router's default).
	AgentSupervisor reportrouter.AgentSupervisor

	// Stderr is where a dispatched unit's stderr lands (the host's log), so a
	// unit that fails to boot is diagnosable. Nil defaults to os.Stderr.
	Stderr io.Writer
}

// BuildInProcess embeds the fleet a host process dispatches missions through —
// the ontology's in-process subagent kernel (a mission is a subagent of THIS
// process). It mirrors serve's composition (agentregistryservice +
// agentinstance kernel + operatorinbox + reportrouter + fleetservice)
// minimally, over the db and bus the host already opened.
//
// It returns the fleet Service, the agent registry it resolves against (the
// host's mission-agent resolver), and ONE teardown that stops the report
// router and Closes the kernel — reaping every dispatched child subprocess —
// which the host MUST run on shutdown: the kernel's children are
// process-bound, so mission lifetime ≤ host-process lifetime.
//
// The fleet-width cap is read from the operator's fleet-max-parallel config
// (MaxParallelConfigKey) over the shared db; absent or unparsable keeps the
// enforced default (DefaultMaxParallel). The knob and the gate share one key
// constant so they cannot drift.
func BuildInProcess(ctx context.Context, deps InProcessDeps) (Service, agentregistryservice.Service, func(), error) {
	agents := agentregistryservice.New(deps.DB)

	// Declare the operator's chain agents (and any registered external agents)
	// as dispatchable — the privileged discovery lane, safe. The hook is
	// best-effort by convention (see InProcessDeps.DiscoverAgents).
	if deps.DiscoverAgents != nil {
		deps.DiscoverAgents(ctx, agents)
	}

	// The kernel is an embeddable LIBRARY, not a serve-bound service. WithStderr
	// routes a dispatched unit's stderr to the host's log so a unit that fails
	// to boot is diagnosable. No unattended permission answerer is wired here
	// (unlike serve): a dispatched unit runs bounded/ungated work or `--auto`,
	// and routing a unit's permission ask into the host's own approval surface
	// is a named follow-up.
	stderr := deps.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	kernel := agentinstance.New(agents, agentinstance.WithStderr(stderr))

	operatorInbox := operatorinbox.New(deps.DB, operatorinbox.WithEventPublisher(deps.Bus))

	// The report router delivers a fired unit's report onto whoever fired it —
	// the host's live session surface first when one is wired, the kernel's own
	// sessions second — falling back to the operator inbox when no live parent
	// owns it. It runs off the shared SQLite bus, so a unit's cross-process
	// ReportAddedEvent reaches it.
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
		// Same envelope-existence guard the serve path enforces, over the host's
		// policy files, so a dispatch naming a typo'd envelope is refused here too.
		opts = append(opts, WithPolicyValidator(hitlservice.NewPolicyValidator(deps.PolicySource, runtimetypes.LocalTenantID, "")))
	}
	// The operator's fleet-width cap. Absent or unparsable keeps the enforced
	// default (DefaultMaxParallel).
	if raw := clikv.Read(ctx, runtimetypes.New(deps.DB.WithoutTransaction()), MaxParallelConfigKey); raw != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			opts = append(opts, WithMaxParallel(n))
		}
	}
	fleet := New(kernel, agents, deps.Missions, nil, deps.ProjectRoot, deps.Tracker, opts...)

	// `mission stop` from ANY process reaches this host: the terminal-status
	// event travels the shared SQLite bus, and the kernel hosting the unit
	// reaps it (see stop.go).
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
