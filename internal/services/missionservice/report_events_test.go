package missionservice

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

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
