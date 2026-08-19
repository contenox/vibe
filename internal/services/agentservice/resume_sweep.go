package agentservice

import (
	"context"
	"errors"
	"time"

	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// StrandedCheckpoints reports the checkpoints an answered ask left with no live resumer: never claimed, or claimed longer ago than the resume staleness bound.
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

// SweepStrandedCheckpoints resumes every stranded checkpoint (see StrandedCheckpoints) in this process; resumed counts runs reaching a terminal or re-suspended state, failed counts errored resumes, and a racing resumer's claim is a clean skip, not a failure.
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
		case errors.Is(rerr, ErrNoCheckpoint), errors.Is(rerr, ErrApprovalUnanswered), errors.Is(rerr, ErrMissionFinished):
			// Claimed or completed by a racer, the ask changed under us, or the
			// mission already finished and the dead checkpoint was discarded.
		default:
			failed++
		}
	}
	return resumed, failed, nil
}
