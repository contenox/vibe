package contenoxcli

import (
	"context"
	"errors"
	"fmt"

	"github.com/contenox/contenox/internal/kernel/llmresolver"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/models/llmrepo"
	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/libtracker"
)

const transcribeInstruction = "Transcribe this audio recording faithfully and completely. " +
	"Return only the transcript text, with no preamble, headers, or commentary. " +
	"If the audio contains no speech, reply exactly: (no speech detected)"

func newAudioTranscriber(models llmrepo.ModelRepo, tracker libtracker.ActivityTracker) localtools.AudioTranscriber {
	return func(ctx context.Context, data []byte, mimeType string) (string, error) {
		messages := []libmodelprovider.Message{{
			Role:    "user",
			Content: transcribeInstruction,
			Audio:   []libmodelprovider.AudioPart{{Data: data, MimeType: mimeType}},
		}}
		res, _, err := models.Chat(ctx, llmrepo.Request{Tracker: tracker}, messages)
		if err != nil {
			// Appends the config key so the message is actionable, matching the unconfigured-seam refusal in localtools.
			if errors.Is(err, llmresolver.ErrNoAudioCapableModel) {
				return "", fmt.Errorf("%w — configure an audio-capable model with `contenox config set default-audio-model <model>` (config key: default-audio-model)", err)
			}
			return "", err
		}
		return res.Message.Content, nil
	}
}

func bindAudioTranscriber(tools map[string]taskengine.ToolsRepo, engine *Engine) {
	if engine == nil || engine.Models == nil {
		return
	}
	fs, ok := tools["local_fs"].(*localtools.LocalFSTools)
	if !ok {
		return
	}
	fs.BindAudioTranscriber(newAudioTranscriber(engine.Models, engine.Tracker))
}
