// Package fleetboot builds the in-process mission fleet a surface embeds so
// `/mission` (or its beam-TUI equivalent) is dispatched as a subagent of THIS
// process, with the fired unit's report delivered back into the very session
// that fired it.
//
// This composition — agentregistry + agentinstance kernel + operatorinbox +
// reportrouter + fleetservice, plus the surface-specific adapters around it
// (live-session report delivery, the autonomous agent-answer offer) — used to
// live unexported inside contenoxcli/acp_cmd.go as buildInProcessFleet. It is
// extracted here per beam-tui.md section 3 item 5 so that `contenox acp` and
// the beam TUI's engine-bridge call the SAME exported constructor instead of
// maintaining two copies of the wiring.
//
// fleetboot must not import internal/surfaces/contenoxcli — contenoxcli
// imports fleetboot, not the other way around, or the two surfaces could not
// share this package. Anything a caller alone knows (which directories back
// its HITL policy source, which roots chain-agent discovery should walk)
// arrives already resolved, through Deps.
package fleetboot

import (
	"context"
	"os"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	"github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/fleetservice"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/reportrouter"
	"github.com/contenox/beam/internal/surfaces/acpsvc"
	"github.com/contenox/beam/libacp"
)

// Deps are the collaborators BuildInProcessFleet wires the embedded fleet
// from — all over the SAME shared db handle and bus the host process already
// opened. This was inProcessFleetDeps, unexported inside
// contenoxcli/acp_cmd.go, until beam-tui.md section 3 item 5 required one
// exported constructor both `contenox acp` and the beam TUI call.
type Deps struct {
	DB       libdb.DBManager
	Bus      libbus.Messenger
	Missions missionservice.Service
	Tracker  libtracker.ActivityTracker
	// Transport late-binds this connection's live acpsvc.Transport (nil until the
	// conn factory runs), so the report deliverer can reach the firing editor
	// session a mission was fired from.
	Transport func() *acpsvc.Transport
	// HITL is the durable ask store this process raises and answers questions
	// through — the same instance the mission tools use, so the supervisor's
	// answer and the unit's question meet in one place.
	HITL hitlservice.Service
	// PolicySource backs fleetservice.InProcessDeps.PolicySource (the
	// creation-time envelope existence check). Resolved by the caller: which
	// directories back it (a workspace .contenox/ vs $HOME/.contenox) is the
	// caller's own knowledge, and this package must not import contenoxcli to
	// compute it itself.
	PolicySource hitlservice.PolicySource
	// DiscoverAgents backs fleetservice.InProcessDeps.DiscoverAgents (the
	// chain-agent discovery pass that seeds the registry before the kernel is
	// built). Supplied pre-built by the caller for the same reason as
	// PolicySource above.
	DiscoverAgents func(ctx context.Context, agents agentregistryservice.Service)
}

// BuildInProcessFleet embeds the fleet a host process dispatches `/mission`
// through — the ontology's in-process subagent kernel (a mission is a
// subagent of THIS process). The composition itself lives in the service
// layer (fleetservice.BuildInProcess — the build-on-services rule); this
// adapter contributes only what the CALLING surface knows:
//
//   - live-parent delivery through this connection's late-bound transport
//     (missionReportDeliverer: the live session first, the kernel second);
//   - the autonomous answer edge serve has too — a unit's question may be
//     offered to the agent driving the session that fired it, when that
//     mission's envelope allows. Without this a fired mission could ask, and
//     be answered by a human, but never by the very agent holding the
//     conversation the mission came from — the case this is most useful in,
//     since Zed and other ACP clients render no answer box of their own;
//   - chain-agent discovery over the caller's own .contenox dirs, and the
//     same envelope-existence guard the serve path enforces over its policy
//     files — both arrive pre-built through Deps.DiscoverAgents/PolicySource
//     so this package need not know how a caller resolves them.
func BuildInProcessFleet(ctx context.Context, deps Deps) (fleetservice.Service, agentregistryservice.Service, func(), error) {
	// A dispatched mission's cwd defaults to the host process's working
	// directory (the project the editor/TUI was launched in) when the request
	// names none.
	projectRoot, _ := os.Getwd()
	return fleetservice.BuildInProcess(ctx, fleetservice.InProcessDeps{
		DB:             deps.DB,
		Bus:            deps.Bus,
		Missions:       deps.Missions,
		ProjectRoot:    projectRoot,
		Tracker:        deps.Tracker,
		PolicySource:   deps.PolicySource,
		DiscoverAgents: deps.DiscoverAgents,
		SessionDeliverer: func(kernel agentinstance.Manager) reportrouter.SessionDeliverer {
			return missionReportDeliverer{
				chat:   func() contenoxSessionDeliverer { return chatDeliverer(deps.Transport()) },
				kernel: kernel,
			}
		},
		AgentSupervisor: agentAnswerOffer{
			hitl:     deps.HITL,
			missions: deps.Missions,
			prompter: transportPrompter{transport: deps.Transport},
			tracker:  deps.Tracker,
		},
		Stderr: os.Stderr,
	})
}

// missionReportDeliverer is the report router's SessionDeliverer for the
// IN-PROCESS topology. Per the governing ontology, a mission is a subagent of
// the process that fired it, and its
// report must reach THAT parent — which, for a `/mission` fired from a live
// session, is one of the host's own native stdio sessions, NOT a kernel-owned
// unit. So the live transport is tried FIRST (Transport.DeliverToContenoxSession
// maps the firing session's contenox id onto the ACP connection and pushes the
// update); the kernel is tried second, for the rarer case of a mission fired by
// an in-process kernel unit's own session. When neither owns the firing session —
// it has ended, or was never here — both miss and the report router inboxes the
// report (the true no-live-parent fallback). This is exactly the live delivery
// the serve-forwarded topology could not make: there the firing session lived in
// a different process, so the report fell to the inbox as parent-gone.
type missionReportDeliverer struct {
	// chat resolves the ACP surface that may own the firing session, late-bound
	// because the host's lone transport does not exist yet when the router is
	// built. It returns nil when there is none to ask. Both shapes of that surface
	// implement the same one method: the editor's single *acpsvc.Transport, and
	// serve's *acpsvc.SessionRouter, which finds the right one of its many
	// connections by the same contenox session id.
	chat   func() contenoxSessionDeliverer
	kernel agentinstance.Manager
}

// contenoxSessionDeliverer injects an out-of-band update into a chat session
// addressed by its CONTENOX (internal) session id — the id a mission carries as
// its ParentSessionID. Declared here, where both implementations are consumed.
type contenoxSessionDeliverer interface {
	DeliverToContenoxSession(ctx context.Context, contenoxSessionID string, n libacp.SessionNotification) error
}

// chatDeliverer adapts a possibly-nil *acpsvc.Transport to the interface. The
// explicit nil check is load-bearing: returning a nil *Transport as an interface
// value yields a NON-nil interface holding a nil pointer, which would sail past
// the caller's nil guard and panic on the first delivery.
func chatDeliverer(t *acpsvc.Transport) contenoxSessionDeliverer {
	if t == nil {
		return nil
	}
	return t
}

var _ reportrouter.SessionDeliverer = missionReportDeliverer{}

func (d missionReportDeliverer) DeliverToSession(ctx context.Context, sessionID libacp.SessionID, n libacp.SessionNotification) error {
	if d.chat != nil {
		if chat := d.chat(); chat != nil {
			if err := chat.DeliverToContenoxSession(ctx, string(sessionID), n); err == nil {
				return nil
			}
		}
	}
	if d.kernel != nil {
		return d.kernel.DeliverToSession(ctx, sessionID, n)
	}
	return acpsvc.ErrSessionNotLive
}

// transportPrompter adapts the host's late-bound transport to the
// session-prompting capability the agent-answer offer needs. Late-bound for the
// same reason the deliverer is: the connection does not exist when the fleet is
// composed.
type transportPrompter struct {
	transport func() *acpsvc.Transport
}

func (p transportPrompter) PromptContenoxSession(ctx context.Context, contenoxSessionID, text string) error {
	t := p.transport()
	if t == nil {
		return acpsvc.ErrSessionNotLive
	}
	return t.PromptContenoxSession(ctx, contenoxSessionID, text)
}
