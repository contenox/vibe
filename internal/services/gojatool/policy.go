package gojatool

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

// Chain-level knobs, read per execution from the context under the name this
// toolset is registered as. They only tighten: every value is clamped to the
// same hard ceilings a Config is.
const (
	PolicyDeadlineMS     = "_deadline_ms"
	PolicyMaxDeadlineMS  = "_max_deadline_ms"
	PolicyMaxOutputBytes = "_max_output_bytes"
	PolicyMaxHostCalls   = "_max_host_calls"
)

func (s *sandbox) effective(ctx context.Context) limits {
	args := taskengine.ToolsArgsFromContext(ctx, s.provider)
	if len(args) == 0 {
		return s.base
	}
	lim := s.base
	if ms, ok := policyInt(args, PolicyMaxDeadlineMS); ok {
		lim.maxDeadline = time.Duration(ms) * time.Millisecond
	}
	if ms, ok := policyInt(args, PolicyDeadlineMS); ok {
		lim.deadline = time.Duration(ms) * time.Millisecond
	}
	if n, ok := policyInt(args, PolicyMaxOutputBytes); ok {
		lim.outputCap = n
	}
	if n, ok := policyInt(args, PolicyMaxHostCalls); ok {
		lim.maxHostCalls = n
	}
	return clampLimits(lim)
}

// policyInt ignores a value that does not parse or is not positive, leaving
// the default standing: a typo in a policy file must not remove a bound.
func policyInt(args map[string]string, key string) (int, bool) {
	raw, ok := args[key]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
