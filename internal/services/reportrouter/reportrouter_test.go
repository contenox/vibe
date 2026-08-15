package reportrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/operatorinbox"
	"github.com/contenox/contenox/libacp"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

type fakeDeliverer struct {
	mu       sync.Mutex
	sessions []libacp.SessionID
	notes    []libacp.SessionNotification
	err      error
}

func (f *fakeDeliverer) DeliverToSession(_ context.Context, sid libacp.SessionID, n libacp.SessionNotification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = append(f.sessions, sid)
	f.notes = append(f.notes, n)
	return f.err
}

func (f *fakeDeliverer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

type fakeInbox struct {
	mu    sync.Mutex
	items []*operatorinbox.Item
	err   error
}

func (f *fakeInbox) Add(_ context.Context, item *operatorinbox.Item) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.items = append(f.items, item)
	return nil
}

func (f *fakeInbox) list() []*operatorinbox.Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*operatorinbox.Item(nil), f.items...)
}

func newTestRouter(t *testing.T, del *fakeDeliverer, inbox *fakeInbox) *Router {
	t.Helper()
	r, err := New(Deps{Bus: libbus.NewInMem(), Sessions: del, Inbox: inbox})
	require.NoError(t, err)
	return r
}

func resultEvent(parentSessionID string) missionservice.ReportAddedEvent {
	return missionservice.ReportAddedEvent{
		MissionID:       "m1",
		ParentSessionID: parentSessionID,
		AgentName:       "runner",
		Intent:          "do the thing",
		Report: missionservice.Report{
			ID: "r1", MissionID: "m1", Kind: missionservice.ReportKindResult,
			Summary: "shipped the board", Detail: "all green", Refs: []string{"a.txt", "b.txt"},
		},
	}
}

// TestUnit_Route_ParentSessionDelivered pins that a live parent session gets the report and nothing lands in the inbox.
func TestUnit_Route_ParentSessionDelivered(t *testing.T) {
	del := &fakeDeliverer{}
	inbox := &fakeInbox{}
	r := newTestRouter(t, del, inbox)

	r.route(context.Background(), resultEvent("parent-42"))

	require.Equal(t, 1, del.count(), "the report is delivered to the parent session")
	require.Equal(t, libacp.SessionID("parent-42"), del.sessions[0])
	require.Empty(t, inbox.list(), "a delivered report does not also land in the inbox")

	n := del.notes[0]
	require.Equal(t, libacp.SessionUpdateAgentMessageChunk, n.Update.SessionUpdate)
	require.NotNil(t, n.Update.Content)
	require.Contains(t, n.Update.Content.Text, "unit runner reported (result): shipped the board")
	require.Contains(t, n.Update.Content.Text, "all green")
	require.Contains(t, n.Update.Content.Text, "refs: a.txt, b.txt")

	var meta reportUpdateMeta
	require.NoError(t, json.Unmarshal(n.Update.Meta, &meta))
	require.NotNil(t, meta.Report)
	require.Equal(t, "m1", meta.Report.MissionID)
	require.Equal(t, "r1", meta.Report.ReportID)
	require.Equal(t, "result", meta.Report.Kind)
}

// TestUnit_Route_ParentGoneFallsBackToInbox pins that an unreachable parent falls back to the inbox marked parent_gone, never dropped.
func TestUnit_Route_ParentGoneFallsBackToInbox(t *testing.T) {
	del := &fakeDeliverer{err: fmt.Errorf("agentinstance: session %q: not found", "parent-42")}
	inbox := &fakeInbox{}
	r := newTestRouter(t, del, inbox)

	r.route(context.Background(), resultEvent("parent-42"))

	require.Equal(t, 1, del.count(), "delivery was attempted")
	items := inbox.list()
	require.Len(t, items, 1, "an undeliverable report falls back to the inbox, never dropped")
	require.Equal(t, operatorinbox.ReasonParentGone, items[0].Reason)
	require.Equal(t, "parent-42", items[0].ParentSessionID, "the intended-but-missed supervisor is recorded")
	require.Equal(t, "shipped the board", items[0].Report.Summary)
}

// TestUnit_Route_OperatorFiredToInbox pins that no parent session means straight to the inbox, never a delivery attempt.
func TestUnit_Route_OperatorFiredToInbox(t *testing.T) {
	del := &fakeDeliverer{}
	inbox := &fakeInbox{}
	r := newTestRouter(t, del, inbox)

	r.route(context.Background(), resultEvent(""))

	require.Equal(t, 0, del.count(), "no supervisor session → no delivery attempt")
	items := inbox.list()
	require.Len(t, items, 1)
	require.Equal(t, operatorinbox.ReasonOperatorFired, items[0].Reason)
	require.Empty(t, items[0].ParentSessionID)
	require.Equal(t, "m1", items[0].MissionID)
}

// TestUnit_Route_InboxWriteFailureNeverPanics pins that an inbox write failure is tolerated, not crashed.
func TestUnit_Route_InboxWriteFailureNeverPanics(t *testing.T) {
	del := &fakeDeliverer{}
	inbox := &fakeInbox{err: fmt.Errorf("store down")}
	r := newTestRouter(t, del, inbox)

	require.NotPanics(t, func() { r.route(context.Background(), resultEvent("")) })
}

// TestUnit_New_RequiresCollaborators proves the wiring guards.
func TestUnit_New_RequiresCollaborators(t *testing.T) {
	_, err := New(Deps{Sessions: &fakeDeliverer{}, Inbox: &fakeInbox{}})
	require.Error(t, err, "Bus is required")
	_, err = New(Deps{Bus: libbus.NewInMem(), Inbox: &fakeInbox{}})
	require.Error(t, err, "Sessions is required")
	_, err = New(Deps{Bus: libbus.NewInMem(), Sessions: &fakeDeliverer{}})
	require.Error(t, err, "Inbox is required")
}

// TestUnit_StartConsumesBusEvents pins that an event published after Start is decoded and routed.
func TestUnit_StartConsumesBusEvents(t *testing.T) {
	del := &fakeDeliverer{}
	inbox := &fakeInbox{}
	bus := libbus.NewInMem()
	r, err := New(Deps{Bus: bus, Sessions: del, Inbox: inbox})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, err := r.Start(ctx)
	require.NoError(t, err)
	defer stop()

	data, err := json.Marshal(resultEvent("parent-42"))
	require.NoError(t, err)
	require.NoError(t, bus.Publish(ctx, missionservice.ReportAddedSubject, data))

	require.Eventually(t, func() bool { return del.count() == 1 }, 2*time.Second, 10*time.Millisecond,
		"the router consumes the published event and delivers it")
	require.Equal(t, libacp.SessionID("parent-42"), del.sessions[0])
}

// TestUnit_RouteAsk_DeliversTheQuestionToTheFiringSession pins that an ask reaches the firing session carrying its answer handle.
func statusEvent(parentSessionID string, newStatus missionservice.Status, reason string) missionservice.StatusChangedEvent {
	return missionservice.StatusChangedEvent{
		MissionID:       "m1",
		ParentSessionID: parentSessionID,
		AgentName:       "runner",
		Intent:          "do the thing",
		OldStatus:       missionservice.StatusOpen,
		NewStatus:       newStatus,
		Reason:          reason,
	}
}

func planEvent(parentSessionID string) missionservice.PlanRevisedEvent {
	return missionservice.PlanRevisedEvent{
		MissionID:       "m1",
		ParentSessionID: parentSessionID,
		AgentName:       "runner",
		Intent:          "do the thing",
		Revision:        3,
		Explanation:     "split the migration in two",
		EntryCount:      4,
		Added:           2,
		Removed:         1,
		Pending:         2,
		InProgress:      1,
		Completed:       1,
	}
}

// TestUnit_RouteStatus_DeliversToFiringSession pins the _meta envelope keys as contract: other surfaces decode them by name.
func TestUnit_RouteStatus_DeliversToFiringSession(t *testing.T) {
	del := &fakeDeliverer{}
	inbox := &fakeInbox{}
	r := newTestRouter(t, del, inbox)

	r.routeStatus(context.Background(), statusEvent("parent-42", missionservice.StatusLanded, ""))

	require.Equal(t, 1, del.count(), "the firing session must be told its unit finished")
	require.Empty(t, inbox.list(), "a status change is never filed: the mission record already holds it")

	n := del.notes[0]
	require.Equal(t, libacp.SessionID("parent-42"), n.SessionID)
	require.Equal(t, libacp.SessionUpdateAgentMessageChunk, n.Update.SessionUpdate)
	require.Equal(t, "unit runner landed", n.Update.Content.Text)
	require.Equal(t, "mission-status-m1-open-landed", n.Update.MessageID)

	var meta statusUpdateMeta
	require.NoError(t, json.Unmarshal(n.Update.Meta, &meta))
	require.NotNil(t, meta.Status)
	require.Equal(t, "m1", meta.Status.MissionID)
	require.Equal(t, "runner", meta.Status.AgentName)
	require.Equal(t, "do the thing", meta.Status.Intent)
	require.Equal(t, "open", meta.Status.OldStatus)
	require.Equal(t, "landed", meta.Status.NewStatus)
	require.Contains(t, string(n.Update.Meta), "contenox.missionStatus")
}

// TestUnit_RouteStatus_DerailedCarriesItsReason pins that a failure reason rides in both the body and the envelope.
func TestUnit_RouteStatus_DerailedCarriesItsReason(t *testing.T) {
	del := &fakeDeliverer{}
	r := newTestRouter(t, del, &fakeInbox{})

	r.routeStatus(context.Background(), statusEvent("parent-42", missionservice.StatusDerailed, "build never went green"))

	n := del.notes[0]
	require.Equal(t, "unit runner derailed: build never went green", n.Update.Content.Text)
	var meta statusUpdateMeta
	require.NoError(t, json.Unmarshal(n.Update.Meta, &meta))
	require.Equal(t, "derailed", meta.Status.NewStatus)
	require.Equal(t, "build never went green", meta.Status.Reason)
}

// TestUnit_RouteStatus_NoParentIsDroppedNotFiled pins that an operator-fired status change is delivered and inboxed nowhere.
func TestUnit_RouteStatus_NoParentIsDroppedNotFiled(t *testing.T) {
	del := &fakeDeliverer{}
	inbox := &fakeInbox{}
	r := newTestRouter(t, del, inbox)

	r.routeStatus(context.Background(), statusEvent("", missionservice.StatusLanded, ""))

	require.Zero(t, del.count(), "no supervisor session → no delivery attempt")
	require.Empty(t, inbox.list(),
		"every mission that ever finishes would otherwise drown the worklist the inbox exists for")
}

// TestUnit_RouteStatus_ParentGoneIsDroppedNotFiled pins that an unreachable parent drops the status change rather than filing it.
func TestUnit_RouteStatus_ParentGoneIsDroppedNotFiled(t *testing.T) {
	del := &fakeDeliverer{err: libdb.ErrNotFound}
	inbox := &fakeInbox{}
	r := newTestRouter(t, del, inbox)

	r.routeStatus(context.Background(), statusEvent("parent-42", missionservice.StatusStuck, "waiting on a human"))

	require.Equal(t, 1, del.count(), "delivery was attempted")
	require.Empty(t, inbox.list(), "an undeliverable status change is dropped, never filed")
}

// TestUnit_RoutePlan_DeliversToFiringSession pins that a re-plan reaches the firing session with its counts, contract-literal like the status lane.
func TestUnit_RoutePlan_DeliversToFiringSession(t *testing.T) {
	del := &fakeDeliverer{}
	inbox := &fakeInbox{}
	r := newTestRouter(t, del, inbox)

	r.routePlan(context.Background(), planEvent("parent-42"))

	require.Equal(t, 1, del.count())
	require.Empty(t, inbox.list(), "a plan revision is never filed: the mission record already holds it")

	n := del.notes[0]
	require.Equal(t, libacp.SessionID("parent-42"), n.SessionID)
	require.Equal(t, libacp.SessionUpdateAgentMessageChunk, n.Update.SessionUpdate)
	require.Equal(t, "unit runner revised its plan (rev 3): split the migration in two\n"+
		"4 entries: 2 pending, 1 in progress, 1 completed", n.Update.Content.Text)
	require.Equal(t, "mission-plan-m1-3", n.Update.MessageID)

	var meta planUpdateMeta
	require.NoError(t, json.Unmarshal(n.Update.Meta, &meta))
	require.NotNil(t, meta.Plan)
	require.Equal(t, "m1", meta.Plan.MissionID)
	require.Equal(t, "runner", meta.Plan.AgentName)
	require.Equal(t, 3, meta.Plan.Revision)
	require.Equal(t, "split the migration in two", meta.Plan.Explanation)
	require.Equal(t, 4, meta.Plan.EntryCount)
	require.Equal(t, 2, meta.Plan.Pending)
	require.Equal(t, 1, meta.Plan.InProgress)
	require.Equal(t, 1, meta.Plan.Completed)
	require.Contains(t, string(n.Update.Meta), "contenox.missionPlan")
}

// TestUnit_RoutePlan_DroppedWithoutInboxWrites pins both drop cases for the plan lane, mirroring the status lane's rule.
func TestUnit_RoutePlan_DroppedWithoutInboxWrites(t *testing.T) {
	for _, tc := range []struct {
		name            string
		parentSessionID string
		deliverErr      error
		wantAttempts    int
	}{
		{"operator fired", "", nil, 0},
		{"parent gone", "parent-42", libdb.ErrNotFound, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			del := &fakeDeliverer{err: tc.deliverErr}
			inbox := &fakeInbox{}
			r := newTestRouter(t, del, inbox)

			r.routePlan(context.Background(), planEvent(tc.parentSessionID))

			require.Equal(t, tc.wantAttempts, del.count())
			require.Empty(t, inbox.list())
		})
	}
}

// TestUnit_DeliveredStatusAndPlanCarryStableMessageIDs pins that message ids are derived from the event, stable under redelivery.
func TestUnit_DeliveredStatusAndPlanCarryStableMessageIDs(t *testing.T) {
	landed := buildStatusNotification(statusEvent("cnx", missionservice.StatusLanded, ""))
	derailed := buildStatusNotification(statusEvent("cnx", missionservice.StatusDerailed, "nope"))
	require.NotEmpty(t, landed.Update.MessageID)
	require.NotEqual(t, landed.Update.MessageID, derailed.Update.MessageID,
		"two different transitions are two different messages")
	require.Equal(t, landed.Update.MessageID,
		buildStatusNotification(statusEvent("cnx", missionservice.StatusLanded, "")).Update.MessageID,
		"the same transition redelivered must reuse its message id, not duplicate the bubble")

	rev3 := buildPlanNotification(planEvent("cnx"))
	rev4 := planEvent("cnx")
	rev4.Revision = 4
	require.NotEqual(t, rev3.Update.MessageID, buildPlanNotification(rev4).Update.MessageID,
		"two revisions are two messages")
	require.Equal(t, rev3.Update.MessageID, buildPlanNotification(planEvent("cnx")).Update.MessageID,
		"one revision redelivered is one message")
}

// TestUnit_StatusAndPlanTextNameAnUnnamedUnit pins that an empty AgentName still reads as legible text.
func TestUnit_StatusAndPlanTextNameAnUnnamedUnit(t *testing.T) {
	ev := statusEvent("cnx", missionservice.StatusLanded, "")
	ev.AgentName = ""
	require.Equal(t, "unit a mission unit landed", statusText(ev))

	pev := planEvent("cnx")
	pev.AgentName = ""
	pev.Explanation = ""
	require.Contains(t, planText(pev), "unit a mission unit revised its plan (rev 3)\n")
}

// TestUnit_StartConsumesStatusAndPlanEvents pins that Start subscribes both the status and plan lanes.
func TestUnit_StartConsumesStatusAndPlanEvents(t *testing.T) {
	del := &fakeDeliverer{}
	bus := libbus.NewInMem()
	r, err := New(Deps{Bus: bus, Sessions: del, Inbox: &fakeInbox{}})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, err := r.Start(ctx)
	require.NoError(t, err)
	defer stop()

	statusData, err := json.Marshal(statusEvent("parent-42", missionservice.StatusLanded, ""))
	require.NoError(t, err)
	require.NoError(t, bus.Publish(ctx, missionservice.StatusChangedSubject, statusData))

	planData, err := json.Marshal(planEvent("parent-42"))
	require.NoError(t, err)
	require.NoError(t, bus.Publish(ctx, missionservice.PlanRevisedSubject, planData))

	require.Eventually(t, func() bool { return del.count() == 2 }, 2*time.Second, 10*time.Millisecond,
		"Start subscribes the status and plan lanes too")

	ids := map[string]bool{}
	del.mu.Lock()
	for _, n := range del.notes {
		ids[n.Update.MessageID] = true
	}
	del.mu.Unlock()
	require.True(t, ids["mission-status-m1-open-landed"])
	require.True(t, ids["mission-plan-m1-3"])
}

// TestUnit_DeliveredReportsCarryTheirOwnMessageID pins that distinct reports get distinct message ids.
func TestUnit_DeliveredReportsCarryTheirOwnMessageID(t *testing.T) {
	first := buildReportNotification(missionservice.ReportAddedEvent{
		MissionID: "m-1", ParentSessionID: "cnx",
		Report: missionservice.Report{ID: "r-1", Kind: missionservice.ReportKindProgress, Summary: "read the README"},
	})
	second := buildReportNotification(missionservice.ReportAddedEvent{
		MissionID: "m-1", ParentSessionID: "cnx",
		Report: missionservice.Report{ID: "r-2", Kind: missionservice.ReportKindResult, Summary: "done"},
	})

	require.NotEmpty(t, first.Update.MessageID)
	require.NotEqual(t, first.Update.MessageID, second.Update.MessageID)
}
