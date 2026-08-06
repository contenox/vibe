package agentservice

// resume_sweep.go drives the reclaim half of the resume machinery. A verdict
// is recorded exactly once, but the process that records it can die mid-resume
// (a stale claim), fail after the gated call (a retained, annotated
// checkpoint), or predate the ordering gate entirely (an answered, never
// claimed row). This sweep finds those and runs them to completion in the
// current process — hitlservice.SweepExpired's analogue one layer up, driven
// from the same operator seams.

import (
	"context"
	"errors"
	"time"

	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// StrandedCheckpoints reports the checkpoints an answered ask left with no
// live resumer: never claimed, or claimed longer ago than the resume
// staleness bound. A pending ask's checkpoint is the normal suspended state,
// not a strand, and is never listed. Read-only; callable without an engine so
// hosts can decide whether building one is worth it.
func StrandedCheckpoints(ctx context.Context, store runtimetypes.Store, limit int) ([]string, error) {
	cps, err := store.ListChainCheckpoints(ctx, nil, limit)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	ids := []string{}
	for _, cp := range cps {
		if cp.ClaimedAt != nil && now.Sub(*cp.ClaimedAt) < resumeClaimStaleness {
			continue // a live resumer holds the claim
		}
		row, err := store.GetHITLApproval(ctx, cp.ID)
		if err != nil || row.State == runtimetypes.HITLApprovalPending {
			continue // unanswered (suspended, not stranded), or unreadable right now
		}
		ids = append(ids, cp.ID)
	}
	return ids, nil
}

// SweepStrandedCheckpoints resumes every stranded checkpoint (see
// StrandedCheckpoints) in this process, re-deriving the stranded set so a
// state change between a caller's own check and this call is honored. resumed
// counts runs carried to a terminal or cleanly re-suspended state; failed
// counts resumes that errored — their checkpoints stay annotated and
// reclaimable once the claim goes stale again. A racing resumer's claim
// (ErrNoCheckpoint) is a clean skip, not a failure.
func SweepStrandedCheckpoints(ctx context.Context, deps Deps, limit int) (resumed, failed int, err error) {
	store := runtimetypes.New(deps.DB.WithoutTransaction())
	ids, err := StrandedCheckpoints(ctx, store, limit)
	if err != nil {
		return 0, 0, err
	}
	for _, id := range ids {
		_, rerr := ResumeFromCheckpoint(ctx, deps, id)
		switch {
		case rerr == nil:
			resumed++
		case errors.Is(rerr, ErrNoCheckpoint), errors.Is(rerr, ErrApprovalUnanswered):
			// Claimed or completed by a racer, or the ask changed under us.
		default:
			failed++
		}
	}
	return resumed, failed, nil
}
