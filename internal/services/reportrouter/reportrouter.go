// Package reportrouter is the supervision edge's delivery half: it subscribes to
// missionservice's ReportAddedEvent and routes each report to whoever fired the
// mission (docs/development/blueprints/acp/fleet-consolidation.md, "Mission
// mode", M3 — "its reports must reach whoever fired it, which is not always the
// operator").
//
//   - A mission fired FROM a session (ParentSessionID set) is supervised by that
//     session: the report is DELIVERED into its update stream, so a coordinating
//     agent or an attached operator sees "unit X reported: …" in the transcript
//     and can act on it on its next turn. This is async talk-back — the floor the
//     blueprint chose to build first, over synchronous blocking.
//   - A mission an operator fired directly (ParentSessionID empty) has no
//     upstream session; its report lands in the operator inbox instead.
//   - A mission whose parent session is GONE by the time the report arrives falls
//     back to the inbox rather than being lost. A supervisor ending must never
//     drop a report.
//
// # Why a bus consumer and not a call from AddReport
//
// missionservice publishes that a report exists and stays ignorant of sessions
// and inboxes; this package subscribes and owns the WHERE. That is the libbus
// decoupling idiom (CONTRIBUTING.md), and it is what makes this slice work today
// off a REST-added report AND compose automatically with the mission-tools slice
// when a unit files its own report through a tool — both paths go through
// AddReport, so both publish, so both route, with no coupling between the slices.
//
// # The best-effort invariant
//
// The report is the durable fact; routing is best-effort DELIVERY on top of it.
// The router runs asynchronously off the bus, so nothing it does can fail the
// AddReport that produced the event. Within the router, a delivery that cannot
// reach a live parent falls back to the inbox, and an inbox write that fails is
// reported to the tracker — never retried into a crash. The durable report
// remains readable via ListReports regardless.
package reportrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	libbus "github.com/contenox/beam/internal/libbus"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/operatorinbox"
	"github.com/contenox/beam/libacp"
)

// SessionDeliverer injects a report update into a supervising session's stream.
// It is the NARROWEST slice of agentinstance.Manager the router needs — inject
// one update into one session, nothing else — declared here (the consumer owns
// the interface it depends on) so this package never imports the kernel Manager
// wholesale. agentinstance.Manager satisfies it.
//
// A non-nil error means the session was not reachable (ErrNotFound: gone, or not
// hosted here); the router treats that as "route to the inbox", never a fault.
type SessionDeliverer interface {
	DeliverToSession(ctx context.Context, sessionID libacp.SessionID, n libacp.SessionNotification) error
}

// InboxWriter records a report that reached no live supervisor. The narrowest
// slice of operatorinbox.Service the router needs (Add only).
type InboxWriter interface {
	Add(ctx context.Context, item *operatorinbox.Item) error
}

// Subscriber is the narrow slice of the event bus the router consumes (Stream
// only). libbus.Messenger satisfies it.
type Subscriber interface {
	Stream(ctx context.Context, subject string, ch chan<- []byte) (libbus.Subscription, error)
}

// Deps are the router's collaborators. Bus, Sessions, and Inbox are required;
// Tracker degrades to a Noop when nil.
type Deps struct {
	Bus      Subscriber
	Sessions SessionDeliverer
	Inbox    InboxWriter
	Tracker  libtracker.ActivityTracker
	// AgentSupervisor is OPTIONAL and only ever consulted for a question, never a
	// report: after the question has been delivered to the firing session, it may
	// also be put to the AGENT driving that session, when the mission's envelope
	// says so. Nil leaves every question to a human, which is the default posture
	// and what a router built before this existed does.
	AgentSupervisor AgentSupervisor
}

// AgentSupervisor offers a unit's question to the agent driving the session that
// fired the mission. Declining is the normal case (the envelope forbids it, the
// cap is spent, the session is busy) and is reported as a nil error: the question
// is already durable and already delivered, so nothing here can lose it.
type AgentSupervisor interface {
	OfferToSupervisingAgent(ctx context.Context, ev missionservice.AttentionAskedEvent) error
}

// Router subscribes to report-added events and routes each to a session or the
// inbox. Build with New, run with Start.
type Router struct {
	deps Deps
}

// New validates deps and returns a Router. A nil Bus/Sessions/Inbox is a wiring
// defect (the router cannot function without any of them) and is rejected up
// front rather than surfacing later as a silent no-route.
func New(deps Deps) (*Router, error) {
	if deps.Bus == nil {
		return nil, fmt.Errorf("reportrouter: Bus is required")
	}
	if deps.Sessions == nil {
		return nil, fmt.Errorf("reportrouter: Sessions is required")
	}
	if deps.Inbox == nil {
		return nil, fmt.Errorf("reportrouter: Inbox is required")
	}
	if deps.Tracker == nil {
		deps.Tracker = libtracker.NoopTracker{}
	}
	return &Router{deps: deps}, nil
}

// streamBuffer bounds the per-router event channel. Reports are low-volume (a
// unit files a handful over a mission), so a small buffer is ample; the bus's
// own backpressure policy (drop on a full at-most-once backend, durable on
// SQLite) applies beyond it.
const streamBuffer = 64

// Start subscribes to ReportAddedSubject and processes events until the returned
// stop function is called or ctx is cancelled. It returns after the subscription
// is established, so an event published after Start returns is seen (the bus
// registers the subscription before Stream returns). The stop function cancels
// the loop, unsubscribes, and waits for the loop goroutine to exit, so no
// delivery is in flight once it returns.
func (r *Router) Start(ctx context.Context) (func(), error) {
	ch := make(chan []byte, streamBuffer)
	sub, err := r.deps.Bus.Stream(ctx, missionservice.ReportAddedSubject, ch)
	if err != nil {
		return nil, fmt.Errorf("reportrouter: subscribe %q: %w", missionservice.ReportAddedSubject, err)
	}
	// The second half of the edge: a unit's QUESTION. Reports say what a unit did;
	// asks say what it needs before it can go on, and both belong to whoever fired
	// the mission. Routing them from one place is what keeps "the supervisor hears
	// from its unit" one mechanism rather than two — the ask's own durability is
	// hitlservice's business, so nothing here can lose it.
	askCh := make(chan []byte, streamBuffer)
	askSub, err := r.deps.Bus.Stream(ctx, missionservice.AttentionAskedSubject, askCh)
	if err != nil {
		_ = sub.Unsubscribe()
		return nil, fmt.Errorf("reportrouter: subscribe %q: %w", missionservice.AttentionAskedSubject, err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r.loop(runCtx, ch)
	}()
	go func() {
		defer wg.Done()
		r.askLoop(runCtx, askCh)
	}()
	return func() {
		cancel()
		_ = sub.Unsubscribe()
		_ = askSub.Unsubscribe()
		wg.Wait()
	}, nil
}

func (r *Router) askLoop(ctx context.Context, ch <-chan []byte) {
	for {
		select {
		case <-ctx.Done():
			return
		case data, ok := <-ch:
			if !ok {
				return
			}
			r.handleAsk(ctx, data)
		}
	}
}

func (r *Router) handleAsk(ctx context.Context, data []byte) {
	var ev missionservice.AttentionAskedEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		reportErr, _, end := r.deps.Tracker.Start(ctx, "fleet", "route_attention_ask")
		reportErr(fmt.Errorf("reportrouter: decode attention-asked event: %w", err))
		end()
		return
	}
	r.routeAsk(ctx, ev)
}

// routeAsk delivers a unit's question into the session that fired its mission.
//
// There is deliberately NO inbox fallback here, and that is the difference from a
// report: an ask is already durable and answerable in its own queue the moment it
// is raised (hitlservice's approval store, which the operator inbox page and
// `contenox approvals` both read). A question with no live parent session is not
// lost — it is simply answered from the queue instead of from a chat. Reports need
// the inbox because nothing else keeps them; asks do not.
func (r *Router) routeAsk(ctx context.Context, ev missionservice.AttentionAskedEvent) {
	reportErr, reportChange, end := r.deps.Tracker.Start(ctx, "fleet", "route_attention_ask",
		"mission_id", ev.MissionID, "ask_id", ev.AskID)
	defer end()

	if ev.ParentSessionID == "" {
		// An operator fired this mission directly: their queue IS the inbox.
		reportChange("routed", "queue_operator_fired")
		return
	}
	reportChange("parent_session_id", ev.ParentSessionID)
	if err := r.deps.Sessions.DeliverToSession(ctx, libacp.SessionID(ev.ParentSessionID), buildAskNotification(ev)); err != nil {
		// The firing session is not live right now. The ask stays pending in the
		// queue, so this is a missed notification, not a lost question.
		reportChange("routed", "queue_parent_not_live")
		reportErr(err)
		return
	}
	reportChange("routed", "session")

	// The operator has the question in front of them now. Offer it to their agent
	// too, if the envelope allows: it may already know the answer from the very
	// conversation the mission was fired in. Ordered AFTER delivery on purpose —
	// the human sees the question first either way, and never learns of it only
	// because an agent happened to decline.
	if r.deps.AgentSupervisor != nil {
		if err := r.deps.AgentSupervisor.OfferToSupervisingAgent(ctx, ev); err != nil {
			reportErr(err)
		}
	}
}

// askUpdateMeta namespaces the question attribution the delivered update carries,
// so a client can recognise it as an ANSWERABLE ask rather than as chat text or a
// report. AskID is what an answer is given against, which is why it rides along:
// the surface that renders the question can answer it without a second lookup.
type askUpdateMeta struct {
	Ask *askAttribution `json:"contenox.missionAsk,omitempty"`
}

type askAttribution struct {
	MissionID string `json:"missionId"`
	AskID     string `json:"askId"`
	AgentName string `json:"agentName,omitempty"`
	Intent    string `json:"intent,omitempty"`
	Summary   string `json:"summary"`
	Detail    string `json:"detail,omitempty"`
}

// buildAskNotification renders a question as the session update delivered into
// the firing session: an agent_message_chunk (so it lands in the transcript the
// supervisor reads, human or agent) plus the `contenox.missionAsk` envelope a
// client renders its answer box from. The text is human-first and says plainly
// that the unit is WAITING, because a question that reads like a status line gets
// scrolled past.
func buildAskNotification(ev missionservice.AttentionAskedEvent) libacp.SessionNotification {
	update := libacp.NewAgentMessageChunk(askText(ev))
	// Its OWN message id, derived from the ask. Streamed chunks group by message
	// id, so an out-of-band delivery that carries none is folded into whatever
	// message the session is currently accumulating — which put two questions in
	// one bubble sharing one answer box, and left the second one unanswerable
	// behind the first's "answer sent" state. One ask, one message.
	update.MessageID = "mission-ask-" + ev.AskID
	meta := askUpdateMeta{Ask: &askAttribution{
		MissionID: ev.MissionID,
		AskID:     ev.AskID,
		AgentName: ev.AgentName,
		Intent:    ev.Intent,
		Summary:   ev.Summary,
		Detail:    ev.Detail,
	}}
	if raw, err := json.Marshal(meta); err == nil {
		update.Meta = raw
	}
	return libacp.SessionNotification{
		SessionID: libacp.SessionID(ev.ParentSessionID),
		Update:    update,
	}
}

// askText composes the human-readable body of a delivered question.
func askText(ev missionservice.AttentionAskedEvent) string {
	unit := strings.TrimSpace(ev.AgentName)
	if unit == "" {
		unit = "a mission unit"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "unit %s is waiting on you: %s", unit, strings.TrimSpace(ev.Summary))
	if detail := strings.TrimSpace(ev.Detail); detail != "" {
		b.WriteString("\n\n")
		b.WriteString(detail)
	}
	return b.String()
}

func (r *Router) loop(ctx context.Context, ch <-chan []byte) {
	for {
		select {
		case <-ctx.Done():
			return
		case data, ok := <-ch:
			if !ok {
				return
			}
			r.handle(ctx, data)
		}
	}
}

func (r *Router) handle(ctx context.Context, data []byte) {
	var ev missionservice.ReportAddedEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		// A malformed event is a bug in the publisher, not a routable report;
		// record it and move on rather than wedging the loop.
		reportErr, _, end := r.deps.Tracker.Start(ctx, "fleet", "route_report")
		reportErr(fmt.Errorf("reportrouter: decode event: %w", err))
		end()
		return
	}
	r.route(ctx, ev)
}

// route is the routing decision, exported to the package's own tests via a
// direct call so the branch table can be asserted without driving the bus.
func (r *Router) route(ctx context.Context, ev missionservice.ReportAddedEvent) {
	reportErr, reportChange, end := r.deps.Tracker.Start(ctx, "fleet", "route_report",
		"mission_id", ev.MissionID, "report_id", ev.Report.ID, "report_kind", string(ev.Report.Kind))
	defer end()

	if ev.ParentSessionID != "" {
		reportChange("parent_session_id", ev.ParentSessionID)
		n := buildReportNotification(ev)
		if err := r.deps.Sessions.DeliverToSession(ctx, libacp.SessionID(ev.ParentSessionID), n); err == nil {
			reportChange("routed", "session")
			return
		}
		// Parent named but not deliverable (the supervisor ended, or its session
		// is not hosted by this Manager): fall back to the inbox, marked so an
		// operator sees a supervisor was intended but missed. Never dropped.
		reportChange("routed", "inbox_parent_gone")
		if err := r.toInbox(ctx, ev, operatorinbox.ReasonParentGone); err != nil {
			reportErr(err)
		}
		return
	}

	reportChange("routed", "inbox_operator_fired")
	if err := r.toInbox(ctx, ev, operatorinbox.ReasonOperatorFired); err != nil {
		reportErr(err)
	}
}

func (r *Router) toInbox(ctx context.Context, ev missionservice.ReportAddedEvent, reason operatorinbox.Reason) error {
	item := &operatorinbox.Item{
		MissionID:       ev.MissionID,
		AgentName:       ev.AgentName,
		Intent:          ev.Intent,
		ParentSessionID: ev.ParentSessionID,
		Reason:          reason,
		Report:          ev.Report,
	}
	if err := r.deps.Inbox.Add(ctx, item); err != nil {
		return fmt.Errorf("reportrouter: write inbox item for mission %q: %w", ev.MissionID, err)
	}
	return nil
}

// reportUpdateMeta namespaces the mission-report attribution the delivered
// update carries in its ACP _meta envelope, so a consumer (beam's transcript,
// a coordinating agent) can recognise and render it as a mission report rather
// than as an ordinary agent message. The key is dotted-namespaced so it never
// collides with another producer's _meta.
type reportUpdateMeta struct {
	Report *reportAttribution `json:"contenox.missionReport,omitempty"`
}

type reportAttribution struct {
	MissionID string `json:"missionId"`
	ReportID  string `json:"reportId"`
	Kind      string `json:"kind"`
	AgentName string `json:"agentName,omitempty"`
}

// buildReportNotification renders a report as the session update delivered into
// the supervising session's stream: an agent_message_chunk (the kind that lands
// in the transcript the parent reads on its next turn), plus a _meta envelope
// carrying the mission/report attribution. The text is human-first — "unit X
// reported (kind): summary" — so it is legible whether the supervisor is a human
// at beam or another agent reading its transcript.
func buildReportNotification(ev missionservice.ReportAddedEvent) libacp.SessionNotification {
	update := libacp.NewAgentMessageChunk(reportText(ev))
	// One report, one message — same reason as an ask (see buildAskNotification):
	// without an id of its own a delivered report merges into the turn the session
	// happens to be streaming, so two reports read as one run-on message.
	update.MessageID = "mission-report-" + ev.Report.ID
	meta := reportUpdateMeta{Report: &reportAttribution{
		MissionID: ev.MissionID,
		ReportID:  ev.Report.ID,
		Kind:      string(ev.Report.Kind),
		AgentName: ev.AgentName,
	}}
	if raw, err := json.Marshal(meta); err == nil {
		update.Meta = raw
	}
	return libacp.SessionNotification{
		SessionID: libacp.SessionID(ev.ParentSessionID),
		Update:    update,
	}
}

// reportText composes the human-readable body of a delivered report. Kept
// deterministic and content-bounded: summary is a single line by validation,
// detail and refs are appended only when present.
func reportText(ev missionservice.ReportAddedEvent) string {
	unit := strings.TrimSpace(ev.AgentName)
	if unit == "" {
		unit = "a mission unit"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "unit %s reported (%s): %s", unit, ev.Report.Kind, ev.Report.Summary)
	if d := strings.TrimSpace(ev.Report.Detail); d != "" {
		b.WriteString("\n")
		b.WriteString(d)
	}
	if len(ev.Report.Refs) > 0 {
		b.WriteString("\nrefs: ")
		b.WriteString(strings.Join(ev.Report.Refs, ", "))
	}
	return b.String()
}
