package missionservice

// sweep.go is the mission garbage collector. A mission unit is a child
// subprocess of the host that fired it, so when the host dies — closed laptop,
// kill, crash — the unit dies with it while the row stays open, holding a
// heartbeat that will never advance. Heartbeat writes liveness and never
// status; every Finish caller is a live in-process decision. SweepAbandoned is
// the only path that brings such an unreachable row to rest, and is the
// mission-side analogue of hitlservice's SweepExpired.

import (
	"context"
	"errors"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// heartbeatCeiling is the widest gap between two heartbeats a live mission can
// legitimately show. Liveness is stamped per drive-loop turn and per mission
// tool call, never on a timer, so the gap is bounded by the longest a single
// turn can block: one parked on an ask, which hitlservice bounds by its
// serve-level approval ceiling of one hour (hitlservice.DefaultApprovalCeiling
// — named in prose, not imported, since hitlservice imports this package).
const heartbeatCeiling = time.Hour

// staleHeartbeatMultiple is how many heartbeatCeilings of silence a mission is
// granted before it is presumed unreachable. Chosen with headroom rather than
// tightness: reaping live work is unrecoverable, while reaping late only
// delays a row an operator was already ignoring.
const staleHeartbeatMultiple = 6

// StaleHeartbeatAfter is the FLOOR on how long an open mission may go without
// a heartbeat before SweepAbandoned reclaims it: staleHeartbeatMultiple ×
// heartbeatCeiling, so a slow-but-alive host — one parked on an ask for the
// full serve-level ceiling, then slow to reach its next turn — is never reaped.
//
// It is the floor and not the whole bound because the serve-level ceiling is
// not the only thing that parks a unit: a policy rule may set its own
// timeout_s, which vetting caps at seven days, and nothing heartbeats while a
// unit waits — the last stamp is the end of the turn that raised the ask. A
// mission parked on a window wider than this is silent and alive, so the
// reclaim decision widens the bound to the longest park still open on that
// mission (see parkBound). A park inside this floor widens nothing.
const StaleHeartbeatAfter = staleHeartbeatMultiple * heartbeatCeiling

// missionSweepBatchLimit caps missions reclaimed per SweepAbandoned call so a
// large backlog can't block indefinitely; the next sweep picks up the rest.
const missionSweepBatchLimit = 200

// AbandonedBySweepReason is the StatusReason lead a reclaimed mission carries.
// StopMission writes "stopped by operator"; this is its garbage-collected
// counterpart, so a mission an operator killed and one the runtime reclaimed
// read differently on `mission list`/`mission show`.
const AbandonedBySweepReason = "reclaimed: host process gone"

// abandonedReportSummary is the blocker a reclaim leaves on the mission, so an
// operator reading the record — or the inbox the report routes to, its host
// being gone — finds why the mission ended rather than a bare status.
const abandonedReportSummary = "Mission reclaimed: its host process is gone."

// SweepAbandoned implements Service.SweepAbandoned: see its doc on the
// interface.
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
		// Same strictly-decreasing-cursor guard GetByInstance walks under: an
		// identical-timestamp storm truncates the scan rather than loops.
		if cursor != nil && !next.Before(*cursor) {
			break
		}
		cursor = next
	}
	return reclaimed, nil
}

// reclaim moves one stale mission to StatusAbandoned under the snapshot it was
// read from, then files the blocker explaining why. Reports whether the write
// landed: false means the row was no longer stale-and-open by the time it was
// re-read, its silence is still inside its own park bound, or a live write beat
// the CAS — all three are the sweep correctly leaving a mission alone, not an
// error.
func (s *service) reclaim(ctx context.Context, id string, now time.Time) (bool, error) {
	m, snapshot, err := s.getWithSnapshot(ctx, id)
	if err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return false, nil // deleted between the scan and here.
		}
		return false, fmt.Errorf("missionservice: read mission %s to reclaim: %w", id, err)
	}
	// Re-judged against the fresh read, not the scan's copy: the scan is a
	// page of possibly-stale snapshots, this is the decision.
	if !isStale(m, now) {
		return false, nil
	}
	// isStale tested the floor; this is the mission's real bound. Queried only
	// for candidates, so a mission that never went silent costs nothing.
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
	// Only once the reclaim is durable, so a mission that finished normally
	// under the race never carries a "reclaimed" blocker. Best-effort in the
	// same register as the publishes: the status is already correct.
	s.fileAbandonedReport(ctx, m, silence, bound)
	return true, nil
}

// fileAbandonedReport records the reclaim as a mission blocker — the durable
// half of the story, since a StatusChangedEvent is dropped when no supervising
// session is live (and by definition none is here), while a report routes on
// to the operator inbox.
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

// abandonedReason is the one line Finish records as StatusReason for a
// reclaim: the lead an operator recognizes, plus the silence that justified it.
func abandonedReason(silence time.Duration) string {
	return fmt.Sprintf("%s, no heartbeat for %s", AbandonedBySweepReason, formatSilence(silence))
}

// formatSilence renders a heartbeat gap at minute resolution — a reclaim is a
// judgement about hours, and second-precision would only read as false rigour.
func formatSilence(d time.Duration) string {
	if d < time.Minute {
		return "less than a minute"
	}
	return d.Round(time.Minute).String()
}

// missionAskScanLimit caps one mission's ask scan. A mission raises a handful
// of asks over its life; this bounds a pathological one rather than makes the
// common case fast.
const missionAskScanLimit = 200

// parkBound is how long missionID's silence may legitimately run:
// StaleHeartbeatAfter, widened to the window of the longest park still open on
// it. The window is the ask's own configured wait (ExpiresAt-CreatedAt), not
// its remaining time — a unit parks at the moment it stamps its last
// heartbeat, so the window and the silence it explains are measured from the
// same instant. Resolved asks park nobody and widen nothing, and neither does
// a park inside the floor: an ask bounded by hitlservice's serve-level ceiling
// is exactly the case StaleHeartbeatAfter was already sized for.
//
// A read failure is returned, not swallowed: reaping live work is
// unrecoverable, so the sweep aborts rather than guessing at the bound.
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

// isStale reports whether m is an open mission whose liveness has gone silent
// past StaleHeartbeatAfter. Candidacy only, since that constant is the floor
// rather than the whole bound: reclaim re-tests the silence against the
// mission's own parkBound before deciding. Only open missions are candidates:
// a terminal mission is already at rest, however long ago it stopped reporting.
func isStale(m *Mission, now time.Time) bool {
	if m == nil || m.Status != StatusOpen {
		return false
	}
	return now.Sub(lastLiveness(m)) > StaleHeartbeatAfter
}

// lastLiveness is the most recent moment a mission was known to be reachable.
// A mission that never stamped a heartbeat is measured from its creation, so a
// host that died before its unit's first turn is reclaimed too rather than
// staying open forever on a missing fact.
func lastLiveness(m *Mission) time.Time {
	last := m.CreatedAt
	if m.LastHeartbeat != nil && m.LastHeartbeat.After(last) {
		last = *m.LastHeartbeat
	}
	return last
}
