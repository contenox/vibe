package acpsvc

import (
	"context"
	"encoding/json"
	"strings"

	libacp "github.com/contenox/contenox/libacp"
)

// droppedContentMetaKey namespaces the dropped-prompt-content report carried on
// a prompt response's `_meta`, following the same dotted convention as
// stopReasonMetaKey. It is a sibling of contenox.stopReason, never a
// replacement: a turn can both park on an approval and have discarded an
// attachment, and a client must be able to read either fact without the other
// hiding it (see mergeMeta).
const droppedContentMetaKey = "contenox.droppedContent"

// droppedContentReport is the operator-facing form of the prompt content that
// never reached the model. libacp.FlattenContent already computes the kinds
// (its text projection cannot carry an image, an audio block, or a binary
// resource) and the native driver adds the image kind when the turn is a slash
// command, which has no use for an attachment. Until this envelope existed the
// list went only to the tracker, so a client that attached a photo got a
// successful turn answered as if nothing had been sent — a loss shaped like a
// success, which is the failure this reports.
type droppedContentReport struct {
	// Kinds are the ACP content-block types that were discarded, deduplicated
	// and in first-seen order, exactly as FlattenContent reported them.
	Kinds []string `json:"kinds"`
	// Explanation is the sentence a client renders in place of the bare kind
	// list, so every editor says the same thing about a discarded attachment.
	Explanation string `json:"explanation"`
}

// explainDroppedContent maps a set of discarded content kinds onto its report.
// ok is false for an empty set, which is what makes absence meaningful: a turn
// that dropped nothing emits no envelope at all, so a client can read presence
// alone as "something was lost" without inspecting the payload.
func explainDroppedContent(kinds []string) (droppedContentReport, bool) {
	if len(kinds) == 0 {
		return droppedContentReport{}, false
	}
	return droppedContentReport{
		Kinds: kinds,
		Explanation: "Part of the prompt never reached the model and had no effect on the answer: " +
			strings.Join(kinds, ", ") +
			". The turn ran on the remaining text alone. Either this path takes text only (a slash command does), the attachment could not be decoded, or the session's model does not accept it — so the reply will keep ignoring it until it is resent somewhere that does.",
	}, true
}

// droppedContentMessage renders the report as the one line a client shows in
// the conversation, carrying the same "⚠️ " prefix as command failures and
// short turn endings so a discarded attachment reads as an interruption rather
// than as agent prose.
func droppedContentMessage(r droppedContentReport) string {
	return "⚠️  " + r.Explanation
}

// droppedContentNotice is the agent message a turn with discarded content
// announces into the conversation. It exists for the same reason the
// suspension notice does: the response `_meta` needs client support, while
// every ACP client already renders an agent message, so an editor that reads
// nothing custom still shows the loss. The kinds are repeated on the update's
// own `_meta` so a client can act on them without parsing prose. ok is false
// when nothing was dropped, and nothing is announced.
func droppedContentNotice(kinds []string) (libacp.SessionUpdate, bool) {
	report, ok := explainDroppedContent(kinds)
	if !ok {
		return libacp.SessionUpdate{}, false
	}
	update := libacp.NewAgentMessageChunk(droppedContentMessage(report))
	update.Meta = mergeMeta(nil, droppedContentMetaKey, report)
	return update, true
}

// announceDroppedContent pushes the notice into the session, through emit —
// t.sendUpdate on the connection-bound path, the turn's journaling emitter on
// the survival path (see runNativeTurn), so a reattaching client reads it too.
// Called before the turn runs, so the operator learns the attachment is gone
// ahead of the answer that ignores it rather than after. A no-op when nothing
// was dropped.
func announceDroppedContent(ctx context.Context, sid libacp.SessionID, kinds []string, emit func(context.Context, libacp.SessionNotification)) {
	notice, ok := droppedContentNotice(kinds)
	if !ok || emit == nil {
		return
	}
	emit(ctx, libacp.SessionNotification{SessionID: sid, Update: notice})
}

// withDroppedContentMeta attaches the report to a prompt response's `_meta`,
// preserving anything already there — a parked turn's contenox.stopReason in
// particular. Returns resp untouched when nothing was dropped, which is what
// lets a client treat the key's presence as the whole signal.
func withDroppedContentMeta(resp libacp.PromptResponse, kinds []string) libacp.PromptResponse {
	report, ok := explainDroppedContent(kinds)
	if !ok {
		return resp
	}
	resp.Meta = mergeMeta(resp.Meta, droppedContentMetaKey, report)
	return resp
}

// mergeMeta sets key on an ACP `_meta` object without disturbing its other
// entries, and is why two independent facts can ride one slot: the envelopes
// are namespaced siblings, so writing one must never be a blind overwrite of
// the other. Returns meta unchanged when value cannot be marshalled or when
// meta is present but is not a JSON object — a malformed merge would cost the
// caller the explanation it already had.
func mergeMeta(meta json.RawMessage, key string, value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return meta
	}
	envelope := map[string]json.RawMessage{}
	if len(meta) > 0 {
		if err := json.Unmarshal(meta, &envelope); err != nil {
			return meta
		}
	}
	envelope[key] = encoded
	merged, err := json.Marshal(envelope)
	if err != nil {
		return meta
	}
	return merged
}
