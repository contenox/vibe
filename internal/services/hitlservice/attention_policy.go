package hitlservice

import (
	"context"
	"fmt"
	"strings"
)

// AttentionBounds is the envelope's say over WHO may answer a unit's question.
//
// The default — the zero value — is that only a human may. That is not timidity,
// it is what the tool means: a unit calls mission_ask_attention precisely because
// it hit something it must not decide alone, and letting another model answer in
// the human's place quietly deletes the escalation the unit performed. An
// operator who wants their supervising agent to handle routine questions says so
// in the envelope, per mission, and gets a bound with it.
type AttentionBounds struct {
	// AllowAgentAnswers lets the session that FIRED the mission answer its unit's
	// questions with its own model instead of waiting for a human. The human can
	// always still answer — this only decides whether the agent is offered the
	// question first.
	AllowAgentAnswers bool `json:"allowAgentAnswers,omitempty"`
	// MaxAgentAnswers caps how many of this mission's questions an agent may
	// answer; further questions wait for a human. Zero means the default cap
	// (DefaultMaxAgentAnswers), never "unlimited" — an unbounded question loop
	// between two models is exactly what this must not permit.
	MaxAgentAnswers int `json:"maxAgentAnswers,omitempty"`
}

// DefaultMaxAgentAnswers bounds agent-answered questions per mission when the
// envelope allows them but names no cap. Small on purpose: a supervisor that has
// answered its unit three times without the unit getting unstuck is a loop, and
// the fourth question is the one a human should see.
const DefaultMaxAgentAnswers = 3

// maxAgentAnswersCeiling rejects an absurd cap the way the compute bounds reject
// theirs: a hand-edited or hallucinated envelope naming ten million agent answers
// is a typo, not an intent.
const maxAgentAnswersCeiling = 1_000

// EffectiveMaxAgentAnswers resolves the cap actually enforced.
func (b AttentionBounds) EffectiveMaxAgentAnswers() int {
	if b.MaxAgentAnswers <= 0 {
		return DefaultMaxAgentAnswers
	}
	return b.MaxAgentAnswers
}

// AttentionBoundsFor reads an envelope's attention half. A policy that declares
// none yields the zero bounds — human-only — which is also what a policy that
// fails to load yields, because "the envelope could not be read" must never widen
// what a model may do.
func (s *service) AttentionBoundsFor(ctx context.Context, policyName string) (AttentionBounds, error) {
	policyPath := strings.TrimSpace(policyName)
	if policyPath == "" {
		policyPath = s.fallbackPolicy
	}
	if policyPath == "" {
		policyPath = defaultPolicyName
	}
	p, err := loadPolicy(ctx, s.src, s.tenantID, policyPath)
	if err != nil {
		return AttentionBounds{}, fmt.Errorf("hitlservice: load attention bounds for %q: %w", policyPath, err)
	}
	if p.Attention == nil {
		return AttentionBounds{}, nil
	}
	return *p.Attention, nil
}

// validateAttentionBounds rejects a nonsense cap at policy-load time, so a broken
// envelope fails loudly where it is written rather than silently at the moment a
// unit is waiting on an answer.
func validateAttentionBounds(b *AttentionBounds) error {
	if b == nil {
		return nil
	}
	if b.MaxAgentAnswers < 0 {
		return fmt.Errorf("attention.maxAgentAnswers must not be negative")
	}
	if b.MaxAgentAnswers > maxAgentAnswersCeiling {
		return fmt.Errorf("attention.maxAgentAnswers %d exceeds the sanity ceiling %d", b.MaxAgentAnswers, maxAgentAnswersCeiling)
	}
	return nil
}

var _ = context.Background
