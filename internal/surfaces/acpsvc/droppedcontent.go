package acpsvc

import (
	"context"
	"encoding/json"
	"slices"
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
// never reached the model. libacp.FlattenContent already computes the kinds:
// its text projection cannot carry a binary resource, an unknown block type,
// or an image or audio block that extraction refused (see extractImageParts
// and extractAudioParts — blocks they accept never reach the projection). The
// native driver adds the image and audio kinds when the turn is a slash
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
//
// audioReason is the specific sentence naming why audio was refused, when the
// refusing site knows it (the pre-flight capability gate does — see
// sessionAudioRefusal); "" renders the generic audio bounds-and-remedy
// sentence instead. Dropped audio always gets one or the other: the bounds
// and the remedy are knowable here, so they are named, never left for the
// operator to guess.
func explainDroppedContent(kinds []string, audioReason string) (droppedContentReport, bool) {
	if len(kinds) == 0 {
		return droppedContentReport{}, false
	}
	explanation := "Part of the prompt never reached the model and had no effect on the answer: " +
		strings.Join(kinds, ", ") +
		". The turn ran on the rest of the prompt alone. Either this path takes text only (a slash command does), the attachment could not be decoded, or the session's model does not accept it — so the reply will keep ignoring it until it is resent somewhere that does."
	if slices.Contains(kinds, string(libacp.ContentKindAudio)) {
		if audioReason == "" {
			audioReason = audioAcceptanceSentence()
		}
		explanation += " " + audioReason
	}
	return droppedContentReport{Kinds: kinds, Explanation: explanation}, true
}

// appendDroppedKind records kind in the dropped set, preserving the report's
// deduplicated first-seen order when the same kind is dropped twice for
// different reasons (one attachment refused by extraction, another discarded
// by a command path): to the client both are one fact, "this kind was lost".
func appendDroppedKind(kinds []string, kind string) []string {
	if slices.Contains(kinds, kind) {
		return kinds
	}
	return append(kinds, kind)
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
// when nothing was dropped, and nothing is announced. audioReason is passed
// through to explainDroppedContent.
func droppedContentNotice(kinds []string, audioReason string) (libacp.SessionUpdate, bool) {
	report, ok := explainDroppedContent(kinds, audioReason)
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
// was dropped. audioReason (see explainDroppedContent) rides the notice only:
// it is the human-facing surface, and threading it further would demand a
// kernel change (nativeturn.Result carries kinds alone).
func announceDroppedContent(ctx context.Context, sid libacp.SessionID, kinds []string, audioReason string, emit func(context.Context, libacp.SessionNotification)) {
	notice, ok := droppedContentNotice(kinds, audioReason)
	if !ok || emit == nil {
		return
	}
	emit(ctx, libacp.SessionNotification{SessionID: sid, Update: notice})
}

// withDroppedContentMeta attaches the report to a prompt response's `_meta`,
// preserving anything already there — a parked turn's contenox.stopReason in
// particular. Returns resp untouched when nothing was dropped, which is what
// lets a client treat the key's presence as the whole signal. The explanation
// here is always the generic one: on the survival path this is rebuilt from a
// nativeturn.Result, which carries only the kinds — and the generic audio
// sentence still names the bounds and the default-audio-model remedy, so no
// surface loses them.
func withDroppedContentMeta(resp libacp.PromptResponse, kinds []string) libacp.PromptResponse {
	report, ok := explainDroppedContent(kinds, "")
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
