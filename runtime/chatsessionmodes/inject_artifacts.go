package chatsessionmodes

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/google/uuid"
)

const (
	maxContextInjectionBytes = 65536
	maxArtifactPayloadBytes  = 32768
)

var artifactKindPattern = regexp.MustCompile(`^[a-zA-Z0-9_\-]{1,64}$`)

// ClientArtifactInjector turns request context.artifacts into system messages.
type ClientArtifactInjector struct{}

// Inject implements Injector.
func (ClientArtifactInjector) Inject(_ context.Context, in InjectInput) ([]taskengine.Message, error) {
	return BuildInjectedSystemMessages(in.Turn.Context, in.Now)
}

// BuildInjectedSystemMessages turns structured context into system messages.
//
// For every artifact it:
//  1. validates the kind against [artifactKindPattern];
//  2. enforces the per-kind byte cap (or the global [maxArtifactPayloadBytes]);
//  3. dispatches to the typed renderer from the kind registry when the kind is
//     known, or falls back to the flat `<raw-json>` body for unknown kinds so
//     older clients keep working;
//  4. prepends the uniform `[Context kind=<kind>]` header so the LLM always
//     sees a consistent delimiter.
//
// The cumulative body size across all emitted messages is capped at
// [maxContextInjectionBytes] to prevent a pathological turn from blowing the
// prompt budget.
func BuildInjectedSystemMessages(ctxPayload *ContextPayload, now time.Time) ([]taskengine.Message, error) {
	if ctxPayload == nil || len(ctxPayload.Artifacts) == 0 {
		return nil, nil
	}
	var totalBytes int
	out := make([]taskengine.Message, 0, len(ctxPayload.Artifacts))
	for i := range ctxPayload.Artifacts {
		a := &ctxPayload.Artifacts[i]
		kind := strings.TrimSpace(a.Kind)
		if kind == "" {
			return nil, fmt.Errorf("context.artifacts[%d].kind is required", i)
		}
		if !artifactKindPattern.MatchString(kind) {
			return nil, fmt.Errorf("context.artifacts[%d].kind must match [a-zA-Z0-9_-]{1,64}", i)
		}
		payload := a.Payload
		if len(payload) == 0 {
			payload = json.RawMessage(`{}`)
		}

		// Per-kind cap (if declared) overrides the global default — tighter kinds
		// like plan_step don't need 32 KiB.
		cap := maxArtifactPayloadBytes
		if spec, ok := lookupKindSpec(kind); ok && spec.MaxBytes > 0 {
			cap = spec.MaxBytes
		}
		if len(payload) > cap {
			return nil, fmt.Errorf("context.artifacts[%d].payload exceeds maximum size (%d > %d)", i, len(payload), cap)
		}

		body, err := renderArtifact(kind, payload)
		if err != nil {
			return nil, fmt.Errorf("context.artifacts[%d]: %w", i, err)
		}

		framed := fmt.Sprintf("[Context kind=%s]\n%s", kind, body)
		totalBytes += len(framed)
		if totalBytes > maxContextInjectionBytes {
			return nil, fmt.Errorf("total injected context exceeds maximum size (%d bytes)", maxContextInjectionBytes)
		}
		out = append(out, taskengine.Message{
			ID:        uuid.NewString(),
			Role:      "system",
			Content:   framed,
			Timestamp: now,
		})
	}
	return out, nil
}

// renderArtifact dispatches to a typed renderer when the kind is registered,
// and falls back to the raw JSON string for unknown kinds. Keeps the legacy
// contract (`[Context kind=<kind>]\n<json>`) for any kind Beam doesn't yet
// recognize, so adding new UI-side kinds never needs a coordinated backend
// release.
func renderArtifact(kind string, payload json.RawMessage) (string, error) {
	if spec, ok := lookupKindSpec(kind); ok && spec.Render != nil {
		return spec.Render(payload)
	}
	return string(payload), nil
}
