package localtools

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

// AudioTranscriber turns audio bytes into transcript text; nil means none is
// configured, and read_file refuses audio files.
type AudioTranscriber func(ctx context.Context, data []byte, mimeType string) (string, error)

// WithAudioTranscriber wires the transcription seam at construction; nil by
// default.
func WithAudioTranscriber(fn AudioTranscriber) FSOption {
	return func(h *LocalFSTools) { h.transcriber = fn }
}

// BindAudioTranscriber wires the transcription seam after construction;
// guarded against Exec calls already running on task-engine goroutines.
func (h *LocalFSTools) BindAudioTranscriber(fn AudioTranscriber) {
	h.transcriberMu.Lock()
	h.transcriber = fn
	h.transcriberMu.Unlock()
}

func (h *LocalFSTools) audioTranscriber() AudioTranscriber {
	h.transcriberMu.RLock()
	defer h.transcriberMu.RUnlock()
	return h.transcriber
}

type audioDetection int

const (
	notAudio audioDetection = iota
	supportedAudio
	unsupportedAudio
)

const supportedAudioFormats = "wav, mp3, m4a, ogg, flac"

func classifyAudio(prefix []byte, ext string) (mime string, det audioDetection) {
	if len(prefix) == 0 {
		return "", notAudio
	}
	ext = strings.ToLower(ext)
	switch ct := http.DetectContentType(prefix); ct {
	case "audio/wave":
		return "audio/wav", supportedAudio
	case "audio/mpeg":
		return "audio/mpeg", supportedAudio
	case "application/ogg":
		return "audio/ogg", supportedAudio
	case "audio/aiff", "audio/basic", "audio/midi":
		return ct, unsupportedAudio
	case "video/mp4":
		if m4aFtypBrand(prefix) || ext == ".m4a" {
			return "audio/mp4", supportedAudio
		}
		return "", notAudio
	}
	if bytes.HasPrefix(prefix, []byte("fLaC")) {
		return "audio/flac", supportedAudio
	}
	if m4aFtypBrand(prefix) {
		return "audio/mp4", supportedAudio
	}
	if mp3FrameSync(prefix) && ext == ".mp3" {
		return "audio/mpeg", supportedAudio
	}
	return "", notAudio
}

func m4aFtypBrand(prefix []byte) bool {
	if len(prefix) < 12 || !bytes.Equal(prefix[4:8], []byte("ftyp")) {
		return false
	}
	brand := strings.ToUpper(string(prefix[8:11]))
	return brand == "M4A" || brand == "M4B" || brand == "M4P"
}

func mp3FrameSync(prefix []byte) bool {
	if len(prefix) < 2 || prefix[0] != 0xFF || prefix[1]&0xE0 != 0xE0 {
		return false
	}
	version := (prefix[1] >> 3) & 0x03
	layer := (prefix[1] >> 1) & 0x03
	return version != 0x01 && layer != 0x00
}

func audioLikeExtension(ext string) bool {
	switch strings.ToLower(ext) {
	case ".wav", ".mp3", ".m4a", ".m4b", ".ogg", ".oga", ".opus", ".flac",
		".aac", ".aiff", ".aif", ".wma", ".mka", ".mid", ".midi", ".amr", ".au", ".weba":
		return true
	}
	return false
}

func refuseUnsupportedAudio(display, detected string) error {
	if detected != "" {
		return recoverablef(
			"local_fs: read_file: %s sniffs as %s, an audio format transcription does not support (supported: %s). Use stat_file or shell tools instead.",
			display, detected, supportedAudioFormats)
	}
	return recoverablef(
		"local_fs: read_file: %s has an audio extension but its content matches no supported audio format (supported: %s). Use stat_file or shell tools instead.",
		display, supportedAudioFormats)
}

func (h *LocalFSTools) readAudioFile(ctx context.Context, display, absPath string, info os.FileInfo, mimeType string) (any, taskengine.DataType, error) {
	transcribe := h.audioTranscriber()
	if transcribe == nil {
		return nil, taskengine.DataTypeAny, recoverablef(
			"local_fs: read_file: %s is an audio file (%s, %s); reading it returns a transcript, but no audio model is configured — set one with `contenox config set default-audio-model <model>` (config key: default-audio-model), then retry",
			display, mimeType, humanSize(info.Size()))
	}

	// The audio cap replaces _max_read_bytes here: the ceiling tracks the
	// inline media limit, not the text-read budget.
	limit, unlimited := h.maxAudioBytesFromPolicy(ctx)
	if !unlimited && info.Size() > limit {
		return nil, taskengine.DataTypeAny, recoverablef(
			"local_fs: read_file: %s is %s (%d bytes), over the %d-byte audio transcription cap; transcribe a smaller file or raise _max_audio_bytes in tools_policies.local_fs",
			display, humanSize(info.Size()), info.Size(), limit)
	}

	data, err := h.fileIO.ReadFile(ctx, absPath)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to read file: %w", err)
	}
	transcript, err := transcribe(ctx, data, mimeType)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: read_file: transcribing %s (%s) via the audio model failed: %w", display, mimeType, err)
	}
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		transcript = "(the audio model returned an empty transcript)"
	}

	out := fmt.Sprintf(
		"local_fs: read_file: %s is audio (%s, %s); the file was consumed by the configured audio model and this transcript is returned in place of raw bytes:\n\n%s",
		display, mimeType, humanSize(info.Size()), transcript)

	// Same output-cap discipline as every other result: never a silent cut.
	outLimit, unlimitedOut := h.maxOutputBytesFromPolicy(ctx)
	if !unlimitedOut && int64(len(out)) > outLimit {
		keep := int(outLimit)
		if keep > len(out) {
			keep = len(out)
		}
		notice := fmt.Sprintf(
			"local_fs: read_file transcript truncated — output capped at %d bytes; raise _max_output_bytes in tools_policies.local_fs for the full transcript. %s",
			outLimit, severityRecoverable)
		return out[:keep] + "\n\n" + notice, taskengine.DataTypeString, nil
	}
	return out, taskengine.DataTypeString, nil
}
