package contenoxcli

import (
	"context"
	"time"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
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

func (s missionSupervision) AnswerAsAgent(ctx context.Context, askID, text string) error {
	return s.hitl.AnswerAsAgent(ctx, askID, text)
}
