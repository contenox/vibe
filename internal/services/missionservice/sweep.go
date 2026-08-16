package missionservice

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
)

const heartbeatCeiling = time.Hour

const staleHeartbeatMultiple = 6

// StaleHeartbeatAfter is the floor on how long an open mission may go without a heartbeat before SweepAbandoned reclaims it; the actual bound widens to the longest park still open on the mission (see parkBound).
const StaleHeartbeatAfter = staleHeartbeatMultiple * heartbeatCeiling

const missionSweepBatchLimit = 200

// AbandonedBySweepReason is the StatusReason lead a reclaimed mission carries, distinct from StopMission's "stopped by operator" so the two read differently on `mission list`/`mission show`.
const AbandonedBySweepReason = "reclaimed: host process gone"

const abandonedReportSummary = "Mission reclaimed: its host process is gone."

func (s *service) SweepAbandoned(ctx context.Context) (int, error) {
	now := time.Now().UTC()
	reclaimed := 0
	var cursor *time.Time
	for reclaimed < missionSweepBatchLimit {
		batch, next, err := s.listPage(ctx, cursor, scanPageSize)
		if err != nil {
			return reclaimed, fmt.Errorf("missionservice: list missions to sweep: %w", err)
		}
		for _, m := range batch {
			if !isStale(m, now) {
				continue
			}
			ok, err := s.reclaim(ctx, m.ID, now)
			if err != nil {
				return reclaimed, err
			}
			if ok {
				reclaimed++
			}
			if reclaimed >= missionSweepBatchLimit {
				break
			}
		}
		if len(batch) < scanPageSize || next == nil {
			break
		}
		// Same strictly-decreasing-cursor guard GetByInstance walks under: truncates rather than loops.
		if cursor != nil && !next.Before(*cursor) {
			break
		}
		cursor = next
	}
	return reclaimed, nil
}

func (s *service) reclaim(ctx context.Context, id string, now time.Time) (bool, error) {
	m, snapshot, err := s.getWithSnapshot(ctx, id)
	if err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return false, nil // deleted between the scan and here.
		}
		return false, fmt.Errorf("missionservice: read mission %s to reclaim: %w", id, err)
	}
	// Re-judged against the fresh read, not the scan's possibly-stale copy.
	if !isStale(m, now) {
		return false, nil
	}
	// isStale tested the floor; this is the mission's real bound, queried only for candidates.
	bound, err := s.parkBound(ctx, id)
	if err != nil {
		return false, err
	}
	silence := now.Sub(lastLiveness(m))
	if silence <= bound {
		return false, nil
	}
	old := m.Status
	m.Status = StatusAbandoned
	m.StatusReason = abandonedReason(silence)
	m.UpdatedAt = now
	if err := s.putIfUnchanged(ctx, m, snapshot); err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return false, nil // a live write landed first; it owns the outcome.
		}
		return false, fmt.Errorf("missionservice: reclaim mission %s: %w", id, err)
	}
	s.publishStatusChanged(ctx, m, old)
	// Only once the reclaim is durable, so a mission that finished normally under the race never carries a "reclaimed" blocker.
	s.fileAbandonedReport(ctx, m, silence, bound)
	return true, nil
}

func (s *service) fileAbandonedReport(ctx context.Context, m *Mission, silence, bound time.Duration) {
	report := &Report{
		Kind:    ReportKindBlocker,
		Summary: abandonedReportSummary,
		Detail: fmt.Sprintf(
			"No heartbeat for %s (a mission unit is a child of the process that fired it, so the unit died with its host), "+
				"longer than any park still open on it could explain. "+
				"The mission was reclaimed as %s, the bound being %s of silence. This collects the mission record only: "+
				"any run it checkpointed is untouched, and any ask it filed is left pending on its own expiry.",
			formatSilence(silence), StatusAbandoned, formatSilence(bound)),
	}
	if err := s.AddReport(ctx, m.ID, report); err != nil {
		reportErr, _, end := s.tracker.Start(ctx, "sweep", "abandoned_report", "missionId", m.ID)
		reportErr(fmt.Errorf("missionservice: file reclaim report failed; mission %s is abandoned either way: %w", m.ID, err))
		end()
	}
}

func abandonedReason(silence time.Duration) string {
	return fmt.Sprintf("%s, no heartbeat for %s", AbandonedBySweepReason, formatSilence(silence))
}

func formatSilence(d time.Duration) string {
	if d < time.Minute {
		return "less than a minute"
	}
	return d.Round(time.Minute).String()
}

const missionAskScanLimit = 200

func (s *service) parkBound(ctx context.Context, missionID string) (time.Duration, error) {
	asks, err := s.store().ListHITLApprovalsForMission(ctx, missionID, missionAskScanLimit)
	if err != nil {
		return 0, fmt.Errorf("missionservice: read mission %s asks to reclaim: %w", missionID, err)
	}
	bound := time.Duration(StaleHeartbeatAfter)
	for _, a := range asks {
		if a == nil || a.State != runtimetypes.HITLApprovalPending {
			continue
		}
		if window := a.ExpiresAt.Sub(a.CreatedAt); window > bound {
			bound = window
		}
	}
	return bound, nil
}

func isStale(m *Mission, now time.Time) bool {
	if m == nil || m.Status != StatusOpen {
		return false
	}
	return now.Sub(lastLiveness(m)) > StaleHeartbeatAfter
}

func lastLiveness(m *Mission) time.Time {
	last := m.CreatedAt
	if m.LastHeartbeat != nil && m.LastHeartbeat.After(last) {
		last = *m.LastHeartbeat
	}
	return last
}
