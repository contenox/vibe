package contenoxcli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// TestUnit_SeededPolicies_StateWhoMayAnswer pins the governance stance every
// shipped preset takes on a unit's question — including, deliberately, the two
// that refuse.
//
// It exists because this is the one setting where a silent edit changes who is
// allowed to decide: flipping `allowAgentAnswers` on in `strict` or `acpx` would
// let a model answer an escalation those presets exist to route to a person, and
// nothing else in the test suite would notice. The values are asserted through
// the REAL policy loader, so a preset that stops parsing fails here too.
func TestUnit_SeededPolicies_StateWhoMayAnswer(t *testing.T) {
	dir := t.TempDir()
	for _, p := range HITLPolicyPresets {
		require.NoError(t, os.WriteFile(filepath.Join(dir, p.Name), []byte(p.Content), 0o600))
	}
	svc := hitlservice.New(hitlservice.NewFSPolicySource(dir), runtimetypes.LocalTenantID, nil, libtracker.NoopTracker{})

	for _, tc := range []struct {
		policy    string
		mayAnswer bool
		cap       int
		why       string
	}{
		{"hitl-policy-acp.json", true, 3, "an editor session's agent holds the conversation the mission came from"},
		{"hitl-policy-default.json", true, 2, "routine questions, with whatever the unit then does still gated"},
		{"hitl-policy-dev.json", true, 5, "the permissive local-development posture"},
		{"hitl-policy-strict.json", false, 0, "a policy whose character is 'a human decides' must not delegate deciding"},
		{"hitl-policy-acpx.json", false, 0, "an untrusted driver's agent must not answer its own subagent"},
	} {
		bounds, err := svc.AttentionBoundsFor(context.Background(), tc.policy)
		require.NoErrorf(t, err, "%s must load", tc.policy)
		require.Equalf(t, tc.mayAnswer, bounds.AllowAgentAnswers, "%s: %s", tc.policy, tc.why)
		if tc.mayAnswer {
			require.Equalf(t, tc.cap, bounds.EffectiveMaxAgentAnswers(), "%s: an allowed preset still names its cap", tc.policy)
		}
	}
}

// TestUnit_SeededPolicies_DeclareAttentionExplicitly guards the legibility half:
// every preset SAYS its stance rather than inheriting the default, so an operator
// reading one file knows the knob exists.
func TestUnit_SeededPolicies_DeclareAttentionExplicitly(t *testing.T) {
	for _, p := range HITLPolicyPresets {
		var doc map[string]any
		require.NoErrorf(t, json.Unmarshal([]byte(p.Content), &doc), "%s must be valid JSON", p.Name)
		require.Containsf(t, doc, "attention", "%s must state who may answer a unit's question", p.Name)
	}
}
