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
// An external session is left untouched: its stop reason came from a
// downstream agent, and these commands are not that agent's levers.
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
	if meta, err := json.Marshal(map[string]stopReasonExplained{stopReasonMetaKey: explained}); err == nil {
		resp.Meta = meta
	}
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
