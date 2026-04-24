package taskengine

import (
	"context"
	"encoding/json"
	"sync"
)

// WidgetHint is one inline-rendering hint emitted by a tools (or any code on
// the execution path) that the UI should render adjacent to an assistant
// message in the chat thread.
//
// The shape mirrors [ContextArtifact] in chatsessionmodes — kind + opaque
// payload — so the frontend can reuse the same artifact→inline-attachment
// mapping for both directions of state flow:
//
//   - User → LLM:   ChatContextPayload.artifacts[]   (Phase 1)
//   - LLM → user:   TaskEvent.Attachments[]          (Phase 5, this file)
//
// First-party kinds that the Beam UI knows how to render today: file_view,
// terminal_excerpt, plan_summary, dag, state_unit. The kind string is
// deliberately untyped here so a tools can emit experimental kinds without a
// coordinated taskengine release; the UI falls back to a JSON dump for
// unknown kinds.
type WidgetHint struct {
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// WidgetHintSink accumulates [WidgetHint]s emitted during a single chain run.
// The runtime drains it at task-event publish time (see [TaskEvent.Attachments]).
//
// Safe for concurrent appenders — multiple tool calls in one
// [HandleExecuteToolCalls] turn each call [AppendWidgetHint] from their own
// goroutine equivalent.
type WidgetHintSink struct {
	mu    sync.Mutex
	hints []WidgetHint
}

// Append records one hint. Safe for concurrent use; nil-safe.
func (s *WidgetHintSink) Append(hint WidgetHint) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.hints = append(s.hints, hint)
	s.mu.Unlock()
}

// Drain atomically returns all accumulated hints and clears the sink. Used by
// the task-event publisher to attach hints to the next outgoing event,
// guaranteeing each hint is reported exactly once.
func (s *WidgetHintSink) Drain() []WidgetHint {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	out := s.hints
	s.hints = nil
	s.mu.Unlock()
	return out
}

// Snapshot returns a copy without clearing — for tests / debugging.
func (s *WidgetHintSink) Snapshot() []WidgetHint {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]WidgetHint, len(s.hints))
	copy(out, s.hints)
	return out
}

type widgetHintSinkKey struct{}

// WithWidgetHintSink attaches sink to ctx so tools reachable from this
// context can call [AppendWidgetHint].
func WithWidgetHintSink(ctx context.Context, sink *WidgetHintSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, widgetHintSinkKey{}, sink)
}

// widgetHintSinkFromContext returns the sink attached via WithWidgetHintSink, if any.
func widgetHintSinkFromContext(ctx context.Context) *WidgetHintSink {
	v, _ := ctx.Value(widgetHintSinkKey{}).(*WidgetHintSink)
	return v
}

// AppendWidgetHint records hint on the context-bound sink, if one is attached.
// No-op when no sink is set so direct taskengine callers (and tests) work
// without ceremony.
//
// Tools call this AFTER successfully producing their primary tool-result
// content. Failure paths should NOT emit hints — the agent does not need a
// widget for an error that already lives in the tool message.
func AppendWidgetHint(ctx context.Context, hint WidgetHint) {
	if s := widgetHintSinkFromContext(ctx); s != nil {
		s.Append(hint)
	}
}

// AppendWidgetHintTyped is the typed variant: encodes payload to JSON and
// calls [AppendWidgetHint]. Errors are silent because UI hints are
// best-effort — a failed encode just means the user sees no widget for that
// tool call, not that the tool itself failed.
func AppendWidgetHintTyped(ctx context.Context, kind string, payload any) {
	if widgetHintSinkFromContext(ctx) == nil {
		return // avoid the marshal cost when nothing will consume it
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	AppendWidgetHint(ctx, WidgetHint{Kind: kind, Payload: raw})
}

// drainWidgetHints atomically pulls hints from the context-bound sink and
// clears it. Internal helper used by the task-event publisher.
func drainWidgetHints(ctx context.Context) []WidgetHint {
	s := widgetHintSinkFromContext(ctx)
	if s == nil {
		return nil
	}
	return s.Drain()
}
