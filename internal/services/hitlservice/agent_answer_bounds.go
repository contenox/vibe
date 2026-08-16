package hitlservice

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// MissionEnvelopeSource resolves the mission a durable ask belongs to, so
// the ask's envelope (the mission's HITL policy) can be read;
// missionservice.Service satisfies it.
type MissionEnvelopeSource interface {
	Get(ctx context.Context, id string) (*missionservice.Mission, error)
}

// AgentAnswerBoundsError is the envelope holding against an agent answer — a
// refusal, not broken plumbing.
type AgentAnswerBoundsError struct{ msg string }

func (e *AgentAnswerBoundsError) Error() string { return e.msg }

func boundsRefusal(format string, args ...any) error {
	return &AgentAnswerBoundsError{msg: fmt.Sprintf(format, args...)}
}

// IsAgentAnswerRefusal reports whether err is the envelope holding
// (AgentAnswerBoundsError).
func IsAgentAnswerRefusal(err error) bool {
	var refusal *AgentAnswerBoundsError
	return errors.As(err, &refusal)
}

// EnforceAgentAnswerBounds refuses an agent-attributed answer unless row's
// mission envelope grants agent answers with budget left; advisory only, the
// fast pre-check in front of AnswerAsAgentWithinBounds, which actually holds the bound.
func EnforceAgentAnswerBounds(ctx context.Context, missions MissionEnvelopeSource, svc Service, row *runtimetypes.HITLApproval) error {
	_, _, err := agentAnswerAllowance(ctx, missions, svc, row)
	return err
}

// AnswerAsAgentWithinBounds delivers an agent-attributed answer to row under its
// mission envelope. The cap rides the WHERE clause of the write, so concurrent
// answers cannot together exceed it.
func AnswerAsAgentWithinBounds(ctx context.Context, missions MissionEnvelopeSource, svc Service, row *runtimetypes.HITLApproval, agentName, text string) error {
	missionID, max, err := agentAnswerAllowance(ctx, missions, svc, row)
	if err != nil {
		return err
	}
	err = svc.AnswerAsAgentBounded(ctx, row.ID, agentName, text, max)
	if errors.Is(err, ErrAgentAnswerBoundSpent) {
		// Lost the write race: re-read so the refusal states the spend the winners actually made.
		used, countErr := svc.AgentAnswerCount(ctx, missionID)
		if countErr != nil {
			used = max
		}
		return boundsRefusal("agent answer refused: mission %s spent its agent-answer bound (%d of %d); this question waits for a human", missionID, used, max)
	}
	return err
}

func agentAnswerAllowance(ctx context.Context, missions MissionEnvelopeSource, svc Service, row *runtimetypes.HITLApproval) (string, int, error) {
	if row.MissionID == nil || strings.TrimSpace(*row.MissionID) == "" {
		return "", 0, boundsRefusal("agent answer refused: ask %s belongs to no mission, so no envelope grants agent answers; a human must answer", row.ID)
	}
	missionID := *row.MissionID
	m, err := missions.Get(ctx, missionID)
	if err != nil {
		return "", 0, boundsRefusal("agent answer refused: mission %s of ask %s cannot be read: %v; a human must answer", missionID, row.ID, err)
	}
	bounds, err := svc.AttentionBoundsFor(ctx, m.HITLPolicyName)
	if err != nil {
		return "", 0, boundsRefusal("agent answer refused: envelope %q of mission %s cannot be read: %v; a human must answer", m.HITLPolicyName, missionID, err)
	}
	if !bounds.AllowAgentAnswers {
		return "", 0, boundsRefusal("agent answer refused: envelope %q of mission %s does not allow agent answers (no attention.allowAgentAnswers grant); a human must answer", m.HITLPolicyName, missionID)
	}
	used, err := svc.AgentAnswerCount(ctx, missionID)
	if err != nil {
		return "", 0, boundsRefusal("agent answer refused: prior agent answers for mission %s cannot be counted: %v; a human must answer", missionID, err)
	}
	max := bounds.EffectiveMaxAgentAnswers()
	if used >= max {
		return "", 0, boundsRefusal("agent answer refused: mission %s spent its agent-answer bound (%d of %d); this question waits for a human", missionID, used, max)
	}
	return missionID, max, nil
}
