package runtimetypes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/internal/libdbexec"
)

// HITLApprovalState is the lifecycle state of one durable human-in-the-loop
// approval ask (table hitl_approvals in schema.sql/schema_sqlite.sql).
// pending is the only non-terminal state; a row ends exactly once, at
// approved/denied (a human's Respond) or expired (the sweeper, once
// expires_at passes with nobody having answered).
type HITLApprovalState string

const (
	HITLApprovalPending  HITLApprovalState = "pending"
	HITLApprovalApproved HITLApprovalState = "approved"
	HITLApprovalDenied   HITLApprovalState = "denied"
	HITLApprovalExpired  HITLApprovalState = "expired"
)

// HITLApproval is a durable row for one runtime/hitlservice approval ask. It
// is written before the ask is published (see hitlservice.RequestApproval)
// so a `contenox serve` restart mid-ask still finds it pending rather than
// losing it, and is resolved by exactly one of Respond (approved/denied) or
// the expiry sweeper (expired, applying OnTimeout).
//
// OnTimeout is stored as a plain string (not hitlservice.Action) so this
// package does not import hitlservice — runtimetypes sits below the service
// layer; hitlservice converts to/from its own Action type at the boundary.
//
// Resolution is deliberately opaque JSON, not a bare boolean: a permission
// ask is yes/no, but a later mission-mode ask may answer with data ("which of
// these three?"). runtimetypes does not interpret its shape — that is
// hitlservice's concern; this column is nil while State is pending and set
// exactly once when it becomes terminal.
//
// InstanceID, SessionID, AgentName and MissionID name who is asking, needed
// to tell otherwise-identical rows apart once more than one fleet unit is
// asking. All four are best-effort — a native chain turn with no fleet unit
// carries none of them — and MissionID is a pointer because not every ask
// has one. Empty/nil means "not applicable", never "unknown but exists".
type HITLApproval struct {
	ID          string            `json:"id" example:"3f9c6e2a-1b4d-4e8f-9a2c-7d5e6f8a9b0c"`
	ToolsName   string            `json:"toolsName" example:"local_fs"`
	ToolName    string            `json:"toolName" example:"write_file"`
	ArgsSummary string            `json:"argsSummary,omitempty" example:"/workspace/main.go"`
	Diff        *string           `json:"diff,omitempty"`
	PolicyName  string            `json:"policyName,omitempty" example:"hitl-policy-default.json"`
	MatchedRule *int              `json:"matchedRule,omitempty"`
	OnTimeout   string            `json:"onTimeout,omitempty" example:"deny"`
	State       HITLApprovalState `json:"state" example:"pending"`
	Resolution  json.RawMessage   `json:"resolution,omitempty" example:"{\"approved\":true}"`
	// InstanceID, SessionID, AgentName and MissionID are the attribution set —
	// see the type doc. InstanceID/SessionID are the fleet unit and the
	// downstream session the ask was raised on; AgentName is the declared agent
	// that unit runs; MissionID is the mission whose envelope escalated it.
	InstanceID string     `json:"instanceId,omitempty" example:"7c1f9e4a-2b3d-4c5e-8f90-a1b2c3d4e5f6"`
	SessionID  string     `json:"sessionId,omitempty" example:"sess_01H8XGJWBWBAQ4Z8"`
	AgentName  string     `json:"agentName,omitempty" example:"reviewer"`
	MissionID  *string    `json:"missionId,omitempty" example:"9d2e7f10-4c8b-4a1e-b3d6-0f5a7c9e1b24"`
	CreatedAt  time.Time  `json:"createdAt" example:"2024-01-15T10:00:00Z"`
	ExpiresAt  time.Time  `json:"expiresAt" example:"2024-01-15T11:00:00Z"`
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
}

// hitlApprovalColumns is the column list every HITLApproval read projects, in
// the exact order scanHITLApproval / scanHITLApprovalRows bind. It is spelled
// once so a column added to the table cannot be added to one query and
// forgotten in another — the drift a hand-copied SELECT list invites.
const hitlApprovalColumns = `id, tools_name, tool_name, args_summary, diff, policy_name, matched_rule, on_timeout, state, resolution, ` +
	`instance_id, session_id, agent_name, mission_id, created_at, expires_at, resolved_at`

func (s *store) CreateHITLApproval(ctx context.Context, a *HITLApproval) error {
	if a.State == "" {
		a.State = HITLApprovalPending
	}
	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO hitl_approvals
		(`+hitlApprovalColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		a.ID, a.ToolsName, a.ToolName, a.ArgsSummary, a.Diff, a.PolicyName, a.MatchedRule, a.OnTimeout, string(a.State), nullableJSON(a.Resolution),
		a.InstanceID, a.SessionID, a.AgentName, a.MissionID, a.CreatedAt, a.ExpiresAt, a.ResolvedAt,
	)
	return err
}

func (s *store) GetHITLApproval(ctx context.Context, id string) (*HITLApproval, error) {
	return s.scanHITLApproval(ctx, `
		SELECT `+hitlApprovalColumns+`
		FROM hitl_approvals WHERE id = $1`, id)
}

func (s *store) scanHITLApproval(ctx context.Context, query string, arg any) (*HITLApproval, error) {
	var a HITLApproval
	var state string
	// []byte round-trips on both Postgres (JSONB) and SQLite (TEXT); scanning
	// directly into json.RawMessage fails on SQLite (see kv.go's getKVScoped).
	var rawResolution []byte
	err := s.Exec.QueryRowContext(ctx, query, arg).Scan(
		&a.ID, &a.ToolsName, &a.ToolName, &a.ArgsSummary, &a.Diff, &a.PolicyName, &a.MatchedRule, &a.OnTimeout, &state, &rawResolution,
		&a.InstanceID, &a.SessionID, &a.AgentName, &a.MissionID, &a.CreatedAt, &a.ExpiresAt, &a.ResolvedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, libdb.ErrNotFound
		}
		return nil, err
	}
	a.State = HITLApprovalState(state)
	if rawResolution != nil {
		a.Resolution = json.RawMessage(rawResolution)
	}
	return &a, nil
}

// ResolveHITLApproval atomically transitions id from pending to state via an
// UPDATE ... WHERE state = 'pending' compare-and-swap, so a human's Respond
// racing the sweeper's timeout-expiry can never both win. Returns
// libdb.ErrNotFound when id does not exist or is no longer pending; callers
// that need to tell those apart follow up with GetHITLApproval.
func (s *store) ResolveHITLApproval(ctx context.Context, id string, state HITLApprovalState, resolution json.RawMessage, resolvedAt time.Time) error {
	result, err := s.Exec.ExecContext(ctx, `
		UPDATE hitl_approvals
		SET state = $2, resolution = $3, resolved_at = $4
		WHERE id = $1 AND state = 'pending'`,
		id, string(state), nullableJSON(resolution), resolvedAt,
	)
	if err != nil {
		return err
	}
	return checkRowsAffected(result)
}

// AgentAnswerBound is the count predicate an agent-attributed resolution is
// written under (see ResolveHITLApprovalWithinBound). Every field is supplied
// by the caller: runtimetypes sits below hitlservice and so cannot name that
// package's attention marks or its resolution payload shape.
type AgentAnswerBound struct {
	// MissionID scopes the count to one mission's asks.
	MissionID string
	// ToolsName and ToolName are the pair that marks a row as an attention ask
	// rather than a permission one.
	ToolsName string
	ToolName  string
	// ResolutionLike is the SQL LIKE pattern a resolution written by an agent
	// matches and a human's does not.
	ResolutionLike string
	// Max is the exclusive ceiling: the write lands only while strictly fewer
	// than Max matching rows already exist.
	Max int
}

// ResolveHITLApprovalWithinBound is ResolveHITLApproval's pending-state CAS
// with bound's count predicate carried in the same WHERE clause, so the
// database — not the caller — decides which of several concurrent
// agent-attributed answers fit under a mission's cap. A caller that counts and
// then writes cannot: every racer reads the same count before any of them
// writes, and they all pass. Returns libdb.ErrNotFound when id does not exist,
// is no longer pending, or the bound is already spent; callers tell those
// apart by re-reading with GetHITLApproval.
func (s *store) ResolveHITLApprovalWithinBound(ctx context.Context, id string, bound AgentAnswerBound, state HITLApprovalState, resolution json.RawMessage, resolvedAt time.Time) error {
	result, err := s.Exec.ExecContext(ctx, `
		UPDATE hitl_approvals
		SET state = $2, resolution = $3, resolved_at = $4
		WHERE id = $1 AND state = 'pending'
		  AND (SELECT COUNT(*) FROM hitl_approvals prior
		       WHERE prior.mission_id = $5
		         AND prior.tools_name = $6
		         AND prior.tool_name = $7
		         AND prior.resolution LIKE $8) < $9`,
		id, string(state), nullableJSON(resolution), resolvedAt,
		bound.MissionID, bound.ToolsName, bound.ToolName, bound.ResolutionLike, bound.Max,
	)
	if err != nil {
		return err
	}
	return checkRowsAffected(result)
}

// nullableJSON returns raw as a string for binding, or untyped nil (SQL
// NULL) when raw is empty — mirrors how *string/*int fields elsewhere in
// this package (e.g. Agent.HarnessID) bind nil as NULL, since json.RawMessage
// has no pointer-nilability of its own to rely on.
func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// ListExpiredHITLApprovals returns pending approvals whose deadline has
// passed as of asOf, oldest deadline first — the batch a sweeper resolves.
func (s *store) ListExpiredHITLApprovals(ctx context.Context, asOf time.Time, limit int) ([]*HITLApproval, error) {
	if limit <= 0 || limit > MAXLIMIT {
		limit = MAXLIMIT
	}
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT `+hitlApprovalColumns+`
		FROM hitl_approvals
		WHERE state = 'pending' AND expires_at <= $1
		ORDER BY expires_at ASC
		LIMIT $2`, asOf, limit)
	if err != nil {
		return nil, fmt.Errorf("hitl_approvals: list expired query: %w", err)
	}
	defer rows.Close()
	return scanHITLApprovalRows(rows)
}

// ListHITLApprovals returns approvals in state, newest first.
func (s *store) ListHITLApprovals(ctx context.Context, state HITLApprovalState, createdAtCursor *time.Time, limit int) ([]*HITLApproval, error) {
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
		SELECT `+hitlApprovalColumns+`
		FROM hitl_approvals
		WHERE state = $1 AND created_at < $2
		ORDER BY created_at DESC, id DESC
		LIMIT $3`, string(state), cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("hitl_approvals: list query: %w", err)
	}
	defer rows.Close()
	return scanHITLApprovalRows(rows)
}

// ListHITLApprovalsForMission returns every ask raised by missionID's unit,
// newest first, in any state — deliberately no state filter, since a mission
// raises few asks and the caller filters in Go rather than pushing a
// resolution-shape query into SQL written twice for the two dialects supported.
func (s *store) ListHITLApprovalsForMission(ctx context.Context, missionID string, limit int) ([]*HITLApproval, error) {
	if limit > MAXLIMIT {
		return nil, ErrLimitParamExceeded
	}
	if limit <= 0 {
		limit = MAXLIMIT
	}
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT `+hitlApprovalColumns+`
		FROM hitl_approvals
		WHERE mission_id = $1
		ORDER BY created_at DESC
		LIMIT $2`, missionID, limit)
	if err != nil {
		return nil, err
	}
	return scanHITLApprovalRows(rows)
}

func scanHITLApprovalRows(rows *sql.Rows) ([]*HITLApproval, error) {
	out := []*HITLApproval{}
	for rows.Next() {
		var a HITLApproval
		var state string
		var rawResolution []byte
		if err := rows.Scan(
			&a.ID, &a.ToolsName, &a.ToolName, &a.ArgsSummary, &a.Diff, &a.PolicyName, &a.MatchedRule, &a.OnTimeout, &state, &rawResolution,
			&a.InstanceID, &a.SessionID, &a.AgentName, &a.MissionID, &a.CreatedAt, &a.ExpiresAt, &a.ResolvedAt,
		); err != nil {
			return nil, fmt.Errorf("hitl_approvals: scan row: %w", err)
		}
		a.State = HITLApprovalState(state)
		if rawResolution != nil {
			a.Resolution = json.RawMessage(rawResolution)
		}
		out = append(out, &a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hitl_approvals: rows error: %w", err)
	}
	return out, nil
}

func (s *store) EstimateHITLApprovalCount(ctx context.Context) (int64, error) {
	return s.estimateCount(ctx, "hitl_approvals")
}
