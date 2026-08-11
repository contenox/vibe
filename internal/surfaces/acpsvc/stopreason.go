package acpsvc

import (
	"context"
	"encoding/json"

	libacp "github.com/contenox/contenox/libacp"
)

// stopReasonMetaKey namespaces the explanation carried on a prompt response's
// `_meta`, following the same dotted convention as the mission envelopes
// (reportrouter's contenox.missionReport / contenox.missionAsk).
const stopReasonMetaKey = "contenox.stopReason"

// stopReasonSuspended names a turn that parked on a human approval rather
// than finishing. It is deliberately NOT a libacp.StopReason: ACP's set
// (end_turn, max_tokens, max_turn_requests, refusal, cancelled) has no
// suspension, and a token outside it would break every client that decodes
// stopReason as a closed enum — losing the whole prompt response, not just
// the gloss. So the spec field stays end_turn (mapStopReason) and the truth
// travels in the `_meta` slot ACP already reserves for exactly this. It is
// distinguishable from a completed turn on the wire because an ordinary
// end_turn carries no `contenox.stopReason` at all (see explainStopReason).
const stopReasonSuspended = "suspended"

// stopReasonFailed marks a turn whose task errored and whose on_failure handler
// answered in its place. Not a libacp.StopReason for the same reason
// stopReasonSuspended is not, and it travels the same way.
const stopReasonFailed = "failed"

// stopReasonExplained is the operator-facing form of a turn's stop reason:
// what happened, and the command that resolves it. ACP puts only the token
// ("max_tokens") on the wire, which is all a client can render — the sentence
// is attached here, in the core, so every editor shows what `contenox acp`
// shows rather than each inventing its own gloss.
type stopReasonExplained struct {
	Reason      string `json:"reason"`
	Explanation string `json:"explanation"`
	// Command is what the operator types next; empty when nothing resolves the
	// stop except prompting again.
	Command string `json:"command,omitempty"`
	// ApprovalID names the durable approval a suspended turn is waiting on, so
	// a client can render "waiting on approval X" instead of going quiet. Set
	// only by explainSuspension; every ACP stop reason leaves it empty.
	ApprovalID string `json:"approvalId,omitempty"`
}

// explainStopReason maps a stop reason onto its sentence. end_turn is the
// ordinary ending and has none; ok is false for it and for any reason this
// build does not recognize, which then travels as the bare token it already was.
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

// explainSuspension is explainStopReason's counterpart for a park. It is a
// separate function, not a case, because a suspension is not one of ACP's
// stop reasons (see stopReasonSuspended) and because it is the only
// explanation that varies per turn: the approval id is what an operator
// answers and what a client tracks.
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

// explainRecoveredFailure is explainStopReason's counterpart for a turn whose
// task errored and whose on_failure handler answered in its place. Separate for
// the same reasons explainSuspension is: it is not one of ACP's stop reasons,
// and it varies per turn.
//
// The cause is quoted rather than glossed. The handler's own output reads as a
// tidy progress summary, so without this the operator is told the turn made
// partial progress and never learns a call failed.
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

// recoveredFailureMeta renders a recovered failure into the response `_meta`
// under the envelope key explainTurnStop uses. Nil when it cannot be
// marshalled, leaving the response the bare end_turn it already was.
func recoveredFailureMeta(cause string) json.RawMessage {
	meta, err := json.Marshal(map[string]stopReasonExplained{stopReasonMetaKey: explainRecoveredFailure(cause)})
	if err != nil {
		return nil
	}
	return meta
}

// recoveredFailureNotice is the agent message a recovered failure announces
// into the conversation, for the clients that render no `_meta`.
func recoveredFailureNotice(cause string) libacp.SessionUpdate {
	update := libacp.NewAgentMessageChunk(stopReasonMessage(explainRecoveredFailure(cause)))
	update.Meta = recoveredFailureMeta(cause)
	return update
}

// suspensionMeta renders a park's explanation into the prompt response's
// `_meta` under the same envelope key explainTurnStop uses, so a client reads
// one slot for every short ending. Returns nil when the explanation cannot be
// marshalled, which leaves the response as the bare end_turn it already was.
func suspensionMeta(approvalID string) json.RawMessage {
	meta, err := json.Marshal(map[string]stopReasonExplained{stopReasonMetaKey: explainSuspension(approvalID)})
	if err != nil {
		return nil
	}
	return meta
}

// suspensionNotice is the agent message a parked turn announces into the
// conversation. Unlike the response `_meta` it needs no client support: every
// ACP client already renders an agent message, which is what makes a park
// visible in the editor that did not read the meta. The approval id is in the
// text and repeated on the update's own `_meta`, so a client can act on it
// without parsing prose. An empty id (a park the engine could not key) still
// announces the suspension — a turn that says nothing is the failure this
// exists to prevent.
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
// conversation as an agent message, rather than only riding the response
// `_meta`. A cancellation is the operator's own act — telling them what they
// just did is noise — so it is explained on the wire but never announced.
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
