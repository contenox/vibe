// Package reportrouter delivers missionservice's report, ask, status, and
// plan-revision events to the session that fired the mission, falling back
// to the operator inbox when none is live. Reports and asks always land
// somewhere; status and plan-revision events are dropped when undeliverable,
// since both are already durable on the mission record.
package reportrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	libbus "github.com/contenox/contenox/internal/libbus"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/operatorinbox"
	"github.com/contenox/libacp"
)

// SessionDeliverer injects a report update into a supervising session's
// stream. A non-nil error means the session is unreachable, which the router
// treats as "route to the inbox", never a fault.
type SessionDeliverer interface {
	DeliverToSession(ctx context.Context, sessionID libacp.SessionID, n libacp.SessionNotification) error
}

// InboxWriter records a report that reached no live supervisor.
type InboxWriter interface {
	Add(ctx context.Context, item *operatorinbox.Item) error
}

// Subscriber is the narrow slice of the event bus the router consumes.
type Subscriber interface {
	Stream(ctx context.Context, subject string, ch chan<- []byte) (libbus.Subscription, error)
}

// Deps are the router's collaborators. Bus, Sessions, and Inbox are
// required; Tracker defaults to a Noop when nil.
type Deps struct {
	Bus      Subscriber
	Sessions SessionDeliverer
	Inbox    InboxWriter
	Tracker  libtracker.ActivityTracker
	// AgentSupervisor is optional and consulted only for a question, after it
	// has already been delivered to the firing session. Nil means every
	// question goes to a human only.
	AgentSupervisor AgentSupervisor
}

// AgentSupervisor offers a unit's question to the agent driving the session
// that fired the mission. A decline is reported as a nil error: the question
// is already durable and delivered, so nothing here can lose it.
type AgentSupervisor interface {
	OfferToSupervisingAgent(ctx context.Context, ev missionservice.AttentionAskedEvent) error
}

// Router subscribes to report-added events and routes each to a session or
// the inbox. Build with New, run with Start.
type Router struct {
	deps Deps
}

// New validates deps and returns a Router. Bus, Sessions, and Inbox are
// required; a nil value is rejected immediately.
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

// streamBuffer bounds the per-subscription event channel size.
const streamBuffer = 64

// Start subscribes to every subject the router handles and processes events
// until the returned stop function is called or ctx is cancelled. It returns
// only after the subscriptions are established, so events published after
// Start returns are not missed. stop cancels the loops, unsubscribes, and
// waits for all loop goroutines to exit before returning.
func (r *Router) Start(ctx context.Context) (func(), error) {
	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	var subs []libbus.Subscription
	stop := func() {
		cancel()
		for _, sub := range subs {
			_ = sub.Unsubscribe()
		}
		wg.Wait()
	}
	subscribe := func(subject string, handle func(context.Context, []byte)) error {
		ch := make(chan []byte, streamBuffer)
		sub, err := r.deps.Bus.Stream(ctx, subject, ch)
		if err != nil {
			return fmt.Errorf("reportrouter: subscribe %q: %w", subject, err)
		}
		subs = append(subs, sub)
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.loop(runCtx, ch, handle)
		}()
		return nil
	}
	for _, lane := range []struct {
		subject string
		handle  func(context.Context, []byte)
	}{
		{missionservice.ReportAddedSubject, r.handle},
		{missionservice.AttentionAskedSubject, r.handleAsk},
		{missionservice.StatusChangedSubject, r.handleStatus},
		{missionservice.PlanRevisedSubject, r.handlePlan},
	} {
		if err := subscribe(lane.subject, lane.handle); err != nil {
			stop()
			return nil, err
		}
	}
	return stop, nil
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

// routeAsk delivers a unit's question to the session that fired its mission.
// There is no inbox fallback: an ask is already durable and answerable from
// hitlservice's own queue, so an unreachable parent leaves it pending there
// rather than lost.
func (r *Router) routeAsk(ctx context.Context, ev missionservice.AttentionAskedEvent) {
	reportErr, reportChange, end := r.deps.Tracker.Start(ctx, "fleet", "route_attention_ask",
		"mission_id", ev.MissionID, "ask_id", ev.AskID)
	defer end()

	if ev.ParentSessionID == "" {
		// Operator-fired mission: their queue is the inbox already.
		reportChange("routed", "queue_operator_fired")
		return
	}
	reportChange("parent_session_id", ev.ParentSessionID)
	if err := r.deps.Sessions.DeliverToSession(ctx, libacp.SessionID(ev.ParentSessionID), buildAskNotification(ev)); err != nil {
		// Parent not live: the ask stays pending in the queue, not lost.
		reportChange("routed", "queue_parent_not_live")
		reportErr(err)
		return
	}
	reportChange("routed", "session")

	// Offered after delivery so the human always sees the question first,
	// whether or not the agent accepts it.
	if r.deps.AgentSupervisor != nil {
		if err := r.deps.AgentSupervisor.OfferToSupervisingAgent(ctx, ev); err != nil {
			reportErr(err)
		}
	}
}

// askUpdateMeta namespaces the ask attribution on a delivered update so a
// client renders it as an answerable ask rather than plain chat text.
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

// buildAskNotification renders a question as an agent_message_chunk carrying
// the contenox.missionAsk meta a client builds its answer box from.
func buildAskNotification(ev missionservice.AttentionAskedEvent) libacp.SessionNotification {
	update := libacp.NewAgentMessageChunk(askText(ev))
	// Own message id: without one, chunks fold into whatever message the
	// session is currently streaming.
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

// loop drains one subscription's channel into handle until ctx is cancelled
// or the channel closes.
func (r *Router) loop(ctx context.Context, ch <-chan []byte, handle func(context.Context, []byte)) {
	for {
		select {
		case <-ctx.Done():
			return
		case data, ok := <-ch:
			if !ok {
				return
			}
			handle(ctx, data)
		}
	}
}

func (r *Router) handle(ctx context.Context, data []byte) {
	var ev missionservice.ReportAddedEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		// Malformed event: record it and continue rather than wedge the loop.
		reportErr, _, end := r.deps.Tracker.Start(ctx, "fleet", "route_report")
		reportErr(fmt.Errorf("reportrouter: decode event: %w", err))
		end()
		return
	}
	r.route(ctx, ev)
}

// route is called directly by tests to assert the branch table without
// driving the bus.
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
		// Parent named but unreachable: fall back to inbox, marked as missed
		// rather than dropped.
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

// reportUpdateMeta namespaces the mission-report attribution a delivered
// update carries in its ACP _meta envelope.
type reportUpdateMeta struct {
	Report *reportAttribution `json:"contenox.missionReport,omitempty"`
}

type reportAttribution struct {
	MissionID string `json:"missionId"`
	ReportID  string `json:"reportId"`
	Kind      string `json:"kind"`
	AgentName string `json:"agentName,omitempty"`
}

// buildReportNotification renders a report as an agent_message_chunk plus a
// _meta envelope carrying the mission/report attribution.
func buildReportNotification(ev missionservice.ReportAddedEvent) libacp.SessionNotification {
	update := libacp.NewAgentMessageChunk(reportText(ev))
	// Own message id, same reasoning as buildAskNotification.
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

// reportText composes a deterministic, human-readable report body.
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

// Status changes and plan revisions are already durable on the mission
// record and re-readable via Get, so routing them is a best-effort
// notification, not a delivery: an unreachable parent means DROP (recorded
// on the tracker), never a write to the inbox.

func (r *Router) handleStatus(ctx context.Context, data []byte) {
	var ev missionservice.StatusChangedEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		reportErr, _, end := r.deps.Tracker.Start(ctx, "fleet", "route_status_change")
		reportErr(fmt.Errorf("reportrouter: decode status-changed event: %w", err))
		end()
		return
	}
	r.routeStatus(ctx, ev)
}

// routeStatus notifies the firing session of a terminal transition; dropped
// when there is no live parent (see the comment above).
func (r *Router) routeStatus(ctx context.Context, ev missionservice.StatusChangedEvent) {
	reportErr, reportChange, end := r.deps.Tracker.Start(ctx, "fleet", "route_status_change",
		"mission_id", ev.MissionID, "old_status", string(ev.OldStatus), "new_status", string(ev.NewStatus))
	defer end()

	if ev.ParentSessionID == "" {
		// Operator-fired: status is already readable from the mission itself.
		reportChange("routed", "dropped_operator_fired")
		return
	}
	reportChange("parent_session_id", ev.ParentSessionID)
	if err := r.deps.Sessions.DeliverToSession(ctx, libacp.SessionID(ev.ParentSessionID), buildStatusNotification(ev)); err != nil {
		reportChange("routed", "dropped_parent_not_live")
		reportErr(err)
		return
	}
	reportChange("routed", "session")
}

func (r *Router) handlePlan(ctx context.Context, data []byte) {
	var ev missionservice.PlanRevisedEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		reportErr, _, end := r.deps.Tracker.Start(ctx, "fleet", "route_plan_revision")
		reportErr(fmt.Errorf("reportrouter: decode plan-revised event: %w", err))
		end()
		return
	}
	r.routePlan(ctx, ev)
}

// routePlan notifies the firing session of a plan revision; same drop rule
// as routeStatus.
func (r *Router) routePlan(ctx context.Context, ev missionservice.PlanRevisedEvent) {
	reportErr, reportChange, end := r.deps.Tracker.Start(ctx, "fleet", "route_plan_revision",
		"mission_id", ev.MissionID, "revision", ev.Revision)
	defer end()

	if ev.ParentSessionID == "" {
		reportChange("routed", "dropped_operator_fired")
		return
	}
	reportChange("parent_session_id", ev.ParentSessionID)
	if err := r.deps.Sessions.DeliverToSession(ctx, libacp.SessionID(ev.ParentSessionID), buildPlanNotification(ev)); err != nil {
		reportChange("routed", "dropped_parent_not_live")
		reportErr(err)
		return
	}
	reportChange("routed", "session")
}

// statusUpdateMeta namespaces the terminal-transition attribution a
// delivered update carries.
type statusUpdateMeta struct {
	Status *statusAttribution `json:"contenox.missionStatus,omitempty"`
}

type statusAttribution struct {
	MissionID string `json:"missionId"`
	AgentName string `json:"agentName,omitempty"`
	Intent    string `json:"intent,omitempty"`
	OldStatus string `json:"oldStatus"`
	NewStatus string `json:"newStatus"`
	Reason    string `json:"reason,omitempty"`
}

// planUpdateMeta namespaces the plan-revision attribution, carrying the
// counts a client draws a progress line from without reading the mission
// back.
type planUpdateMeta struct {
	Plan *planAttribution `json:"contenox.missionPlan,omitempty"`
}

type planAttribution struct {
	MissionID   string `json:"missionId"`
	AgentName   string `json:"agentName,omitempty"`
	Revision    int    `json:"revision"`
	Explanation string `json:"explanation,omitempty"`
	EntryCount  int    `json:"entryCount"`
	Pending     int    `json:"pending"`
	InProgress  int    `json:"inProgress"`
	Completed   int    `json:"completed"`
}

// buildStatusNotification renders a terminal transition as an
// agent_message_chunk plus a dotted _meta envelope, same shape as a report.
func buildStatusNotification(ev missionservice.StatusChangedEvent) libacp.SessionNotification {
	update := libacp.NewAgentMessageChunk(statusText(ev))
	update.MessageID = statusMessageID(ev)
	meta := statusUpdateMeta{Status: &statusAttribution{
		MissionID: ev.MissionID,
		AgentName: ev.AgentName,
		Intent:    ev.Intent,
		OldStatus: string(ev.OldStatus),
		NewStatus: string(ev.NewStatus),
		Reason:    ev.Reason,
	}}
	if raw, err := json.Marshal(meta); err == nil {
		update.Meta = raw
	}
	return libacp.SessionNotification{
		SessionID: libacp.SessionID(ev.ParentSessionID),
		Update:    update,
	}
}

// statusMessageID derives the id from the (mission, old, new) transition,
// not a counter: Finish accepts only non-terminal-to-terminal transitions on
// an otherwise immutable mission, so the triple is collision-free and stable
// under the bus's at-least-once redelivery.
func statusMessageID(ev missionservice.StatusChangedEvent) string {
	return fmt.Sprintf("mission-status-%s-%s-%s", ev.MissionID, ev.OldStatus, ev.NewStatus)
}

// statusText composes the transition's body; the status value doubles as the
// verb so no per-status phrase table can fall out of sync.
func statusText(ev missionservice.StatusChangedEvent) string {
	unit := strings.TrimSpace(ev.AgentName)
	if unit == "" {
		unit = "a mission unit"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "unit %s %s", unit, ev.NewStatus)
	if reason := strings.TrimSpace(ev.Reason); reason != "" {
		b.WriteString(": ")
		b.WriteString(reason)
	}
	return b.String()
}

// buildPlanNotification renders a plan revision as the session update
// delivered into the firing session.
func buildPlanNotification(ev missionservice.PlanRevisedEvent) libacp.SessionNotification {
	update := libacp.NewAgentMessageChunk(planText(ev))
	// Revision is the mission's own monotonic counter, so it is already
	// collision-free and redelivery-stable.
	update.MessageID = fmt.Sprintf("mission-plan-%s-%d", ev.MissionID, ev.Revision)
	meta := planUpdateMeta{Plan: &planAttribution{
		MissionID:   ev.MissionID,
		AgentName:   ev.AgentName,
		Revision:    ev.Revision,
		Explanation: ev.Explanation,
		EntryCount:  ev.EntryCount,
		Pending:     ev.Pending,
		InProgress:  ev.InProgress,
		Completed:   ev.Completed,
	}}
	if raw, err := json.Marshal(meta); err == nil {
		update.Meta = raw
	}
	return libacp.SessionNotification{
		SessionID: libacp.SessionID(ev.ParentSessionID),
		Update:    update,
	}
}

// planText composes the revision body: what changed on the first line, the
// plan's current counts on the second.
func planText(ev missionservice.PlanRevisedEvent) string {
	unit := strings.TrimSpace(ev.AgentName)
	if unit == "" {
		unit = "a mission unit"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "unit %s revised its plan (rev %d)", unit, ev.Revision)
	if explanation := strings.TrimSpace(ev.Explanation); explanation != "" {
		b.WriteString(": ")
		b.WriteString(explanation)
	}
	fmt.Fprintf(&b, "\n%d entries: %d pending, %d in progress, %d completed",
		ev.EntryCount, ev.Pending, ev.InProgress, ev.Completed)
	return b.String()
}
