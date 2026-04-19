package taskengine

import (
	"context"

	"github.com/contenox/contenox/runtime/taskengine/compact"
)

// CompactionStateRegistry holds per-task [compact.State] across multiple
// invocations of the same chat task within one chain run, so the circuit
// breaker carries between agentic-loop rounds.
//
// planservice attaches a registry via [WithCompactionRegistry] before running
// a compiled plan chain. Each chat_completion task looks up state by task ID;
// missing entries are created lazily.
type CompactionStateRegistry struct {
	states map[string]*compact.State
}

// NewCompactionStateRegistry returns an empty registry.
func NewCompactionStateRegistry() *CompactionStateRegistry {
	return &CompactionStateRegistry{states: map[string]*compact.State{}}
}

// Get returns the state for taskID, creating it on first access.
func (r *CompactionStateRegistry) Get(taskID string) *compact.State {
	if r == nil {
		return nil
	}
	if r.states == nil {
		r.states = map[string]*compact.State{}
	}
	st, ok := r.states[taskID]
	if !ok {
		st = &compact.State{}
		r.states[taskID] = st
	}
	return st
}

type compactionRegistryKey struct{}

// WithCompactionRegistry attaches reg to ctx so chat_completion tasks can look
// up persistent [compact.State] for compaction circuit-breaking.
func WithCompactionRegistry(ctx context.Context, reg *CompactionStateRegistry) context.Context {
	if reg == nil {
		return ctx
	}
	return context.WithValue(ctx, compactionRegistryKey{}, reg)
}

func compactionRegistryFromContext(ctx context.Context) *CompactionStateRegistry {
	v, _ := ctx.Value(compactionRegistryKey{}).(*CompactionStateRegistry)
	return v
}
