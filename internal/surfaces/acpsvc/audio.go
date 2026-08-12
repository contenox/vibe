package acpsvc

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/libacp"
)

// extractAudioParts is extractImageParts' audio sibling: it splits content
// blocks into audio attachments (as taskengine.AudioPart, for CanAudio
// providers) and the rest, which still go through libacp.FlattenContent —
// otherwise its lossy text projection would drop audio silently.
//
// On top of the image path's undecodable-base64 rule, the two provider-side
// inline-audio bounds are enforced per block here, at the surface: a block
// whose media type is outside modelrepo.SupportedAudioMimeTypes, or whose
// decoded bytes would push the prompt's raw audio total past
// modelrepo.MaxInlineAudioBytes, is returned in rest exactly like a broken
// one. Refusing it here keeps the loss per block and visible — it surfaces as
// a dropped kind whose report names the bounds (see explainDroppedContent) —
// where forwarding it would fail the whole turn at provider request building,
// taking the blocks that were acceptable down with it.
func extractAudioParts(blocks []libacp.ContentBlock) (audio []taskengine.AudioPart, rest []libacp.ContentBlock) {
	total := 0
	for _, block := range blocks {
		if block.Type != string(libacp.ContentKindAudio) {
			rest = append(rest, block)
			continue
		}
		data, err := base64.StdEncoding.DecodeString(block.Data)
		if err != nil || len(data) == 0 {
			rest = append(rest, block)
			continue
		}
		if !modelrepo.SupportedAudioMimeTypes[block.MimeType] || total+len(data) > modelrepo.MaxInlineAudioBytes {
			rest = append(rest, block)
			continue
		}
		total += len(data)
		audio = append(audio, taskengine.AudioPart{Data: data, MimeType: block.MimeType})
	}
	return audio, rest
}

// audioAcceptanceSentence names what inline audio needs to survive this
// surface — the two per-block bounds extractAudioParts enforces plus the
// audio-capable-model requirement the pre-flight gate enforces (see
// sessionAudioRefusal) — for the dropped-content explanation. Rendered from
// the modelrepo constants — accepted types sorted for stable wording — so the
// copy a client shows can never drift from what is actually refused.
func audioAcceptanceSentence() string {
	types := make([]string, 0, len(modelrepo.SupportedAudioMimeTypes))
	for t := range modelrepo.SupportedAudioMimeTypes {
		types = append(types, t)
	}
	sort.Strings(types)
	return fmt.Sprintf("Audio is accepted as %s, up to %d MiB of decoded audio per prompt, and needs an audio-capable model (the default-audio-model config key sets one); audio outside those bounds, or audio for a session without such a model, is refused here, before the model.",
		strings.Join(types, ", "), modelrepo.MaxInlineAudioBytes>>20)
}

// sessionAudioRefusal is the pre-flight capability gate for a prompt carrying
// audio: it answers, before dispatch, whether this session's audio is doomed —
// from the same observed runtime capability state the model dropdown and the
// context-window lookup already read, never a resolution roundtrip. Refusing
// here instead of letting resolution refuse mid-chain matters twice over: the
// turn proceeds on the rest of the prompt instead of dying as an RPC error,
// and the audio never reaches the user message — an audio-bearing message
// persisted to history re-imposes the audio requirement on every later turn
// of the session, text-only ones included. Returns the operator-facing reason,
// or "" to forward the audio.
func (t *Transport) sessionAudioRefusal(ctx context.Context, sess *sessionEntry) string {
	return audioCapabilityRefusal(t.runtimeStates(ctx), sess.modelOrDefault(t.model()))
}

// audioCapabilityRefusal is the decision under sessionAudioRefusal, pure over
// the observed fleet state and the session's effective (pinned) model. It
// refuses only on positive knowledge, mirroring the resolver's own two
// refusals (llmresolver.ErrPinnedModelLacksAudio / ErrNoAudioCapableModel):
// a pinned model the fleet reports as non-audio, or a fleet with models and
// none audio-capable. No state, no models, or a pin the state has not seen is
// UNKNOWN, not incapable — the audio forwards and the resolver keeps the
// final word, so a missing state service can never eat an attachment.
func audioCapabilityRefusal(states []runtimestate.BackendRuntimeState, pinnedModel string) string {
	modelSeen := false
	pinnedSeen := false
	pinnedCanAudio := false
	anyCanAudio := false
	for _, state := range states {
		for _, pulled := range state.PulledModels {
			modelSeen = true
			if pulled.CanAudio {
				anyCanAudio = true
			}
			if pinnedModel != "" && pulled.Model == pinnedModel {
				pinnedSeen = true
				pinnedCanAudio = pinnedCanAudio || pulled.CanAudio
			}
		}
	}
	switch {
	case pinnedSeen && !pinnedCanAudio:
		return fmt.Sprintf("The session's model %q does not accept audio input, so the audio was not sent and the turn ran without it. Select an audio-capable model for this session, or set the default-audio-model config key, then resend the audio.", pinnedModel)
	case modelSeen && !anyCanAudio:
		return "No configured model accepts audio input, so the audio was not sent and the turn ran without it. Configure an audio-capable model (the default-audio-model config key sets one), then resend the audio."
	}
	return ""
}
