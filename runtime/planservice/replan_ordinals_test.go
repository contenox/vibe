package planservice

import (
	"testing"

	"github.com/contenox/contenox/runtime/planstore"
)

func Test_maxOrdinalAmongRetainedPlanSteps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []*planstore.PlanStep
		want int
	}{
		{
			name: "empty",
			in:   nil,
			want: 0,
		},
		{
			name: "only_pending",
			in: []*planstore.PlanStep{
				{Ordinal: 1, Status: planstore.StepStatusPending},
				{Ordinal: 2, Status: planstore.StepStatusPending},
			},
			want: 0,
		},
		{
			name: "failed_counts_so_new_steps_dont_collide",
			in: []*planstore.PlanStep{
				{Ordinal: 1, Status: planstore.StepStatusCompleted},
				{Ordinal: 2, Status: planstore.StepStatusCompleted},
				{Ordinal: 3, Status: planstore.StepStatusFailed},
				{Ordinal: 4, Status: planstore.StepStatusPending},
			},
			want: 3,
		},
		{
			name: "running_counts",
			in: []*planstore.PlanStep{
				{Ordinal: 1, Status: planstore.StepStatusCompleted},
				{Ordinal: 2, Status: planstore.StepStatusRunning},
			},
			want: 2,
		},
		{
			name: "skipped_and_completed",
			in: []*planstore.PlanStep{
				{Ordinal: 1, Status: planstore.StepStatusCompleted},
				{Ordinal: 2, Status: planstore.StepStatusSkipped},
			},
			want: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := maxOrdinalAmongRetainedPlanSteps(tc.in)
			if got != tc.want {
				t.Fatalf("maxOrdinalAmongRetainedPlanSteps() = %d, want %d", got, tc.want)
			}
		})
	}
}
