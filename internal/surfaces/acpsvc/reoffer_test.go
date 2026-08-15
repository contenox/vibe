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

// This file drives the failure the re-offer closes: an approval was raised,
// nobody answered inside localtools.ApprovalParkWindow, the run parked with a
// durable checkpoint — and every client showed a session that had simply
// stopped, with no card and no way to answer.
//
// The asks here are written straight to the store rather than raised through a
// turn, because what is under test is what an ATTACH does with a row that
// outlived its asker. A live turn cannot produce that state without waiting
// out the park window.

// parkedAskFleet is a fleet whose transports carry a real hitlservice over the
// fleet's own store, plus the resume hook a verdict must reach. Keeping the
// hook's log is what lets a test assert the durable outcome rather than the
// RPC.
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
// session, and the chain checkpoint the run suspended into. Both halves
// matter — the checkpoint is what makes hitlservice refuse a verdict from a
// process that cannot resume it.
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

// cardsFor counts how many cards this client was shown for one approval —
// the number that must stay at one no matter how many connections hold the
// session.
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

// TestFleet_AttachingToAParkedSessionIsShownTheApproval is the incident. A run
// parked on an approval nobody answered; the ask survives as a pending row and
// the run is resumable, but the asking goroutine is gone, so no live card
// exists for a client to be handed. Attaching must raise it again.
//
// The card is asserted to carry the ask's own id — the answer has to land on
// the same durable row — and the recovery envelope, so the operator sees the
// deadline and the terminal command even here.
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
// decides whether any of this is worth showing. The goroutine that raised the
// original ask was abandoned when the park window elapsed, so a verdict that
// stops at this surface unblocks nothing — it would tell the operator they
// released a run that is still parked.
//
// The assertion is therefore on the row and on the resume hook, not on the
// RPC: the ask must end approved, and the hook that restarts the checkpointed
// run must have been called with its id.
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

// TestFleet_AnsweredAndExpiredAsksAreNotReoffered pins the two rows that are
// not live questions. An answered ask is decided; an ask past expires_at is a
// decided question waiting for the sweeper to record on_timeout, and answering
// it would be refused. Re-offering either puts a card in front of an operator
// that can change nothing.
//
// A third row is parked alongside them so the assertion is "these were
// filtered", not "nothing happened".
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
// idempotency lives. A session is held by every attached connection, so the
// same approval showing on a phone and a desk is the intended state — each of
// them is a screen someone may be looking at. What must never happen is ONE
// connection being asked twice about one approval, which is exactly what a
// client reloading a session it already holds would otherwise produce.
//
// Both halves are asserted. The desk loads the session twice — the card it is
// already showing must not be stacked behind itself — and the phone then loads
// it once, being shown the ask for the first time, because a second connection
// is a second screen and not a duplicate.
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
