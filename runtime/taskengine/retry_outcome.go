package taskengine

import (
	"context"
	"sync"

	"github.com/contenox/contenox/runtime/taskengine/llmretry"
)

// RetryOutcomeSink collects per-call retry outcomes from chat_completion tasks
// running inside one chain invocation. It is safe for concurrent appenders.
//
// planservice attaches a sink via [WithRetryOutcomeSink] before running a
// compiled plan chain so it can observe whether any chat call retried, used
// fallback, or hit a non-retryable class (e.g. capacity) — used to decide
// auto-replan in §3.
type RetryOutcomeSink struct {
	mu       sync.Mutex
	outcomes []llmretry.Outcome
}

// Append records one outcome. Safe for concurrent use.
func (s *RetryOutcomeSink) Append(o llmretry.Outcome) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.outcomes = append(s.outcomes, o)
	s.mu.Unlock()
}

// Outcomes returns a snapshot of recorded outcomes in append order.
func (s *RetryOutcomeSink) Outcomes() []llmretry.Outcome {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]llmretry.Outcome, len(s.outcomes))
	copy(out, s.outcomes)
	return out
}

// LastErrorClass returns the class of the most recent recorded outcome, or
// [llmretry.ClassNone] if no outcomes were recorded.
func (s *RetryOutcomeSink) LastErrorClass() llmretry.ErrorClass {
	if s == nil {
		return llmretry.ClassNone
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.outcomes) == 0 {
		return llmretry.ClassNone
	}
	return s.outcomes[len(s.outcomes)-1].LastErrorClass
}

type retryOutcomeSinkKey struct{}

// WithRetryOutcomeSink attaches sink to ctx so chat_completion tasks can append
// outcomes via [appendRetryOutcome].
func WithRetryOutcomeSink(ctx context.Context, sink *RetryOutcomeSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, retryOutcomeSinkKey{}, sink)
}

// retryOutcomeSinkFromContext returns the sink attached via WithRetryOutcomeSink, if any.
func retryOutcomeSinkFromContext(ctx context.Context) *RetryOutcomeSink {
	v, _ := ctx.Value(retryOutcomeSinkKey{}).(*RetryOutcomeSink)
	return v
}

// appendRetryOutcome records o on the context-bound sink, if one is attached.
// No-op when no sink is set so tests and direct taskengine callers are unaffected.
func appendRetryOutcome(ctx context.Context, o llmretry.Outcome) {
	if s := retryOutcomeSinkFromContext(ctx); s != nil {
		s.Append(o)
	}
}
