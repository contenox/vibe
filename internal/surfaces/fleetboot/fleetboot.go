// Package fleetboot builds the in-process mission fleet a surface embeds so
// `/mission` is dispatched as a subagent of the host process, with reports
// delivered back to the session that fired them.
//
// fleetboot must not import internal/surfaces/contenoxcli (contenoxcli
// imports fleetboot). Caller-specific knowledge — HITL policy source,
// chain-agent discovery roots — arrives pre-resolved through Deps.
package fleetboot

import (
	"context"
	"os"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/fleetservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/reportrouter"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
)

// Deps are the collaborators BuildInProcessFleet wires the embedded fleet
// from, sharing the host process's db handle and bus.
type Deps struct {
	DB       libdb.DBManager
	Bus      libbus.Messenger
	Missions missionservice.Service
	Tracker  libtracker.ActivityTracker
	// Transport late-binds the connection's live acpsvc.Transport; nil until
	// the conn factory runs.
	Transport func() *acpsvc.Transport
	// HITL is shared with the mission tools, so the supervisor's answer and
	// the unit's question meet in one store.
	HITL hitlservice.Service
	// PolicySource and DiscoverAgents are resolved by the caller (fleetboot
	// must not import contenoxcli to compute them itself).
	PolicySource   hitlservice.PolicySource
	DiscoverAgents func(ctx context.Context, agents agentregistryservice.Service)
	// WorkspaceID is the workspace the host stamps mission events with,
	// forwarded to dispatched chain-kind units. See
	// fleetservice.InProcessDeps.WorkspaceID.
	WorkspaceID string

	// DBPath is the database file the host opened, forwarded to dispatched
	// chain-kind units so their mission writes land where the mission row is.
	// See fleetservice.InProcessDeps.DBPath.
	DBPath string

	// WorkspaceRoots is the host's workspace-root allowlist, so a dispatched
	// unit is bounded by the same roots the firing session is. Nil configures
	// no allowlist. See fleetservice.InProcessDeps.WorkspaceRoots.
	WorkspaceRoots *vfs.Factory
}

// BuildInProcessFleet embeds the fleet a host process dispatches `/mission`
// through. The composition lives in fleetservice.BuildInProcess; this adapter
// contributes only what the calling surface knows: live-parent delivery
// through the late-bound transport, the autonomous answer offer to the
// firing agent, and discovery roots/policy source pre-built via Deps.
func BuildInProcessFleet(ctx context.Context, deps Deps) (fleetservice.Service, agentregistryservice.Service, func(), error) {
	// Defaults a dispatched mission's cwd to the host process's own, when the
	// request names none.
	projectRoot, _ := os.Getwd()
	return fleetservice.BuildInProcess(ctx, fleetservice.InProcessDeps{
		DB:             deps.DB,
		Bus:            deps.Bus,
		Missions:       deps.Missions,
		ProjectRoot:    projectRoot,
		WorkspaceRoots: deps.WorkspaceRoots,
		WorkspaceID:    deps.WorkspaceID,
		DBPath:         deps.DBPath,
		Tracker:        deps.Tracker,
		PolicySource:   deps.PolicySource,
		// Without it the kernel cancels every gated call a viewer-less unit
		// raises, so the ask is refused before any adjudicator can rule on it.
		HITL:           deps.HITL,
		DiscoverAgents: deps.DiscoverAgents,
		SessionDeliverer: func(kernel agentinstance.Manager) reportrouter.SessionDeliverer {
			return missionReportDeliverer{
				chat:   func() contenoxSessionDeliverer { return chatDeliverer(deps.Transport()) },
				kernel: kernel,
			}
		},
		Stderr: os.Stderr,
	})
}

// missionReportDeliverer is the report router's SessionDeliverer for the
// in-process topology: the live transport is tried first, the kernel second,
// and the report router inboxes the report when neither owns the firing
// session.
type missionReportDeliverer struct {
	// chat is late-bound (the transport doesn't exist yet when the router is
	// built) and may return nil.
	chat   func() contenoxSessionDeliverer
	kernel agentinstance.Manager
}

// contenoxSessionDeliverer injects an out-of-band update into a chat session
// addressed by its internal session id (a mission's ParentSessionID).
type contenoxSessionDeliverer interface {
	DeliverToContenoxSession(ctx context.Context, contenoxSessionID string, n libacp.SessionNotification) error
}

// chatDeliverer adapts a possibly-nil *acpsvc.Transport to the interface. The
// nil check is load-bearing: a nil *Transport boxed as an interface is a
// non-nil interface holding a nil pointer, which would bypass a nil guard.
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
// session-prompting capability the agent-answer offer needs.
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
