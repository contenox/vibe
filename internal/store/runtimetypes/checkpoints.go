package runtimetypes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
)

// ChainCheckpoint is one suspended chain run, keyed by the approval ID whose
// verdict resumes it (table chain_checkpoints). Payload is the engine's
// versioned JSON envelope (taskengine.Checkpoint) and is opaque here;
// runtimetypes only stores, claims, deletes, and annotates it. SchemaVersion
// is mirrored out as a queryable column so stranded old-version checkpoints
// are spottable without parsing payloads.
//
// Lifecycle: created when a run suspends; claimed by exactly one resumer via
// compare-and-swap on claimed_at, so a racing hook-triggered and inline
// post-suspend resume cannot both run the chain; deleted on success, or
// retained with a failure annotation so a run is never silently lost. A claim
// older than the staleness bound passed to ClaimChainCheckpoint is reclaimable.
type ChainCheckpoint struct {
	ID            string          `json:"id"`
	SchemaVersion int             `json:"schemaVersion"`
	Payload       json.RawMessage `json:"payload"`
	SessionID     string          `json:"sessionId,omitempty"`
	RequestID     string          `json:"requestId,omitempty"`
	Failure       *string         `json:"failure,omitempty"`
	ClaimedAt     *time.Time      `json:"claimedAt,omitempty"`
	CreatedAt     time.Time       `json:"createdAt"`
	UpdatedAt     time.Time       `json:"updatedAt"`
}

// chainCheckpointColumns is the single projection every read binds, in scan
// order — spelled once so a new column cannot be added to one query and
// forgotten in another (same idiom as hitlApprovalColumns).
const chainCheckpointColumns = `id, schema_version, payload, session_id, request_id, failure, claimed_at, created_at, updated_at`

func (s *store) CreateChainCheckpoint(ctx context.Context, cp *ChainCheckpoint) error {
	now := time.Now().UTC()
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	cp.UpdatedAt = now
	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO chain_checkpoints
		(`+chainCheckpointColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		cp.ID, cp.SchemaVersion, nullableJSON(cp.Payload), cp.SessionID, cp.RequestID, cp.Failure, cp.ClaimedAt, cp.CreatedAt, cp.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("chain_checkpoints: create %s: %w", cp.ID, err)
	}
	return nil
}

func (s *store) GetChainCheckpoint(ctx context.Context, id string) (*ChainCheckpoint, error) {
	var cp ChainCheckpoint
	var rawPayload []byte
	err := s.Exec.QueryRowContext(ctx, `
		SELECT `+chainCheckpointColumns+`
		FROM chain_checkpoints WHERE id = $1`, id).Scan(
		&cp.ID, &cp.SchemaVersion, &rawPayload, &cp.SessionID, &cp.RequestID, &cp.Failure, &cp.ClaimedAt, &cp.CreatedAt, &cp.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, libdb.ErrNotFound
		}
		return nil, fmt.Errorf("chain_checkpoints: get %s: %w", id, err)
	}
	// Scan via []byte, not json.RawMessage, for SQLite TEXT compatibility
	// (see kv.go's getKVScoped).
	if rawPayload != nil {
		cp.Payload = json.RawMessage(rawPayload)
	}
	return &cp, nil
}

// ClaimChainCheckpoint marks id as being resumed by this caller: an atomic
// compare-and-swap that succeeds only when the checkpoint is unclaimed OR its
// claim is older than staleBefore (a resumer that died mid-run relinquishes
// by staleness, never by cooperation). Returns libdb.ErrNotFound when the id
// does not exist or another live resumer holds the claim — the two cases are
// deliberately indistinct, since the caller's move is the same: do nothing.
func (s *store) ClaimChainCheckpoint(ctx context.Context, id string, now, staleBefore time.Time) error {
	result, err := s.Exec.ExecContext(ctx, `
		UPDATE chain_checkpoints
		SET claimed_at = $2, updated_at = $2
		WHERE id = $1 AND (claimed_at IS NULL OR claimed_at < $3)`,
		id, now, staleBefore,
	)
	if err != nil {
		return fmt.Errorf("chain_checkpoints: claim %s: %w", id, err)
	}
	return checkRowsAffected(result)
}

// TouchChainCheckpointClaim refreshes id's claim timestamp — the live
// resumer's heartbeat. Wall-clock staleness (see ClaimChainCheckpoint) must
// only ever reclaim a dead resumer, never a slow one still executing; the
// periodic touch is what makes that distinction real. Only refreshes an
// existing claim, never creates one.
func (s *store) TouchChainCheckpointClaim(ctx context.Context, id string, now time.Time) error {
	result, err := s.Exec.ExecContext(ctx, `
		UPDATE chain_checkpoints
		SET claimed_at = $2, updated_at = $2
		WHERE id = $1 AND claimed_at IS NOT NULL`,
		id, now,
	)
	if err != nil {
		return fmt.Errorf("chain_checkpoints: touch claim %s: %w", id, err)
	}
	return checkRowsAffected(result)
}

// UpdateChainCheckpointPayload replaces id's payload — the claiming resumer's
// own bookkeeping write (recording a completed gate call's result inside the
// agentservice envelope). The claim CAS makes the live resumer the row's only
// writer, so no compare is needed here; the payload stays opaque to this layer.
func (s *store) UpdateChainCheckpointPayload(ctx context.Context, id string, payload json.RawMessage) error {
	now := time.Now().UTC()
	result, err := s.Exec.ExecContext(ctx, `
		UPDATE chain_checkpoints
		SET payload = $2, updated_at = $3
		WHERE id = $1`,
		id, nullableJSON(payload), now,
	)
	if err != nil {
		return fmt.Errorf("chain_checkpoints: update payload %s: %w", id, err)
	}
	return checkRowsAffected(result)
}

// SetChainCheckpointFailure annotates id with why its resume failed, keeping
// the row: a run whose resume errored must stay findable (and re-claimable
// once the claim goes stale) rather than vanishing.
func (s *store) SetChainCheckpointFailure(ctx context.Context, id string, failure string) error {
	now := time.Now().UTC()
	result, err := s.Exec.ExecContext(ctx, `
		UPDATE chain_checkpoints
		SET failure = $2, updated_at = $3
		WHERE id = $1`,
		id, failure, now,
	)
	if err != nil {
		return fmt.Errorf("chain_checkpoints: annotate %s: %w", id, err)
	}
	return checkRowsAffected(result)
}

// DeleteChainCheckpoint removes id — the successful-terminal path. Deleting a
// row that is already gone returns libdb.ErrNotFound (checkRowsAffected), so
// double-deletes are visible to callers that care and ignorable by those that
// do not.
func (s *store) DeleteChainCheckpoint(ctx context.Context, id string) error {
	result, err := s.Exec.ExecContext(ctx, `
		DELETE FROM chain_checkpoints WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("chain_checkpoints: delete %s: %w", id, err)
	}
	return checkRowsAffected(result)
}

// ListChainCheckpoints returns suspended runs newest first — the substrate
// `approvals list` can join against, and the operator's view of
// stranded runs (non-nil Failure) awaiting a retry.
func (s *store) ListChainCheckpoints(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*ChainCheckpoint, error) {
	cursor := time.Now().UTC()
	if createdAtCursor != nil {
		cursor = *createdAtCursor
	}
	if limit > MAXLIMIT {
		return nil, ErrLimitParamExceeded
	}
	if limit <= 0 {
		limit = MAXLIMIT
	}
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT `+chainCheckpointColumns+`
		FROM chain_checkpoints
		WHERE created_at < $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("chain_checkpoints: list: %w", err)
	}
	defer rows.Close()

	out := []*ChainCheckpoint{}
	for rows.Next() {
		var cp ChainCheckpoint
		var rawPayload []byte
		if err := rows.Scan(
			&cp.ID, &cp.SchemaVersion, &rawPayload, &cp.SessionID, &cp.RequestID, &cp.Failure, &cp.ClaimedAt, &cp.CreatedAt, &cp.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("chain_checkpoints: scan row: %w", err)
		}
		if rawPayload != nil {
			cp.Payload = json.RawMessage(rawPayload)
		}
		out = append(out, &cp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chain_checkpoints: rows error: %w", err)
	}
	return out, nil
}
