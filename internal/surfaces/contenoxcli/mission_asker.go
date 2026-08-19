package contenoxcli

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
)

type missionAttentionAsker struct {
	hitl     hitlservice.Service
	missions missionservice.Service
	bus      missionservice.EventPublisher
}

var _ missiontools.AttentionAsker = missionAttentionAsker{}

func (a missionAttentionAsker) RaiseAttention(ctx context.Context, ask missiontools.AttentionAsk) (string, error) {
	missionID, summary, detail := ask.MissionID, ask.Summary, ask.Detail
	var parentSessionID, agentName, intent string
	if a.missions != nil {
		if m, err := a.missions.Get(ctx, missionID); err == nil && m != nil {
			parentSessionID, agentName, intent = m.ParentSessionID, m.AgentName, m.Intent
		}
	}
	answer, err := a.hitl.RequestAttention(ctx, hitlservice.AttentionRequest{
		Summary:   summary,
		Detail:    detail,
		MissionID: missionID,
		AgentName: agentName,
		AskID:     ask.AskID,
		Detached:  ask.Detached,
		OnRaised: func(askID string) {
			a.publishAsked(ctx, missionservice.AttentionAskedEvent{
				MissionID:       missionID,
				AskID:           askID,
				ParentSessionID: parentSessionID,
				AgentName:       agentName,
				Intent:          intent,
				Summary:         summary,
				Detail:          detail,
			})
		},
	}, taskengine.NoopTaskEventSink{})
	var pending *hitlservice.AttentionPendingError
	if errors.As(err, &pending) {
		return "", &taskengine.ApprovalPendingError{ApprovalID: pending.AskID, ToolName: missiontools.ToolNameAskAttention}
	}
	return answer, err
}

func (a missionAttentionAsker) publishAsked(ctx context.Context, ev missionservice.AttentionAskedEvent) {
	if a.bus == nil {
		return
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_ = a.bus.Publish(ctx, missionservice.AttentionAskedSubject, raw)
}
