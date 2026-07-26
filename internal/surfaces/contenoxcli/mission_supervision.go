package contenoxcli

import (
	"context"
	"time"

	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/missiontools"
)

// missionSupervision is the composition-root adapter that gives a session which
// FIRED missions the two things it needs to actually supervise them: a read of
// its own units, and a way to answer what they ask.
//
// It sits here, at the composition root, because it is the one place that holds
// both halves — missions live in missionservice, questions live in the durable
// ask store (hitlservice), and the tool package deliberately depends on neither.
// Both `contenox serve` and the in-process editor wire the same value, so a
// supervising session behaves identically wherever it is hosted.
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

// MissionsFiredBy returns the missions this session fired, newest first.
//
// It filters a bounded page rather than querying by parent, because the mission
// store is a KV prefix scan with no secondary index — a "list by parent" would be
// this same scan wearing a different name, and pretending otherwise would hide
// the bound from the caller.
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
