package acpsvc

import (
	"context"
	"encoding/json"

	libacp "github.com/contenox/contenox/libacp"
)

const stopReasonMetaKey = "contenox.stopReason"

// stopReasonSuspended names a turn that parked on a human approval. It is
// deliberately not a libacp.StopReason, whose set is closed: the spec field stays
// end_turn and the truth travels in `_meta`.
const stopReasonSuspended = "suspended"

const stopReasonFailed = "failed"

// stopReasonExplained is the operator-facing form of a turn's stop reason: what
// happened, and the command that resolves it.
type stopReasonExplained struct {
	Reason      string `json:"reason"`
	Explanation string `json:"explanation"`
	Command     string `json:"command,omitempty"`
	// ApprovalID names the durable approval a suspended turn is waiting on; set
	// only by explainSuspension.
	ApprovalID string `json:"approvalId,omitempty"`
}

func explainStopReason(r libacp.StopReason) (stopReasonExplained, bool) {
	switch r {
	case libacp.StopReasonMaxTokens:
		return stopReasonExplained{
			Reason:      string(r),
			Explanation: "The reply stopped at the response token cap, so it is cut off rather than finished. Raise the cap, or reclaim context if the conversation has grown long.",
			Command:     "/max-tokens <count>  (or /compact to reclaim context)",
		}, true
	case libacp.StopReasonMaxTurnRequests:
		return stopReasonExplained{
			Reason:      string(r),
			Explanation: "The turn used up its budget of model requests before reaching an answer — usually a tool loop that could not converge. Narrowing the next request, or clearing the history it is reasoning over, gets it moving again.",
			Command:     "/clear  (or /compact to keep the recent turns)",
		}, true
	case libacp.StopReasonRefusal:
		return stopReasonExplained{
			Reason:      string(r),
			Explanation: "The model declined to answer this prompt. Nothing is broken and nothing was executed; rephrasing the request is the only move.",
		}, true
	case libacp.StopReasonCancelled:
		return stopReasonExplained{
			Reason:      string(r),
			Explanation: "The turn was cancelled before it finished, so any work in flight was abandoned. Whatever it had already written to disk stays written.",
		}, true
	}
	return stopReasonExplained{}, false
}

func explainSuspension(approvalID string) stopReasonExplained {
	e := stopReasonExplained{
		Reason:      stopReasonSuspended,
		Explanation: "The turn is suspended, not finished: a tool call is waiting on a human approval that nobody answered inside the park window. The run is checkpointed — answering the approval resumes it from exactly where it stopped, in this process or any other. The gated tool has not run.",
		ApprovalID:  approvalID,
	}
	if approvalID != "" {
		e.Command = "contenox approvals respond " + approvalID + " --approve  (or --deny)"
	}
	return e
}

// explainRecoveredFailure explains a turn whose task errored and whose on_failure
// handler answered in its place. The cause is quoted rather than glossed, since
// the handler's own output reads as a tidy progress summary.
func explainRecoveredFailure(cause string) stopReasonExplained {
	e := stopReasonExplained{
		Reason:      stopReasonFailed,
		Explanation: "A step of this turn failed and the chain's recovery handler answered in its place, so the reply above is a summary rather than the work. Nothing here is a context or budget problem — clearing the session will not help.",
	}
	if cause != "" {
		e.Explanation += " The failure was: " + cause
	}
	return e
}

func recoveredFailureMeta(cause string) json.RawMessage {
	meta, err := json.Marshal(map[string]stopReasonExplained{stopReasonMetaKey: explainRecoveredFailure(cause)})
	if err != nil {
		return nil
	}
	return meta
}

func recoveredFailureNotice(cause string) libacp.SessionUpdate {
	update := libacp.NewAgentMessageChunk(stopReasonMessage(explainRecoveredFailure(cause)))
	update.Meta = recoveredFailureMeta(cause)
	return update
}

func suspensionMeta(approvalID string) json.RawMessage {
	meta, err := json.Marshal(map[string]stopReasonExplained{stopReasonMetaKey: explainSuspension(approvalID)})
	if err != nil {
		return nil
	}
	return meta
}

// suspensionNotice is the agent message a parked turn announces into the
// conversation, so a park is visible in a client that reads no `_meta`.
func suspensionNotice(approvalID string) libacp.SessionUpdate {
	text := stopReasonMessage(explainSuspension(approvalID))
	if approvalID != "" {
		text += "\nApproval: " + approvalID
	}
	update := libacp.NewAgentMessageChunk(text)
	update.Meta = suspensionMeta(approvalID)
	return update
}

// stopReasonAnnounced reports whether the explanation is also pushed into the
// conversation as an agent message. A cancellation never is.
func stopReasonAnnounced(r libacp.StopReason) bool {
	return r != libacp.StopReasonCancelled
}

// explainTurnStop attaches the operator-facing form of a non-end_turn stop
// reason to a finished turn: the same trio on the response `_meta` (for a
// client that renders its own turn-end chrome in place of the bare token) and,
// unless the operator caused the stop themselves, the sentence as an agent
// message, which every client already renders. Called once per session/prompt
// at the transport boundary — the single point every driver's response passes
// through — so no driver can forget it and none can double-send it.
//
// The explanation is merged into `_meta` rather than assigned onto it: the
// driver may already have attached contenox.droppedContent, and a turn that
// both ended short and discarded an attachment must report both.
//
// An external session is left untouched: its stop reason came from a
// downstream agent, and these commands are not that agent's levers. A parked
// turn passes through unchanged too — it arrives already carrying its
// suspension `_meta` (see suspensionMeta), and its end_turn token has no
// explanation of its own to overwrite it with.
func (t *Transport) explainTurnStop(ctx context.Context, sid libacp.SessionID, sess *sessionEntry, resp libacp.PromptResponse) libacp.PromptResponse {
	if sess != nil && sess.driver != nil && sess.driver.AgentName() != "" {
		return resp
	}
	explained, ok := explainStopReason(resp.StopReason)
	if !ok {
		return resp
	}
	if stopReasonAnnounced(resp.StopReason) {
		t.sendUpdate(ctx, libacp.SessionNotification{
			SessionID: sid,
			Update:    libacp.NewAgentMessageChunk(stopReasonMessage(explained)),
		})
	}
	resp.Meta = mergeMeta(resp.Meta, stopReasonMetaKey, explained)
	return resp
}

// stopReasonMessage renders the explanation as the one line a client shows in
// the conversation, carrying the same "⚠️ " prefix command failures use so a
// turn that ended short reads as an interruption, not as agent prose.
func stopReasonMessage(e stopReasonExplained) string {
	msg := "⚠️  " + e.Explanation
	if e.Command != "" {
		msg += "\nNext: " + e.Command
	}
	return msg
}
