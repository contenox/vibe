package hitlservice

import (
	"context"
	"fmt"
	"strings"
)

// AttentionBounds is the envelope's say over who may answer a unit's
// question; the zero value means only a human may.
type AttentionBounds struct {
	// AllowAgentAnswers lets the session that fired the mission answer its
	// unit's questions with its own model instead of waiting for a human.
	AllowAgentAnswers bool `json:"allowAgentAnswers,omitempty"`
	// MaxAgentAnswers caps how many of this mission's questions an agent may
	// answer; zero means the default cap (DefaultMaxAgentAnswers), never unlimited.
	MaxAgentAnswers int `json:"maxAgentAnswers,omitempty"`
}

// DefaultMaxAgentAnswers bounds agent-answered questions per mission when
// the envelope allows them but names no cap.
const DefaultMaxAgentAnswers = 3

const maxAgentAnswersCeiling = 1_000

// EffectiveMaxAgentAnswers resolves the cap actually enforced.
func (b AttentionBounds) EffectiveMaxAgentAnswers() int {
	if b.MaxAgentAnswers <= 0 {
		return DefaultMaxAgentAnswers
	}
	return b.MaxAgentAnswers
}

// AttentionBoundsFor reads an envelope's attention half; a policy that
// declares none, or fails to load, yields the zero (human-only) bounds.
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
