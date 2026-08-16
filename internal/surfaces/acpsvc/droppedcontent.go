package acpsvc

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	libacp "github.com/contenox/contenox/libacp"
)

// droppedContentMetaKey namespaces the dropped-prompt-content report carried on
// a prompt response's `_meta`, a sibling of contenox.stopReason.
const droppedContentMetaKey = "contenox.droppedContent"

// droppedContentReport is the operator-facing form of the prompt content that
// never reached the model.
type droppedContentReport struct {
	Kinds       []string `json:"kinds"`
	Explanation string   `json:"explanation"`
}

// explainDroppedContent maps a set of discarded content kinds onto its report;
// ok is false for an empty set. An empty audioReason renders the generic audio
// sentence.
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

func appendDroppedKind(kinds []string, kind string) []string {
	if slices.Contains(kinds, kind) {
		return kinds
	}
	return append(kinds, kind)
}

func droppedContentMessage(r droppedContentReport) string {
	return "⚠️  " + r.Explanation
}

// droppedContentNotice is the agent message a turn with discarded content
// announces into the conversation; ok is false when nothing was dropped.
func droppedContentNotice(kinds []string, audioReason string) (libacp.SessionUpdate, bool) {
	report, ok := explainDroppedContent(kinds, audioReason)
	if !ok {
		return libacp.SessionUpdate{}, false
	}
	update := libacp.NewAgentMessageChunk(droppedContentMessage(report))
	update.Meta = mergeMeta(nil, droppedContentMetaKey, report)
	return update, true
}

// announceDroppedContent pushes the notice into the session through emit; a
// no-op when nothing was dropped.
func announceDroppedContent(ctx context.Context, sid libacp.SessionID, kinds []string, audioReason string, emit func(context.Context, libacp.SessionNotification)) {
	notice, ok := droppedContentNotice(kinds, audioReason)
	if !ok || emit == nil {
		return
	}
	emit(ctx, libacp.SessionNotification{SessionID: sid, Update: notice})
}

// withDroppedContentMeta attaches the report to a prompt response's `_meta`,
// preserving anything already there, and returns resp untouched when nothing was
// dropped.
func withDroppedContentMeta(resp libacp.PromptResponse, kinds []string) libacp.PromptResponse {
	report, ok := explainDroppedContent(kinds, "")
	if !ok {
		return resp
	}
	resp.Meta = mergeMeta(resp.Meta, droppedContentMetaKey, report)
	return resp
}

// mergeMeta sets key on an ACP `_meta` object without disturbing its other
// entries, returning meta unchanged when value cannot be marshalled or meta is
// not a JSON object.
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
