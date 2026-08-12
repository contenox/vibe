package modelrepo

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Typed audio-input refusal sentinels; all are deterministic client-side refusals raised before the wire.
var (
	// ErrAudioNotSupported is returned when a message carries audio but the provider integration has no audio encoding.
	ErrAudioNotSupported = errors.New("model cannot accept audio input")
	// ErrUnsupportedAudioMime is returned when an audio attachment's media type
	// is outside the accepted set (see SupportedAudioMimeTypes).
	ErrUnsupportedAudioMime = errors.New("unsupported audio media type")
	// ErrAudioTooLarge is returned when a request's inline audio exceeds
	// MaxInlineAudioBytes.
	ErrAudioTooLarge = errors.New("inline audio exceeds the request size limit")
)

// SupportedAudioMimeTypes is the accepted set of audio media types for inline attachments; matching is exact against canonical types.
var SupportedAudioMimeTypes = map[string]bool{
	"audio/wav":  true,
	"audio/mpeg": true,
	"audio/mp4":  true,
	"audio/ogg":  true,
	"audio/flac": true,
}

// MaxInlineAudioBytes caps the total raw audio bytes carried inline across all messages in one request.
const MaxInlineAudioBytes = 14 << 20

func supportedAudioMimeList() string {
	types := make([]string, 0, len(SupportedAudioMimeTypes))
	for t := range SupportedAudioMimeTypes {
		types = append(types, t)
	}
	sort.Strings(types)
	return strings.Join(types, ", ")
}

func countAudioParts(messages []Message) (parts int, totalBytes int) {
	for _, m := range messages {
		for _, a := range m.Audio {
			parts++
			totalBytes += len(a.Data)
		}
	}
	return parts, totalBytes
}

// RefuseAudioInput returns a typed refusal when any message carries audio and the named provider has no audio encoding.
func RefuseAudioInput(providerType, modelName string, messages []Message) error {
	parts, _ := countAudioParts(messages)
	if parts == 0 {
		return nil
	}
	return fmt.Errorf("%w: provider %s (model %s) received %d audio attachment(s) but has no audio input support; use an audio-capable model or remove the audio attachments",
		ErrAudioNotSupported, providerType, modelName, parts)
}

// ValidateAudioParts enforces accepted media types and the inline size cap for audio-capable providers, before the wire.
func ValidateAudioParts(providerType, modelName string, messages []Message) error {
	parts, totalBytes := countAudioParts(messages)
	if parts == 0 {
		return nil
	}
	for _, m := range messages {
		for _, a := range m.Audio {
			if !SupportedAudioMimeTypes[a.MimeType] {
				return fmt.Errorf("%w: %q is not accepted by provider %s (model %s); accepted types: %s",
					ErrUnsupportedAudioMime, a.MimeType, providerType, modelName, supportedAudioMimeList())
			}
		}
	}
	if totalBytes > MaxInlineAudioBytes {
		return fmt.Errorf("%w: request carries %d bytes of audio across %d attachment(s) but provider %s (model %s) accepts at most %d bytes inline; send smaller or fewer audio files",
			ErrAudioTooLarge, totalBytes, parts, providerType, modelName, MaxInlineAudioBytes)
	}
	return nil
}
