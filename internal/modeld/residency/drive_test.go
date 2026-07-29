package residency

import (
	"context"
	"errors"
	"testing"
)

type driveFakeExecutor struct {
	caps     Capabilities
	evicted  []Range
	evictErr error
}

func (f *driveFakeExecutor) Capabilities() Capabilities                  { return f.caps }
func (f *driveFakeExecutor) AdmitRange(_ context.Context, _ Range) error { return nil }
func (f *driveFakeExecutor) EvictRange(_ context.Context, r Range) error {
	if f.evictErr != nil {
		return f.evictErr
	}
	f.evicted = append(f.evicted, r)
	return nil
}

func TestUnit_EvictColdRanges_TailFirstAndFiltersDegenerate(t *testing.T) {
	plan := Plan{EvictCold: []Block{
		{Range: Range{Start: 4, End: 8}},
		{Range: Range{Start: 16, End: 20}},
		{Range: Range{Start: 10, End: 10}}, // degenerate, dropped
		{Range: Range{Start: 0, End: 4}},
	}}
	got := EvictColdRanges(plan)
	want := []Range{{Start: 16, End: 20}, {Start: 4, End: 8}, {Start: 0, End: 4}}
	if len(got) != len(want) {
		t.Fatalf("got %d ranges, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("range[%d] = %+v, want %+v (tail-first order)", i, got[i], want[i])
		}
	}
}

func TestUnit_Drive_EvictsAllColdRangesTailFirst(t *testing.T) {
	exec := &driveFakeExecutor{}
	plan := Plan{EvictCold: []Block{
		{Range: Range{Start: 0, End: 4}},
		{Range: Range{Start: 8, End: 12}},
	}}
	if err := Drive(context.Background(), exec, plan); err != nil {
		t.Fatalf("Drive: %v", err)
	}
	want := []Range{{Start: 8, End: 12}, {Start: 0, End: 4}}
	if len(exec.evicted) != len(want) {
		t.Fatalf("evicted %+v, want %+v", exec.evicted, want)
	}
	for i := range want {
		if exec.evicted[i] != want[i] {
			t.Fatalf("evicted[%d] = %+v, want %+v", i, exec.evicted[i], want[i])
		}
	}
}

func TestUnit_Drive_NoColdIsNoop(t *testing.T) {
	exec := &driveFakeExecutor{}
	if err := Drive(context.Background(), exec, Plan{}); err != nil {
		t.Fatalf("Drive empty: %v", err)
	}
	if len(exec.evicted) != 0 {
		t.Fatalf("expected no evictions, got %+v", exec.evicted)
	}
}

func TestUnit_Drive_PropagatesEvictError(t *testing.T) {
	sentinel := errors.New("boom")
	exec := &driveFakeExecutor{evictErr: sentinel}
	err := Drive(context.Background(), exec, Plan{EvictCold: []Block{{Range: Range{Start: 0, End: 4}}}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Drive error = %v, want %v", err, sentinel)
	}
}
