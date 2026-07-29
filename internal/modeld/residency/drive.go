package residency

import (
	"context"
	"sort"
)

// EvictColdRanges returns the ranges of a plan's EvictCold blocks ordered
// tail-first (highest Start first). A caller that evicts them in this order does
// not shift the indices of ranges it has not evicted yet: removing a higher
// range never moves the tokens below it. Empty or degenerate ranges are dropped.
//
// Backend adapters driving eviction from inside an already-locked prefill path
// iterate this over their own *Locked eviction primitive; callers that do not
// hold the session lock use Drive.
func EvictColdRanges(plan Plan) []Range {
	if len(plan.EvictCold) == 0 {
		return nil
	}
	out := make([]Range, 0, len(plan.EvictCold))
	for _, b := range plan.EvictCold {
		if b.Range.End > b.Range.Start {
			out = append(out, b.Range)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start > out[j].Start })
	return out
}

// Drive executes the cold-eviction half of a residency plan against a
// self-locking Executor: it evicts each EvictCold range to the cold store,
// freeing hot KV. It is a no-op when the plan evicts nothing. Use it only from
// callers that do NOT already hold the session lock, since Executor methods lock
// internally.
func Drive(ctx context.Context, exec Executor, plan Plan) error {
	for _, r := range EvictColdRanges(plan) {
		if err := exec.EvictRange(ctx, r); err != nil {
			return err
		}
	}
	return nil
}
