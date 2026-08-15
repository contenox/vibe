package acpsvc

import (
	"context"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/services/approvalflow"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
)

// parkedAskReofferLimit bounds how many parked approvals one attach
// re-presents. A session with more open asks than this has a problem no card
// stack solves; the rest stay answerable from a terminal.
const parkedAskReofferLimit = 20

// parkedAskListTimeout bounds the durable lookup an attach triggers. The
// lookup runs off the session/load response entirely, so this is not a
// deadline the client waits on — it exists so a stalled database cannot leave
// a goroutine pinned for the life of the connection.
const parkedAskListTimeout = 5 * time.Second

// reofferParkedAsks re-presents every approval this session is still parked
// on to the connection that just attached.
//
// # Why an attach has to do this at all
//
// An ask outlives the goroutine that raised it. When
// localtools.ApprovalParkWindow elapses with no verdict, the run checkpoints
// and releases its process, and the ask() call that was driving the card is
// abandoned — which reaches the client as a cancelled request, so the card
// disappears. The durable row stays pending and the run stays resumable, but
// nothing re-presents the question. A client attaching a second later, or
// reconnecting an hour later, sees a session that has simply stopped.
//
// # Where it is wired, and where it is not
//
// LoadSession and ResumeSession, beside reattachNativeTurn: those are the
// three things a returning client needs re-established — the transcript, the
// turn in flight, and the question the turn is parked on. NewSession is not
// wired: it mints a fresh contenox session id, and no ask can already name a
// session that did not exist when the ask was recorded. Wiring it would buy a
// guaranteed-empty query on the hottest path.
//
// # It cannot make an attach slow or fallible
//
// Everything after the response is scheduled: the lookup runs on its own
// goroutine, bound to the connection rather than to the request, and each
// card blocks on its own. A lookup that fails or returns nothing is dropped —
// session/load succeeds either way, because someone reconnecting on a phone
// must not be held behind a database query, and refusing the load would cost
// them the transcript as well as the card.
func (t *Transport) reofferParkedAsks(ctx context.Context, sid libacp.SessionID, contenoxSessionID string) {
	if t.deps.Asks == nil || t.conn == nil || contenoxSessionID == "" {
		return
	}
	libacp.AfterResponse(ctx, func() {
		go t.offerParkedAsks(sid, contenoxSessionID)
	})
}

// offerParkedAsks reads contenoxSessionID's open asks and raises one card per
// live row on this connection. Runs detached from the attach request.
func (t *Transport) offerParkedAsks(sid libacp.SessionID, contenoxSessionID string) {
	base := t.connContext()
	reportErr, reportChange, end := t.tracker().Start(base, "reoffer", "acp_parked_asks", "session_id", string(sid))
	defer end()

	listCtx, cancel := context.WithTimeout(base, parkedAskListTimeout)
	rows, err := t.deps.Asks.ListPendingForSession(listCtx, contenoxSessionID, parkedAskReofferLimit)
	cancel()
	if err != nil {
		reportErr(err)
		return
	}

	now := time.Now().UTC()
	offered := 0
	for _, row := range rows {
		if !reofferableAsk(row, now) {
			continue
		}
		if !t.claimPermissionCard(sid, row.ID) {
			continue
		}
		offered++
		go t.offerParkedAsk(base, sid, row)
	}
	reportChange(string(sid), map[string]any{"pending": len(rows), "offered": offered})
}

// offerParkedAsk raises one re-offered permission card and records whatever
// answer comes back through the durable responder.
//
// The verdict goes to AskInbox.Respond, never to a channel this surface owns:
// the original waiter is gone, so Respond's resume hook is what actually
// restarts the checkpointed run. An answer that resolved only the card would
// be worse than no card at all.
//
// A verdict refused because the ask was already resolved elsewhere — the
// operator answered on another screen, or the sweeper applied OnTimeout — is
// reported and dropped. It is not an error the client can act on, and the row
// is terminal exactly once by construction (see hitlservice.resolve's CAS).
func (t *Transport) offerParkedAsk(ctx context.Context, sid libacp.SessionID, row *runtimetypes.HITLApproval) {
	defer t.clearPermissionPending(sid, row.ID)

	reportErr, reportChange, end := t.tracker().Start(ctx, "reoffer", "acp_permission", "tool_call_id", row.ID)
	defer end()

	rpcReq := t.parkedAskCard(sid, row)
	t.attachAskRecovery(ctx, &rpcReq, row.ID)

	resp, err := t.conn.RequestPermission(ctx, rpcReq)
	if err != nil {
		reportErr(err)
		return
	}
	reportChange("outcome", string(resp.Outcome.Outcome))
	if resp.Outcome.Outcome != libacp.PermissionOutcomeSelected {
		return
	}
	approved := resp.Outcome.OptionID == approvalflow.OptionAllow
	reportChange("approved", approved)
	if err := t.deps.Asks.Respond(ctx, row.ID, approved); err != nil {
		reportErr(err)
	}
}

// reofferableAsk reports whether row is still a live question worth putting
// back in front of an operator.
//
// State is already filtered in SQL; it is re-checked here because a row read
// before an answer landed is a row that must not be re-asked. Expiry is the
// check SQL cannot make: expires_at is the moment SweepExpired applies
// on_timeout and any later answer is refused, so a row past it is a decided
// question waiting to be recorded, not an open one. Every pending ask that
// reaches here is a permission ask, which is exactly what an allow/deny card
// answers.
func reofferableAsk(row *runtimetypes.HITLApproval, now time.Time) bool {
	switch {
	case row == nil, row.ID == "":
		return false
	case row.State != runtimetypes.HITLApprovalPending:
		return false
	case !row.ExpiresAt.IsZero() && !row.ExpiresAt.After(now):
		return false
	}
	return true
}

// parkedAskCard renders a durable row as the permission request that asks it
// again, under the ask's own id so the answer lands on the same row.
//
// The card carries no rawInput. The row keeps only args_summary, never the
// call's arguments, and a reconstructed argument object would be presented to
// the operator as the input the policy actually gated. The summary is stated
// as text instead: it is what the row is willing to vouch for.
func (t *Transport) parkedAskCard(sid libacp.SessionID, row *runtimetypes.HITLApproval) libacp.RequestPermissionRequest {
	req := hitlservice.ApprovalRequest{
		ToolCallID:  row.ID,
		ToolsName:   row.ToolsName,
		ToolName:    row.ToolName,
		PolicyName:  row.PolicyName,
		MatchedRule: row.MatchedRule,
		OnTimeout:   hitlservice.Action(row.OnTimeout),
		SessionID:   row.SessionID,
	}
	if row.Diff != nil {
		req.Diff = *row.Diff
	}
	card := approvalflow.BuildRequest(req, approvalflow.BuildOptions{
		SessionID:   sid,
		PolicyName:  row.PolicyName,
		PolicyPath:  t.hitlPolicyPath(row.PolicyName),
		MatchedRule: row.MatchedRule,
	})
	summary := strings.TrimSpace(row.ArgsSummary)
	if summary == "" {
		return card
	}
	if !strings.Contains(card.ToolCall.Title, summary) {
		card.ToolCall.Title += ": " + summary
	}
	if len(card.ToolCall.Content) == 0 {
		block := libacp.NewTextContent(summary)
		card.ToolCall.Content = []libacp.ToolCallContent{{Type: libacp.ToolCallContentRegular, Content: &block}}
	}
	return card
}

// connContext is the connection's own lifetime as a context: what work that
// outlives one request must be bound to, so a client drop tears down its
// outstanding cards. Falls back to Background for a Transport built without a
// connection (setup-only, and unit tests).
func (t *Transport) connContext() context.Context {
	if t.connCtx != nil {
		return t.connCtx
	}
	return context.Background()
}

// AskInbox, the seam this file re-offers parked approvals through, is declared with the rest of the ask surface in commands_answer.go.
