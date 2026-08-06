package reportrouter

import (
	"context"
	"sync"
	"testing"

	libbus "github.com/contenox/contenox/internal/libbus"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/stretchr/testify/require"
)

// recordingSupervisor records every offer it is handed.
type recordingSupervisor struct {
	mu     sync.Mutex
	offers []missionservice.AttentionAskedEvent
}

func (s *recordingSupervisor) OfferToSupervisingAgent(_ context.Context, ev missionservice.AttentionAskedEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offers = append(s.offers, ev)
	return nil
}

func (s *recordingSupervisor) list() []missionservice.AttentionAskedEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]missionservice.AttentionAskedEvent(nil), s.offers...)
}

// TestUnit_RouteAsk_OperatorFiredOfferedToSupervisor pins the oracle driver's
// mount point: an operator-fired ask (no parent session) is offered on the
// AgentSupervisor seam — no session delivery, no inbox write, the ask stays
// durable in its own queue either way.
func TestUnit_RouteAsk_OperatorFiredOfferedToSupervisor(t *testing.T) {
	sessions := &fakeDeliverer{}
	inbox := &fakeInbox{}
	sup := &recordingSupervisor{}
	r, err := New(Deps{Bus: libbus.NewInMem(), Sessions: sessions, Inbox: inbox, AgentSupervisor: sup})
	require.NoError(t, err)

	r.routeAsk(context.Background(), missionservice.AttentionAskedEvent{
		MissionID: "m-2", AskID: "ask-1", Summary: "anyone?",
	})

	require.Zero(t, sessions.count(), "no parent session, no delivery attempt")
	require.Empty(t, inbox.items)
	offers := sup.list()
	require.Len(t, offers, 1, "the operator-fired ask is offered on the supervisor seam")
	require.Equal(t, "ask-1", offers[0].AskID)
	require.Empty(t, offers[0].ParentSessionID)
}

// TestUnit_RouteAsk_ParentedOfferUnchanged pins the a2a path byte-identical:
// a parented ask is delivered to the firing session first, then offered;
// a failed delivery (session not live) is never offered.
func TestUnit_RouteAsk_ParentedOfferUnchanged(t *testing.T) {
	t.Run("delivered then offered", func(t *testing.T) {
		sessions := &fakeDeliverer{}
		sup := &recordingSupervisor{}
		r, err := New(Deps{Bus: libbus.NewInMem(), Sessions: sessions, Inbox: &fakeInbox{}, AgentSupervisor: sup})
		require.NoError(t, err)

		r.routeAsk(context.Background(), missionservice.AttentionAskedEvent{
			MissionID: "m-1", AskID: "ask-9", ParentSessionID: "cnx-parent", Summary: "which project?",
		})
		require.Equal(t, 1, sessions.count(), "the human sees the question first")
		require.Len(t, sup.list(), 1, "then the supervising agent is offered it")
	})

	t.Run("undelivered is not offered", func(t *testing.T) {
		sessions := &fakeDeliverer{err: context.DeadlineExceeded}
		sup := &recordingSupervisor{}
		r, err := New(Deps{Bus: libbus.NewInMem(), Sessions: sessions, Inbox: &fakeInbox{}, AgentSupervisor: sup})
		require.NoError(t, err)

		r.routeAsk(context.Background(), missionservice.AttentionAskedEvent{
			MissionID: "m-1", AskID: "ask-9", ParentSessionID: "gone", Summary: "which project?",
		})
		require.Empty(t, sup.list(), "an ask the human never saw is not handed to the agent")
	})
}
