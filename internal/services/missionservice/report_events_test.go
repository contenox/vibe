package missionservice

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/stretchr/testify/require"
)

// fakePublisher captures every publish so a test can assert the subject and the
// decoded event, and can be primed to fail so the best-effort contract is
// testable.
type fakePublisher struct {
	mu       sync.Mutex
	subjects []string
	payloads [][]byte
	err      error
}

func (p *fakePublisher) Publish(_ context.Context, subject string, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subjects = append(p.subjects, subject)
	cp := make([]byte, len(data))
	copy(cp, data)
	p.payloads = append(p.payloads, cp)
	return p.err
}

func (p *fakePublisher) events(t *testing.T) []ReportAddedEvent {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ReportAddedEvent, 0, len(p.payloads))
	for i, raw := range p.payloads {
		require.Equal(t, ReportAddedSubject, p.subjects[i], "reports publish only on ReportAddedSubject")
		var ev ReportAddedEvent
		require.NoError(t, json.Unmarshal(raw, &ev))
		out = append(out, ev)
	}
	return out
}

// TestUnit_AddReport_PublishesReportAddedEvent proves AddReport announces a
// stored report on the bus, carrying the supervision edge (ParentSessionID) and
// the report itself, so a routing service can act without reading anything back.
func TestUnit_AddReport_PublishesReportAddedEvent(t *testing.T) {
	ctx, db := setupMissionDB(t)
	pub := &fakePublisher{}
	svc := New(db, WithEventPublisher(pub))

	m := newMission("supervise the sub-unit")
	m.ParentSessionID = "parent-session-42"
	require.NoError(t, svc.Create(ctx, m))

	report := &Report{Kind: ReportKindResult, Summary: "shipped the board", Detail: "all green", Refs: []string{"a.txt"}}
	require.NoError(t, svc.AddReport(ctx, m.ID, report))

	evs := pub.events(t)
	require.Len(t, evs, 1, "exactly one event per report added")
	ev := evs[0]
	require.Equal(t, m.ID, ev.MissionID)
	require.Equal(t, "parent-session-42", ev.ParentSessionID, "the supervision edge rides on the event")
	require.Equal(t, m.AgentName, ev.AgentName)
	require.Equal(t, m.Intent, ev.Intent)
	require.Equal(t, ReportKindResult, ev.Report.Kind)
	require.Equal(t, "shipped the board", ev.Report.Summary)
	require.Equal(t, report.ID, ev.Report.ID, "the event carries the assigned report id")
	require.Equal(t, m.ID, ev.Report.MissionID)
}

// TestUnit_AddReport_OperatorFiredEventHasEmptyEdge proves a mission with no
// parent session publishes an event with an empty ParentSessionID — the router's
// signal to route the report to the operator inbox.
func TestUnit_AddReport_OperatorFiredEventHasEmptyEdge(t *testing.T) {
	ctx, db := setupMissionDB(t)
	pub := &fakePublisher{}
	svc := New(db, WithEventPublisher(pub))

	m := newMission("operator fired this directly") // no ParentSessionID
	require.NoError(t, svc.Create(ctx, m))
	require.NoError(t, svc.AddReport(ctx, m.ID, &Report{Kind: ReportKindProgress, Summary: "halfway there"}))

	evs := pub.events(t)
	require.Len(t, evs, 1)
	require.Empty(t, evs[0].ParentSessionID, "an operator-fired mission carries no supervision edge")
}

// TestUnit_AddReport_PublishFailureDoesNotFailAddReport is the best-effort
// invariant: a publish error never turns a successfully-stored report into a
// failed AddReport, and the report remains durably readable.
func TestUnit_AddReport_PublishFailureDoesNotFailAddReport(t *testing.T) {
	ctx, db := setupMissionDB(t)
	pub := &fakePublisher{err: fmt.Errorf("bus is down")}
	svc := New(db, WithEventPublisher(pub))

	m := newMission("survive a broken bus")
	require.NoError(t, svc.Create(ctx, m))
	require.NoError(t, svc.AddReport(ctx, m.ID, &Report{Kind: ReportKindFinding, Summary: "found it"}),
		"a publish failure must not fail AddReport — the report is the durable fact")

	reports, err := svc.ListReports(ctx, m.ID, 100)
	require.NoError(t, err)
	require.Len(t, reports, 1, "the report is durable regardless of the routing nudge")
	require.Equal(t, "found it", reports[0].Summary)
}

// TestUnit_AddReport_NoPublisherStillStores proves the publisher is optional: a
// service built without one stores reports and simply publishes nothing.
func TestUnit_AddReport_NoPublisherStillStores(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db) // no publisher

	m := newMission("no bus wired")
	require.NoError(t, svc.Create(ctx, m))
	require.NoError(t, svc.AddReport(ctx, m.ID, &Report{Kind: ReportKindResult, Summary: "done"}))

	reports, err := svc.ListReports(ctx, m.ID, 100)
	require.NoError(t, err)
	require.Len(t, reports, 1)
}

// recordingTracker records the (operation, subject, error) of every report so a
// test can assert WHAT a best-effort path said when it shrugged, rather than
// asserting that it merely stayed silent.
type recordingTracker struct {
	mu     sync.Mutex
	events []trackedEvent
}

type trackedEvent struct {
	op, subject string
	err         error
}

func (r *recordingTracker) Start(_ context.Context, op, subject string, _ ...any) (func(error), func(string, any), func()) {
	return func(err error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.events = append(r.events, trackedEvent{op: op, subject: subject, err: err})
		},
		func(string, any) {},
		func() {}
}

func (r *recordingTracker) errorsFor(op, subject string) []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []error
	for _, ev := range r.events {
		if ev.op == op && ev.subject == subject && ev.err != nil {
			out = append(out, ev.err)
		}
	}
	return out
}

var _ libtracker.ActivityTracker = (*recordingTracker)(nil)

// TestUnit_PublishFailuresAreReportedToTracker proves the three best-effort
// publish paths (report, plan revision, terminal status) do not swallow a bus
// failure: each reports it through the tracker — the one instrumentation seam —
// while the durable write it announced still stands.
func TestUnit_PublishFailuresAreReportedToTracker(t *testing.T) {
	ctx, db := setupMissionDB(t)
	busErr := fmt.Errorf("bus is down")
	pub := &fakePublisher{err: busErr}
	tracker := &recordingTracker{}
	svc := New(db, WithEventPublisher(pub), WithTracker(tracker))

	m := newMission("shrug audibly")
	m.ParentSessionID = "parent-session-7"
	require.NoError(t, svc.Create(ctx, m))

	require.NoError(t, svc.AddReport(ctx, m.ID, &Report{Kind: ReportKindResult, Summary: "done"}))
	_, err := svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("", "scope the work", PlanEntryPending, PlanEntryPriorityMedium),
	}, "initial plan")
	require.NoError(t, err)
	_, err = svc.Finish(ctx, m.ID, StatusLanded, "shipped")
	require.NoError(t, err)

	for _, subject := range []string{"report_added_event", "plan_revised_event", "status_changed_event"} {
		reported := tracker.errorsFor("publish", subject)
		require.Len(t, reported, 1, "a failed publish of %s is reported exactly once", subject)
		require.ErrorIs(t, reported[0], busErr, "the report carries the bus error, not a summary of it")
		require.Contains(t, reported[0].Error(), "routing nudge skipped",
			"the report keeps the consequence the operator needs: stored, not routed")
	}
}

// TestUnit_PublishSuccessReportsNothing pins the volume: the tracker hears from
// these paths only when a publish fails, exactly as the log line it replaced
// fired only on failure.
func TestUnit_PublishSuccessReportsNothing(t *testing.T) {
	ctx, db := setupMissionDB(t)
	tracker := &recordingTracker{}
	svc := New(db, WithEventPublisher(&fakePublisher{}), WithTracker(tracker))

	m := newMission("quiet success")
	require.NoError(t, svc.Create(ctx, m))
	require.NoError(t, svc.AddReport(ctx, m.ID, &Report{Kind: ReportKindResult, Summary: "done"}))

	require.Empty(t, tracker.errorsFor("publish", "report_added_event"))
}
