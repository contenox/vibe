package reportrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	libbus "github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/operatorinbox"
	"github.com/contenox/beam/libacp"
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

// TestUnit_Route_ParentSessionDelivered: with a live parent session, the report
// is delivered into it and nothing lands in the inbox.
func TestUnit_Route_ParentSessionDelivered(t *testing.T) {
	del := &fakeDeliverer{}
	inbox := &fakeInbox{}
	r := newTestRouter(t, del, inbox)

	r.route(context.Background(), resultEvent("parent-42"))

	require.Equal(t, 1, del.count(), "the report is delivered to the parent session")
	require.Equal(t, libacp.SessionID("parent-42"), del.sessions[0])
	require.Empty(t, inbox.list(), "a delivered report does not also land in the inbox")

	// The delivered update is a transcript-legible agent_message_chunk carrying
	// the mission-report attribution in its _meta envelope.
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

// TestUnit_Route_ParentGoneFallsBackToInbox: a named parent that cannot be
// reached (deliverer errors) falls back to the inbox marked parent_gone. A
// supervisor ending must never drop a report.
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

// TestUnit_Route_OperatorFiredToInbox: no parent session → straight to the
// operator inbox, never a delivery attempt.
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

// TestUnit_Route_InboxWriteFailureNeverPanics: an inbox write failure is
// tolerated (tracked, not crashed) — routing is best-effort.
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

// TestUnit_StartConsumesBusEvents drives the full loop: an event published on
// the bus after Start is decoded and routed. Uses the in-memory bus, so it is
// fast and needs no subprocess.
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

// TestUnit_RouteAsk_DeliversTheQuestionToTheFiringSession is the ask half of the
// supervision edge: a unit's QUESTION must reach the session that fired the
// mission, carrying the handle an answer is given against, so the operator can
// answer where they already are instead of hunting through a separate queue.
func TestUnit_RouteAsk_DeliversTheQuestionToTheFiringSession(t *testing.T) {
	sessions := &fakeDeliverer{}
	inbox := &fakeInbox{}
	r, err := New(Deps{Bus: libbus.NewInMem(), Sessions: sessions, Inbox: inbox})
	require.NoError(t, err)

	r.routeAsk(context.Background(), missionservice.AttentionAskedEvent{
		MissionID:       "m-1",
		AskID:           "ask-9",
		ParentSessionID: "cnx-parent",
		AgentName:       "chain-acp",
		Summary:         "which project did you mean?",
	})

	require.Equal(t, 1, sessions.count(), "the firing session must be told")
	got := sessions.notes[0]
	require.Equal(t, libacp.SessionID("cnx-parent"), got.SessionID)
	require.Contains(t, got.Update.Content.Text, "which project did you mean?")
	require.Contains(t, got.Update.Content.Text, "waiting on you")
	require.Contains(t, string(got.Update.Meta), "contenox.missionAsk")
	require.Contains(t, string(got.Update.Meta), "ask-9", "the answer handle must ride along")

	require.Empty(t, inbox.items,
		"an ask is already durable in its own queue — the report inbox must not be double-written")
}

// TestUnit_RouteAsk_OperatorFiredStaysInTheQueue pins the no-parent case: nothing
// is delivered and nothing is inboxed, because the ask queue IS where an operator
// who fired directly answers.
func TestUnit_RouteAsk_OperatorFiredStaysInTheQueue(t *testing.T) {
	sessions := &fakeDeliverer{}
	inbox := &fakeInbox{}
	r, err := New(Deps{Bus: libbus.NewInMem(), Sessions: sessions, Inbox: inbox})
	require.NoError(t, err)

	r.routeAsk(context.Background(), missionservice.AttentionAskedEvent{
		MissionID: "m-2", AskID: "ask-1", Summary: "anyone?",
	})

	require.Zero(t, sessions.count())
	require.Empty(t, inbox.items)
}

// TestUnit_RouteAsk_ParentNotLiveIsNotAFault covers the reconnect gap: a firing
// session that is not currently held by a connection cannot be told, and that is
// a missed notification — the question stays answerable in the queue.
func TestUnit_RouteAsk_ParentNotLiveIsNotAFault(t *testing.T) {
	sessions := &fakeDeliverer{err: libdb.ErrNotFound}
	inbox := &fakeInbox{}
	r, err := New(Deps{Bus: libbus.NewInMem(), Sessions: sessions, Inbox: inbox})
	require.NoError(t, err)

	r.routeAsk(context.Background(), missionservice.AttentionAskedEvent{
		MissionID: "m-3", AskID: "ask-2", ParentSessionID: "gone", Summary: "still there?",
	})

	require.Empty(t, inbox.items, "a missed ask notification is not an inbox report")
}

// TestUnit_DeliveredAsksCarryTheirOwnMessageID is the regression for two
// questions rendering as one: streamed chunks group by message id, so a delivery
// that carries none is folded into whatever message the session is accumulating.
// Two questions then shared one transcript bubble and one answer box — and once
// the first was answered, the second had no field at all.
func TestUnit_DeliveredAsksCarryTheirOwnMessageID(t *testing.T) {
	first := buildAskNotification(missionservice.AttentionAskedEvent{
		MissionID: "m-1", AskID: "ask-1", ParentSessionID: "cnx", Summary: "which project?",
	})
	second := buildAskNotification(missionservice.AttentionAskedEvent{
		MissionID: "m-1", AskID: "ask-2", ParentSessionID: "cnx", Summary: "which line?",
	})

	require.NotEmpty(t, first.Update.MessageID)
	require.NotEqual(t, first.Update.MessageID, second.Update.MessageID,
		"two questions must not land in one message — the second would be unanswerable behind the first")
}

// TestUnit_DeliveredReportsCarryTheirOwnMessageID holds the same line for
// reports: two reports are two things a unit said, not one run-on message.
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
