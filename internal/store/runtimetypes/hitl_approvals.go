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

// HITLApprovalState is the lifecycle state of one durable human-in-the-loop approval ask; pending is the only non-terminal state.
type HITLApprovalState string

const (
	HITLApprovalPending  HITLApprovalState = "pending"
	HITLApprovalApproved HITLApprovalState = "approved"
	HITLApprovalDenied   HITLApprovalState = "denied"
	HITLApprovalExpired  HITLApprovalState = "expired"
)

// noDeadline is the expires_at of an ask that waits until it is answered: the sweeper skips it and every reader renders it as no deadline.
var noDeadline time.Time

// HITLApproval is a durable row for one runtime/hitlservice approval ask, resolved by exactly one of Respond or the expiry sweeper.
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
	// InstanceID, SessionID, AgentName and MissionID are the attribution set: which fleet unit, session, agent, and mission raised the ask.
	InstanceID string    `json:"instanceId,omitempty" example:"7c1f9e4a-2b3d-4c5e-8f90-a1b2c3d4e5f6"`
	SessionID  string    `json:"sessionId,omitempty" example:"sess_01H8XGJWBWBAQ4Z8"`
	AgentName  string    `json:"agentName,omitempty" example:"reviewer"`
	MissionID  *string   `json:"missionId,omitempty" example:"9d2e7f10-4c8b-4a1e-b3d6-0f5a7c9e1b24"`
	CreatedAt  time.Time `json:"createdAt" example:"2024-01-15T10:00:00Z"`
	// ExpiresAt is the moment the sweeper resolves the ask by OnTimeout; the zero time is an ask with no deadline, which stays pending until it is answered.
	ExpiresAt  time.Time  `json:"expiresAt" example:"2024-01-15T11:00:00Z"`
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
}

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
	// []byte round-trips on both Postgres and SQLite; scanning directly into json.RawMessage fails on SQLite.
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

// AgentAnswerBound is the count predicate an agent-attributed resolution is written under, supplied entirely by the caller.
type AgentAnswerBound struct {
	// MissionID scopes the count to one mission's asks.
	MissionID string
	// ToolsName and ToolName narrow the count to one tool identity; empty matches every row, which is how a permission-ask bound spans the varying tools a unit gates on.
	ToolsName string
	ToolName  string
	// ResolutionLike is the SQL LIKE pattern a resolution written by an agent
	// matches and a human's does not.
	ResolutionLike string
	// Max is the exclusive ceiling: the write lands only while strictly fewer
	// than Max matching rows already exist.
	Max int
}

func (s *store) ResolveHITLApprovalWithinBound(ctx context.Context, id string, bound AgentAnswerBound, state HITLApprovalState, resolution json.RawMessage, resolvedAt time.Time) error {
	result, err := s.Exec.ExecContext(ctx, `
		UPDATE hitl_approvals
		SET state = $2, resolution = $3, resolved_at = $4
		WHERE id = $1 AND state = 'pending'
		  AND (SELECT COUNT(*) FROM hitl_approvals prior
		       WHERE prior.mission_id = $5
		         AND ($6 = '' OR prior.tools_name = $6)
		         AND ($7 = '' OR prior.tool_name = $7)
		         AND prior.resolution LIKE $8) < $9`,
		id, string(state), nullableJSON(resolution), resolvedAt,
		bound.MissionID, bound.ToolsName, bound.ToolName, bound.ResolutionLike, bound.Max,
	)
	if err != nil {
		return err
	}
	return checkRowsAffected(result)
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

func (s *store) ListExpiredHITLApprovals(ctx context.Context, asOf time.Time, limit int) ([]*HITLApproval, error) {
	if limit <= 0 || limit > MAXLIMIT {
		limit = MAXLIMIT
	}
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT `+hitlApprovalColumns+`
		FROM hitl_approvals
		WHERE state = 'pending' AND expires_at > $1 AND expires_at <= $2
		ORDER BY expires_at ASC
		LIMIT $3`, noDeadline, asOf, limit)
	if err != nil {
		return nil, fmt.Errorf("hitl_approvals: list expired query: %w", err)
	}
	defer rows.Close()
	return scanHITLApprovalRows(rows)
}

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

func (s *store) ListPendingHITLApprovalsForSession(ctx context.Context, sessionID string, limit int) ([]*HITLApproval, error) {
	if limit > MAXLIMIT {
		return nil, ErrLimitParamExceeded
	}
	if limit <= 0 {
		limit = MAXLIMIT
	}
	if sessionID == "" {
		return []*HITLApproval{}, nil
	}
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT `+hitlApprovalColumns+`
		FROM hitl_approvals
		WHERE session_id = $1 AND state = 'pending'
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("hitl_approvals: list pending for session query: %w", err)
	}
	defer rows.Close()
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
