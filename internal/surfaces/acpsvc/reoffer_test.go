package acpsvc

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/approvalflow"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

// The asks here are written straight to the store rather than raised through a
// turn: what is under test is what an attach does with a row that outlived its
// asker, and a live turn cannot produce that state without waiting out the park
// window.

// parkedAskFleet is a fleet whose transports carry a real hitlservice over the
// fleet's own store, plus the resume hook a verdict must reach.
type parkedAskFleet struct {
	*fleet

	mu      sync.Mutex
	resumed []string
}

func newParkedAskFleet(t *testing.T) *parkedAskFleet {
	t.Helper()
	pf := &parkedAskFleet{}
	pf.fleet = newFleet(t, func(deps *Deps, db libdb.DBManager) {
		svc := hitlservice.NewWithDefaultPolicy(nil, runtimetypes.LocalTenantID, runtimetypes.New(db.WithoutTransaction()), nil, "")
		hitlservice.SetResumeHook(svc, func(_ context.Context, approvalID string) error {
			pf.mu.Lock()
			pf.resumed = append(pf.resumed, approvalID)
			pf.mu.Unlock()
			return nil
		})
		deps.Asks = svc
	})
	return pf
}

// resumedRuns reports which approvals ran the resume hook — the evidence that
// a verdict reached the checkpointed run and not just the card.
func (pf *parkedAskFleet) resumedRuns() []string {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	return append([]string(nil), pf.resumed...)
}

// parkAsk writes the state a park leaves behind: a pending ask naming its
// session, and the chain checkpoint the run suspended into.
func (pf *parkedAskFleet) parkAsk(t *testing.T, askID, contenoxSessionID string) *runtimetypes.HITLApproval {
	t.Helper()
	row := &runtimetypes.HITLApproval{
		ID:          askID,
		ToolsName:   "local_shell",
		ToolName:    "exec",
		ArgsSummary: "find home/naro -name go.mod",
		PolicyName:  "hitl-policy-acp.json",
		OnTimeout:   "deny",
		State:       runtimetypes.HITLApprovalPending,
		SessionID:   contenoxSessionID,
		CreatedAt:   time.Now().UTC(),
		ExpiresAt:   time.Now().UTC().Add(time.Hour),
	}
	store := runtimetypes.New(pf.db.WithoutTransaction())
	require.NoError(t, store.CreateHITLApproval(context.Background(), row))
	require.NoError(t, store.CreateChainCheckpoint(context.Background(), &runtimetypes.ChainCheckpoint{
		ID:        askID,
		Payload:   json.RawMessage(`{"parked":true}`),
		SessionID: contenoxSessionID,
	}))
	return row
}

func (pf *parkedAskFleet) askState(t *testing.T, askID string) runtimetypes.HITLApprovalState {
	t.Helper()
	row, err := runtimetypes.New(pf.db.WithoutTransaction()).GetHITLApproval(context.Background(), askID)
	require.NoError(t, err)
	return row.State
}

// loadOnDesk attaches the desk connection to a session the phone opened.
func (pf *parkedAskFleet) loadOnDesk(t *testing.T, sid libacp.SessionID) {
	t.Helper()
	ctx := context.Background()
	_, err := pf.desk.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	_, err = pf.desk.client.LoadSession(ctx, libacp.LoadSessionRequest{
		SessionID:  sid,
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)
}

// awaitCard waits for a client to be shown a card for askID and returns it.
func awaitCard(t *testing.T, peer *fleetPeer, askID string) libacp.RequestPermissionRequest {
	t.Helper()
	var found libacp.RequestPermissionRequest
	require.Eventually(t, func() bool {
		peer.lc.permMu.Lock()
		defer peer.lc.permMu.Unlock()
		for _, req := range peer.lc.permReqs {
			if req.ToolCall.ToolCallID == askID {
				found = req
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond, "no card for %s was raised on this client", askID)
	return found
}

// cardsFor counts how many cards this client was shown for one approval — the
// number that must stay at one no matter how many connections hold the session.
func cardsFor(peer *fleetPeer, askID string) int {
	peer.lc.permMu.Lock()
	defer peer.lc.permMu.Unlock()
	n := 0
	for _, req := range peer.lc.permReqs {
		if req.ToolCall.ToolCallID == askID {
			n++
		}
	}
	return n
}

// TestFleet_AttachingToAParkedSessionIsShownTheApproval is the incident.
func TestFleet_AttachingToAParkedSessionIsShownTheApproval(t *testing.T) {
	pf := newParkedAskFleet(t)
	phone := pf.attach(t, "att-phone")
	phoneSID, internalID := phone.newSession(t)
	pf.parkAsk(t, "ask-parked-1", internalID)

	pf.desk.lc.setPermissionResponse(libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeSelected, OptionID: approvalflow.OptionAllow},
	})
	pf.loadOnDesk(t, phoneSID)

	card := awaitCard(t, pf.desk, "ask-parked-1")
	require.Equal(t, phoneSID, card.SessionID)
	require.Contains(t, card.ToolCall.Title, "find home/naro",
		"the row's args summary is what says which call is being gated")

	var rec askRecovery
	require.NoError(t, json.Unmarshal(card.Meta, &rec))
	require.Equal(t, "ask-parked-1", rec.AskID)
	require.Equal(t, "deny", rec.OnTimeout)
	require.NotEmpty(t, rec.ExpiresAt)
	require.NotEmpty(t, rec.RecoveryCommand)

	meta, ok := approvalflow.ParseMeta(card.Meta)
	require.True(t, ok)
	require.Equal(t, "local_shell", meta.ToolsName)
	require.Equal(t, "hitl-policy-acp.json", meta.PolicyName)
	require.Nil(t, card.ToolCall.RawInput,
		"the durable row keeps no arguments; a re-offered card must not invent the input the policy gated")
}

// TestFleet_AnsweringAReofferedAskResolvesTheDurableRow is the constraint that
// decides whether any of this is worth showing.
func TestFleet_AnsweringAReofferedAskResolvesTheDurableRow(t *testing.T) {
	pf := newParkedAskFleet(t)
	phone := pf.attach(t, "att-phone")
	phoneSID, internalID := phone.newSession(t)
	pf.parkAsk(t, "ask-parked-2", internalID)
	require.Equal(t, runtimetypes.HITLApprovalPending, pf.askState(t, "ask-parked-2"))

	pf.desk.lc.setPermissionResponse(libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeSelected, OptionID: approvalflow.OptionAllow},
	})
	pf.loadOnDesk(t, phoneSID)
	awaitCard(t, pf.desk, "ask-parked-2")

	require.Eventually(t, func() bool {
		return pf.askState(t, "ask-parked-2") == runtimetypes.HITLApprovalApproved
	}, 10*time.Second, 10*time.Millisecond, "the verdict must land on the durable row, not only on the card")
	require.Eventually(t, func() bool {
		for _, id := range pf.resumedRuns() {
			if id == "ask-parked-2" {
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond, "the verdict must reach the resume hook the checkpointed run waits on")
}

// TestFleet_AnsweredAndExpiredAsksAreNotReoffered pins the two rows that are not
// live questions.
func TestFleet_AnsweredAndExpiredAsksAreNotReoffered(t *testing.T) {
	pf := newParkedAskFleet(t)
	phone := pf.attach(t, "att-phone")
	phoneSID, internalID := phone.newSession(t)

	store := runtimetypes.New(pf.db.WithoutTransaction())
	answered := pf.parkAsk(t, "ask-answered", internalID)
	require.NoError(t, store.ResolveHITLApproval(context.Background(), answered.ID, runtimetypes.HITLApprovalApproved, nil, time.Now().UTC()))

	expired := &runtimetypes.HITLApproval{
		ID:          "ask-expired",
		ToolsName:   "local_shell",
		ToolName:    "exec",
		ArgsSummary: "rm -rf /",
		OnTimeout:   "deny",
		State:       runtimetypes.HITLApprovalPending,
		SessionID:   internalID,
		CreatedAt:   time.Now().UTC().Add(-2 * time.Hour),
		ExpiresAt:   time.Now().UTC().Add(-time.Hour),
	}
	require.NoError(t, store.CreateHITLApproval(context.Background(), expired))
	pf.parkAsk(t, "ask-still-open", internalID)

	pf.desk.lc.setPermissionResponse(libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeSelected, OptionID: approvalflow.OptionDeny},
	})
	pf.loadOnDesk(t, phoneSID)

	awaitCard(t, pf.desk, "ask-still-open")
	require.Zero(t, cardsFor(pf.desk, "ask-answered"), "an answered ask is not a live question")
	require.Zero(t, cardsFor(pf.desk, "ask-expired"), "an ask past its expiry is not a live question")
	require.Equal(t, runtimetypes.HITLApprovalPending, pf.askState(t, "ask-expired"),
		"filtering it must not resolve it; the sweeper owns on_timeout")
}

// TestFleet_ReattachingDoesNotRaiseASecondCardForOneApproval pins where the
// idempotency lives.
func TestFleet_ReattachingDoesNotRaiseASecondCardForOneApproval(t *testing.T) {
	pf := newParkedAskFleet(t)
	phone := pf.attach(t, "att-phone")
	phoneSID, internalID := phone.newSession(t)
	pf.parkAsk(t, "ask-parked-3", internalID)

	releaseDesk := pf.desk.lc.holdPermission()
	defer releaseDesk()
	releasePhone := phone.lc.holdPermission()
	defer releasePhone()

	pf.loadOnDesk(t, phoneSID)
	awaitCard(t, pf.desk, "ask-parked-3")

	ctx := context.Background()
	_, err := pf.desk.client.LoadSession(ctx, libacp.LoadSessionRequest{
		SessionID:  phoneSID,
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)

	_, err = phone.client.LoadSession(ctx, libacp.LoadSessionRequest{
		SessionID:  phoneSID,
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)
	awaitCard(t, phone, "ask-parked-3")

	require.Len(t, pf.router.transportsFor(internalID), 2, "both connections hold the session")
	require.Never(t, func() bool {
		return cardsFor(pf.desk, "ask-parked-3") > 1 || cardsFor(phone, "ask-parked-3") > 1
	}, 2*time.Second, 50*time.Millisecond, "one connection must never stack two cards for one approval")
	require.Equal(t, runtimetypes.HITLApprovalPending, pf.askState(t, "ask-parked-3"),
		"nobody answered; the ask stays open")
}

// TestUnit_ReofferableAsk_RejectsWhatIsNotALiveQuestion pins the filter itself,
// including the case the fleet tests cannot stage cheaply: an attention ask.
// It is answered with the operator's words through /answer, and a permission
// card offering allow/deny cannot answer it — offering one would record a
// verdict the waiting unit cannot read.
