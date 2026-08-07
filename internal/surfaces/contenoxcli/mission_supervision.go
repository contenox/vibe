package contenoxcli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
)

// missionSupervision adapts missionservice and hitlservice so a session that
// fired missions can read its own units and answer what they ask. Wired only
// for the in-process ACP host (`contenox acp` / `contenox acpx`, via
// acpToolset in acp_toolset.go): a firing session's agent may need to check
// on or answer its own units. `contenox beam` gets its mission tools from
// engine.go's plain toolset instead, with no supervisor wired, so a beam
// session can fire missions but its agent gets no supervision tools for
// them — reports still arrive live via the report router, independent of
// this type.
type missionSupervision struct {
	missions missionservice.Service
	hitl     hitlservice.Service
	// db backs the ask lookup AnswerAsAgent's bounds check needs; nil leaves
	// mission_answer refusing every write rather than answering unbounded.
	db libdb.DBManager
	// tracker carries the operator-facing reason for a refusal — the ONLY
	// place it is stated, since the model gets the plain denial.
	tracker libtracker.ActivityTracker
}

var (
	_ missiontools.SupervisorStore   = missionSupervision{}
	_ missiontools.AttentionResolver = missionSupervision{}
)

// supervisorScanPage bounds the mission page scanned to find a session's own
// missions. Missions are per-session and few; a supervisor that fired more than
// this in one session has a different problem than a missing row in a list.
const supervisorScanPage = 200

// MissionsFiredBy returns the missions this session fired, newest first,
// filtering a bounded page rather than querying by parent (the store has no
// secondary index for it).
func (s missionSupervision) MissionsFiredBy(ctx context.Context, parentSessionID string, limit int) ([]*missionservice.Mission, error) {
	if parentSessionID == "" {
		return nil, nil
	}
	page, err := s.missions.List(ctx, nil, supervisorScanPage)
	if err != nil {
		return nil, err
	}
	out := make([]*missionservice.Mission, 0, 8)
	for _, m := range page {
		if m == nil || m.ParentSessionID != parentSessionID {
			continue
		}
		out = append(out, m)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s missionSupervision) ListReports(ctx context.Context, missionID string, limit int) ([]*missionservice.Report, error) {
	return s.missions.ListReports(ctx, missionID, limit)
}

// PendingAsks returns what a mission is currently waiting on its supervisor for.
func (s missionSupervision) PendingAsks(ctx context.Context, missionID string) ([]missiontools.PendingAsk, error) {
	rows, err := s.hitl.PendingAttentionAsks(ctx, missionID)
	if err != nil {
		return nil, err
	}
	out := make([]missiontools.PendingAsk, 0, len(rows))
	for _, row := range rows {
		ask := missiontools.PendingAsk{
			AskID:     row.ID,
			MissionID: missionID,
			Question:  row.ArgsSummary,
			AskedAt:   row.CreatedAt.UTC().Format(time.RFC3339),
		}
		if row.Diff != nil {
			ask.Detail = *row.Diff
		}
		out = append(out, ask)
	}
	return out, nil
}

// AnswerAsAgent answers one of this session's units, enforcing the mission
// envelope's agent-answer bounds first. The bound has to hold HERE, not only
// where the question was offered: a session agent reaches a live askId from
// mission_list or a delivered ask and can call mission_answer unprompted, so
// the offer-side check it never went through would grant nothing. Runs the
// same hitlservice.AnswerAsAgentWithinBounds as `approvals respond --as-agent`
// and the oracle driver — one statement that counts and writes — so a mix of
// the three cannot exceed the envelope's total even concurrently.
func (s missionSupervision) AnswerAsAgent(ctx context.Context, askID, text string) error {
	if s.db == nil {
		return s.refuse(ctx, askID, "mission supervision has no store wired in this process, so the envelope's agent-answer bound cannot be read")
	}
	store := runtimetypes.New(s.db.WithoutTransaction())
	row, err := store.GetHITLApproval(ctx, askID)
	if err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return s.refuse(ctx, askID, fmt.Sprintf("ask %s no longer exists", askID))
		}
		return fmt.Errorf("read ask %s: %w", askID, err)
	}
	if !hitlservice.IsAttentionAsk(row) {
		return s.refuse(ctx, askID, fmt.Sprintf("ask %s is a permission request, not a question", askID))
	}
	if err := hitlservice.AnswerAsAgentWithinBounds(ctx, s.missions, s.hitl, row, "", text); err != nil {
		if hitlservice.IsAgentAnswerRefusal(err) {
			return s.refuse(ctx, askID, err.Error())
		}
		return err
	}
	return nil
}

// refuse states reason on the operator's trace and returns the typed refusal
// missiontools renders as the plain denied-per-policy result, reason discarded.
func (s missionSupervision) refuse(ctx context.Context, askID, reason string) error {
	tracker := s.tracker
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	_, reportChange, end := tracker.Start(ctx, "refuse", missiontools.ToolNameAnswer, "ask_id", askID)
	reportChange("refused", reason)
	end()
	return &missiontools.AnswerRefusedError{Reason: reason}
}
