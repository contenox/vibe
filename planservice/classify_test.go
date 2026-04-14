package planservice

import (
	"context"
	"fmt"
	"testing"

	"github.com/contenox/contenox/planstore"
	"github.com/contenox/contenox/taskengine"
	"github.com/contenox/contenox/taskengine/llmretry"
)

func TestClassifyStepFailure_FromSink(t *testing.T) {
	cases := []struct {
		name      string
		sinkClass llmretry.ErrorClass
		execErr   error
		want      planstore.FailureClass
	}{
		{
			name:      "sink_capacity_wins",
			sinkClass: llmretry.ClassCapacity,
			execErr:   fmt.Errorf("chat failed: input token count 200000 exceeds context length 128000"),
			want:      planstore.FailureClassCapacity,
		},
		{
			name:      "sink_rate_limit_is_transient",
			sinkClass: llmretry.ClassRateLimit,
			execErr:   fmt.Errorf("chat failed: status: 429"),
			want:      planstore.FailureClassTransient,
		},
		{
			name:      "sink_server_error_is_transient",
			sinkClass: llmretry.ClassServerError,
			execErr:   fmt.Errorf("chat failed: status: 503"),
			want:      planstore.FailureClassTransient,
		},
		{
			name:      "sink_auth_falls_to_logic",
			sinkClass: llmretry.ClassAuth,
			execErr:   fmt.Errorf("chat failed: 401"),
			want:      planstore.FailureClassLogic,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &taskengine.RetryOutcomeSink{}
			sink.Append(llmretry.Outcome{LastErrorClass: tc.sinkClass})
			got := classifyStepFailure(tc.execErr, sink)
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyStepFailure_FallbackToErrText(t *testing.T) {
	// Empty sink (e.g. failure happened before any chat call landed) → classify
	// from the error text.
	sink := &taskengine.RetryOutcomeSink{}
	got := classifyStepFailure(fmt.Errorf("chat failed: input token count 999 exceeds context length 8"), sink)
	if got != planstore.FailureClassCapacity {
		t.Fatalf("got %q want capacity", got)
	}
	got = classifyStepFailure(fmt.Errorf("hook tool foo failed: not found"), sink)
	if got != planstore.FailureClassLogic {
		t.Fatalf("got %q want logic", got)
	}
}

func TestClassifyStepFailure_NilSinkOK(t *testing.T) {
	got := classifyStepFailure(fmt.Errorf("chat failed: status: 429"), nil)
	if got != planstore.FailureClassTransient {
		t.Fatalf("got %q want transient", got)
	}
}

// Smoke check: the helper is import-stable; confirm context compiles in.
func TestClassifyStepFailure_ContextCompiles(t *testing.T) {
	_ = context.Background()
}
