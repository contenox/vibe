package localtools_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/stretchr/testify/require"
)

// ceilingPolicy is a policy that also answers what the host's approval ceiling
// is, the way hitlservice does.
type ceilingPolicy struct {
	*recordingApprovePolicy
	ceiling time.Duration
}

func (p ceilingPolicy) ApprovalCeiling() time.Duration { return p.ceiling }

// TestUnit_HITLWrapper_CardOutlivesNothingItShould pins the card's lifetime
// against the row's: a rule wait bounds it, an unset wait falls to the host's
// ceiling, and a wait with no deadline gets no context deadline at all — the
// card stands until it is answered, which is what makes "close the laptop"
// true on the client side too.
func TestUnit_HITLWrapper_CardOutlivesNothingItShould(t *testing.T) {
	for _, tc := range []struct {
		name     string
		timeoutS int
		ceiling  time.Duration
		want     time.Duration // zero means: the card must carry no deadline
	}{
		{name: "the rule's own wait", timeoutS: 30, ceiling: time.Minute, want: 30 * time.Second},
		{name: "unset falls to the host ceiling", ceiling: 2 * time.Hour, want: 2 * time.Hour},
		{name: "the rule says wait forever", timeoutS: hitlservice.TimeoutIndefinite, ceiling: time.Minute},
		{name: "the host ceiling says wait forever", ceiling: hitlservice.WaitIndefinite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := &mockInnerTools{}
			policy := ceilingPolicy{recordingApprovePolicy: newRecordingApprovePolicy(), ceiling: tc.ceiling}
			policy.result = hitlservice.EvaluationResult{Action: hitlservice.ActionApprove, TimeoutS: tc.timeoutS}

			type window struct {
				deadline time.Time
				ok       bool
			}
			seen := make(chan window, 1)
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			ask := func(ctx context.Context, _ hitlservice.ApprovalRequest) (bool, error) {
				deadline, ok := ctx.Deadline()
				seen <- window{deadline, ok}
				select {
				case <-ctx.Done():
					return false, ctx.Err()
				case <-release:
					return false, nil
				}
			}

			raised := time.Now()
			_, _, err := execSuspendableCall(t, localtools.NewHITLWrapper(inner, ask, policy, nil), "call-wait")
			require.Error(t, err, "a gated call parks; the card is raised beside it")

			var got window
			select {
			case got = <-seen:
			case <-time.After(5 * time.Second):
				t.Fatal("the card was never raised")
			}

			if tc.want == 0 {
				require.False(t, got.ok, "an ask with no deadline must not be handed one")
				return
			}
			require.True(t, got.ok)
			require.WithinDuration(t, raised.Add(tc.want), got.deadline, 5*time.Second)
		})
	}
}
