package fleetservice

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// TestFleetE2E_AgentAnswerBounds is the acceptance for the autonomous edge of
// the ask channel: a supervising agent may answer its own subagent's
// question only when the envelope opts in, and only up to its cap, enforced
// durably and actor-aware.
func TestFleetE2E_AgentAnswerBounds(t *testing.T) {
	ctx := context.Background()
	policyDir := t.TempDir()
	db, err := libdb.NewSQLiteDBManager(ctx, t.TempDir()+"/agent-answer.db", runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	store := runtimetypes.New(db.WithoutTransaction())
	hitl := hitlservice.New(hitlservice.NewFSPolicySource(policyDir), runtimetypes.LocalTenantID, store, libtracker.NoopTracker{})

	// Two envelopes: the default posture, and one that opts in with a cap of two.
	humanOnly := writePolicy(t, policyDir, "human-only.json", map[string]any{
		"default_action": "approve",
		"rules":          []any{},
	})
	agentOK := writePolicy(t, policyDir, "agent-answers.json", map[string]any{
		"default_action": "approve",
		"rules":          []any{},
		"attention":      map[string]any{"allowAgentAnswers": true, "maxAgentAnswers": 2},
	})

	// The default envelope keeps questions human-only — the whole point of the
	// escalation the unit performed.
	bounds, err := hitl.AttentionBoundsFor(ctx, humanOnly)
	require.NoError(t, err)
	require.False(t, bounds.AllowAgentAnswers, "an envelope that says nothing must not let a model answer")

	// The opt-in envelope carries its own cap.
	bounds, err = hitl.AttentionBoundsFor(ctx, agentOK)
	require.NoError(t, err)
	require.True(t, bounds.AllowAgentAnswers)
	require.Equal(t, 2, bounds.EffectiveMaxAgentAnswers())

	// An envelope that opts in without a cap still gets one: "allowed" never means
	// "unbounded" for a model answering a model.
	uncapped := writePolicy(t, policyDir, "agent-uncapped.json", map[string]any{
		"default_action": "approve",
		"rules":          []any{},
		"attention":      map[string]any{"allowAgentAnswers": true},
	})
	bounds, err = hitl.AttentionBoundsFor(ctx, uncapped)
	require.NoError(t, err)
	require.Equal(t, hitlservice.DefaultMaxAgentAnswers, bounds.EffectiveMaxAgentAnswers())

	// The count the cap is enforced against is durable and actor-aware: two
	// agent answers and one human answer on the same mission read as two.
	const missionID = "m-cap"
	for i := 0; i < 2; i++ {
		askID := raisePendingAsk(t, ctx, hitl, missionID)
		require.NoError(t, hitl.AnswerAsAgent(ctx, askID, "agent reply"))
	}
	humanAsk := raisePendingAsk(t, ctx, hitl, missionID)
	require.NoError(t, hitl.Answer(ctx, humanAsk, "human reply"))

	used, err := hitl.AgentAnswerCount(ctx, missionID)
	require.NoError(t, err)
	require.Equal(t, 2, used, "a human's answer must not consume the agent budget")
}

// raisePendingAsk parks a question on missionID and returns its id, so a test can
// answer it as whichever actor it is exercising.
func raisePendingAsk(t *testing.T, ctx context.Context, hitl hitlservice.Service, missionID string) string {
	t.Helper()
	raised := make(chan string, 1)
	go func() {
		_, _ = hitl.RequestAttention(ctx, hitlservice.AttentionRequest{
			Summary:   "which project?",
			MissionID: missionID,
			OnRaised:  func(askID string) { raised <- askID },
		}, nil)
	}()
	select {
	case id := <-raised:
		return id
	case <-time.After(10 * time.Second):
		t.Fatal("the ask was never raised")
		return ""
	}
}
