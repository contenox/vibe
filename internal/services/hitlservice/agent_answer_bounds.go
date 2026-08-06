package hitlservice

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// MissionEnvelopeSource resolves the mission a durable ask belongs to, so the
// ask's envelope (the mission's HITL policy) can be read.
// missionservice.Service satisfies it.
type MissionEnvelopeSource interface {
	Get(ctx context.Context, id string) (*missionservice.Mission, error)
}

// AgentAnswerBoundsError is the envelope holding against an agent answer — a
// refusal, not broken plumbing. Callers branch on it with errors.As: the CLI
// prints it and exits clean, the oracle driver ends its contract
// WAIT-equivalent and never retries.
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
// mission envelope grants agent answers with budget left, counted on the
// durable records. Advisory on its own — the count it reads can go stale
// before the answer is written — so it is the fast, well-worded refusal in
// front of AnswerAsAgentWithinBounds, which is what actually holds the bound.
// Every refusal (an *AgentAnswerBoundsError) names the human path: the
// invariant holding, not an error to soften.
func EnforceAgentAnswerBounds(ctx context.Context, missions MissionEnvelopeSource, svc Service, row *runtimetypes.HITLApproval) error {
	_, _, err := agentAnswerAllowance(ctx, missions, svc, row)
	return err
}

// AnswerAsAgentWithinBounds delivers an agent-attributed answer to row under
// its mission envelope, atomically: the envelope's cap rides the WHERE clause
// of the one statement that writes the resolution, so concurrent answers —
// across goroutines and across processes, which no mutex spans — cannot
// together exceed it. The one delivery every agent-answer surface runs
// (`approvals respond --as-agent`, the oracle attention driver, the
// `mission_answer` supervision tool) so they cannot drift, and so a mix of the
// three still totals at most the envelope's cap. A blank agentName degrades to
// the generic agent marker.
func AnswerAsAgentWithinBounds(ctx context.Context, missions MissionEnvelopeSource, svc Service, row *runtimetypes.HITLApproval, agentName, text string) error {
	missionID, max, err := agentAnswerAllowance(ctx, missions, svc, row)
	if err != nil {
		return err
	}
	err = svc.AnswerAsAgentBounded(ctx, row.ID, agentName, text, max)
	if errors.Is(err, ErrAgentAnswerBoundSpent) {
		// Lost the write race: re-read so the refusal states the spend the
		// winners actually made, in the same words the pre-check uses.
		used, countErr := svc.AgentAnswerCount(ctx, missionID)
		if countErr != nil {
			used = max
		}
		return boundsRefusal("agent answer refused: mission %s spent its agent-answer bound (%d of %d); this question waits for a human", missionID, used, max)
	}
	return err
}

// agentAnswerAllowance resolves row's mission envelope and returns the mission
// and the cap an agent answer must fit under, or the typed refusal. Spelled
// once so the pre-check and the atomic delivery cannot word or count the
// envelope differently.
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
