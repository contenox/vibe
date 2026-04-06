package chatsessionmodes

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/contenox/contenox/taskengine"
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
		if len(payload) > maxArtifactPayloadBytes {
			return nil, fmt.Errorf("context.artifacts[%d].payload exceeds maximum size", i)
		}
		body := fmt.Sprintf("[Context kind=%s]\n%s", kind, string(payload))
		totalBytes += len(body)
		if totalBytes > maxContextInjectionBytes {
			return nil, fmt.Errorf("total injected context exceeds maximum size (%d bytes)", maxContextInjectionBytes)
		}
		out = append(out, taskengine.Message{
			ID:        uuid.NewString(),
			Role:      "system",
			Content:   body,
			Timestamp: now,
		})
	}
	return out, nil
}
