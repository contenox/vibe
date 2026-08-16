package contenoxcli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/fleetservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
)

// supervisorScanPage bounds the page scanned for a session's own missions;
// missionservice has no parent-session index, so the filter runs here.
const supervisorScanPage = 200

type missionSupervision struct {
	missions missionservice.Service
	hitl     hitlservice.Service
	db       libdb.DBManager
	tracker  libtracker.ActivityTracker
}

var (
	_ missiontools.SupervisorStore   = missionSupervision{}
	_ missiontools.AttentionResolver = missionSupervision{}
)

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

func (s missionSupervision) PendingAsks(ctx context.Context, missionID string) ([]missiontools.PendingAsk, error) {
	if s.hitl == nil {
		return nil, nil
	}
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

// The bound is enforced here, not only where the ask was offered: an agent can
// reach a live askId from mission_list and call mission_answer unprompted.
func (s missionSupervision) AnswerAsAgent(ctx context.Context, askID, text string) error {
	if s.db == nil || s.hitl == nil {
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

// fleetSpawner is the write half of the supervisor surface: start one subagent.
// The dispatcher is resolved per call rather than captured, since the fleet is
// built after the toolset.
type fleetSpawner struct {
	fleet func() fleetservice.Service
}

func (f fleetSpawner) Spawn(ctx context.Context, spec missiontools.SubagentSpec) (missiontools.SubagentHandle, error) {
	if f.fleet == nil {
		return missiontools.SubagentHandle{}, fmt.Errorf("the fleet is not running in this process")
	}
	fleet := f.fleet()
	if fleet == nil {
		return missiontools.SubagentHandle{}, fmt.Errorf("the fleet is not running in this process")
	}
	res, err := fleet.Dispatch(ctx, fleetservice.DispatchRequest{
		AgentName:       spec.AgentName,
		Intent:          spec.Intent,
		HITLPolicyName:  spec.HITLPolicyName,
		ParentSessionID: spec.ParentSessionID,
	})
	if err != nil {
		return missiontools.SubagentHandle{}, err
	}
	return missiontools.SubagentHandle{MissionID: res.MissionID}, nil
}

// subagentDefaults reads the agent and envelope a subagent started with neither
// named runs under.
func subagentDefaults(db libdb.DBManager) missiontools.SubagentDefaults {
	return func(ctx context.Context) (string, string) {
		store := runtimetypes.New(db.WithoutTransaction())
		return strings.TrimSpace(clikv.Read(ctx, store, "default-mission-agent")),
			strings.TrimSpace(clikv.Read(ctx, store, "default-mission-policy"))
	}
}
