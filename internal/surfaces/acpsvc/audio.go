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

// extractAudioParts splits content blocks into audio attachments and the rest.
// A block outside modelrepo's media-type or inline-size bounds is returned in
// rest, so the loss stays per block instead of failing the whole turn.
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

// audioAcceptanceSentence names what inline audio needs to survive this surface,
// rendered from the modelrepo constants so the copy cannot drift.
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
// audio. It returns the operator-facing reason, or "" to forward the audio.
func (t *Transport) sessionAudioRefusal(ctx context.Context, sess *sessionEntry) string {
	return audioCapabilityRefusal(t.runtimeStates(ctx), sess.modelOrDefault(t.model()))
}

// audioCapabilityRefusal refuses only on positive knowledge: a pinned model the
// fleet reports as non-audio, or a fleet with models and none audio-capable.
// Anything unknown forwards, leaving the resolver the final word.
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
