package hitlservice_test

// The agent-answer bound is held by the write, not by the caller's check.
// AnswerAsAgentWithinBounds resolves the envelope's cap and then delivers
// through a single conditional statement that counts the mission's prior agent
// answers in its own WHERE clause, so a check that went stale between reading
// and writing cannot spend budget that is gone. These tests pin that the cap is
// exact when answers serialize AND when they do not.

import (
	"context"
	"sync"
	"testing"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// answerAsAgentBounded is the exact delivery all three agent-answer surfaces
// run: contenoxcli's `approvals respond --as-agent`, the oracle attention
// driver, and the `mission_answer` supervision tool each call
// AnswerAsAgentWithinBounds. Spelled here so a test exercises the real seam
// rather than a paraphrase of it.
func answerAsAgentBounded(ctx context.Context, svc hitlservice.Service, missions hitlservice.MissionEnvelopeSource, row *runtimetypes.HITLApproval, text string) error {
	return hitlservice.AnswerAsAgentWithinBounds(ctx, missions, svc, row, "oracle", text)
}

// seedAsks persists n pending attention asks on missionID.
func seedAsks(t *testing.T, ctx context.Context, store runtimetypes.Store, missionID string, n int) []*runtimetypes.HITLApproval {
	t.Helper()
	rows := make([]*runtimetypes.HITLApproval, 0, n)
	for i := 0; i < n; i++ {
		row := attentionRow(missionID)
		require.NoError(t, store.CreateHITLApproval(ctx, row))
		rows = append(rows, row)
	}
	return rows
}

// TestUnit_AgentAnswerBounds_SerialAnswersStopExactlyAtTheCap is the control:
// when answers do not overlap, the envelope's cap holds to the unit.
func TestUnit_AgentAnswerBounds_SerialAnswersStopExactlyAtTheCap(t *testing.T) {
	const cap = 2
	const asks = 6
	ctx := context.Background()
	svc, store, policy := boundsFixture(t, `{"default_action":"deny","rules":[],"attention":{"allowAgentAnswers":true,"maxAgentAnswers":2}}`)
	missions := fakeMissions{policy: policy}

	const missionID = "m-serial"
	delivered, refused := 0, 0
	for _, row := range seedAsks(t, ctx, store, missionID, asks) {
		err := answerAsAgentBounded(ctx, svc, missions, row, "agent reply")
		switch {
		case err == nil:
			delivered++
		case hitlservice.IsAgentAnswerRefusal(err):
			refused++
		default:
			t.Fatalf("unexpected error delivering an agent answer: %v", err)
		}
	}

	require.Equal(t, cap, delivered, "exactly the cap may be spent")
	require.Equal(t, asks-cap, refused, "every answer past the cap is refused by policy, not by an error")

	used, err := svc.AgentAnswerCount(ctx, missionID)
	require.NoError(t, err)
	require.Equal(t, cap, used)
}

// TestUnit_AgentAnswerBounds_TheWriteHoldsTheBoundNotTheCheck exercises the
// interleaving the check alone cannot survive: every caller reads the count
// before any of them writes, so every check passes. Deterministic on purpose —
// it drives the interleaving directly rather than hoping the scheduler
// produces it. The bound still holds, because the conditional write re-counts
// inside the same statement.
func TestUnit_AgentAnswerBounds_TheWriteHoldsTheBoundNotTheCheck(t *testing.T) {
	const cap = 2
	const racers = 5
	ctx := context.Background()
	svc, store, policy := boundsFixture(t, `{"default_action":"deny","rules":[],"attention":{"allowAgentAnswers":true,"maxAgentAnswers":2}}`)
	missions := fakeMissions{policy: policy}

	const missionID = "m-interleaved"
	rows := seedAsks(t, ctx, store, missionID, racers)

	// Phase one: every caller checks the envelope. Nobody has written yet, so
	// the count each of them reads is zero and the advisory check passes for
	// all five — exactly the stale read the bound must survive.
	for _, row := range rows {
		require.NoError(t, hitlservice.EnforceAgentAnswerBounds(ctx, missions, svc, row),
			"the pre-check reads a count that is still zero for everyone")
	}
	// Phase two: every caller delivers on that stale grant. The cap rides the
	// write, so only the first two statements find fewer than two prior agent
	// answers.
	delivered, spent := 0, 0
	for _, row := range rows {
		err := svc.AnswerAsAgentBounded(ctx, row.ID, "oracle", "agent reply", cap)
		if err == nil {
			delivered++
			continue
		}
		require.ErrorIs(t, err, hitlservice.ErrAgentAnswerBoundSpent)
		spent++
	}
	require.Equal(t, cap, delivered, "the conditional write admits exactly the cap")
	require.Equal(t, racers-cap, spent, "every later write is refused by the bound, not by the row")

	used, err := svc.AgentAnswerCount(ctx, missionID)
	require.NoError(t, err)
	require.Equal(t, cap, used, "a mission capped at %d agent answers spent %d", cap, used)

	// The composed surface path renders that same refusal in the operator's
	// words, so a stale grant reads as the envelope holding, not as plumbing.
	err = answerAsAgentBounded(ctx, svc, missions, rows[racers-1], "agent reply")
	require.True(t, hitlservice.IsAgentAnswerRefusal(err))
	require.Contains(t, err.Error(), "spent its agent-answer bound (2 of 2)")
	require.Contains(t, err.Error(), "this question waits for a human")
}

// TestUnit_AgentAnswerBounds_ConcurrentAnswersStopExactlyAtTheCap races the
// real composition through goroutines. The cap is a database predicate, so the
// outcome is schedule-independent: exactly the cap lands, every other racer is
// refused by the envelope, and nothing fails for another reason.
func TestUnit_AgentAnswerBounds_ConcurrentAnswersStopExactlyAtTheCap(t *testing.T) {
	const cap = 2
	const racers = 8
	ctx := context.Background()
	svc, store, policy := boundsFixture(t, `{"default_action":"deny","rules":[],"attention":{"allowAgentAnswers":true,"maxAgentAnswers":2}}`)
	missions := fakeMissions{policy: policy}

	const missionID = "m-raced"
	rows := seedAsks(t, ctx, store, missionID, racers)

	start := make(chan struct{})
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i, row := range rows {
		wg.Add(1)
		go func(i int, row *runtimetypes.HITLApproval) {
			defer wg.Done()
			<-start
			errs[i] = answerAsAgentBounded(ctx, svc, missions, row, "agent reply")
		}(i, row)
	}
	close(start)
	wg.Wait()

	delivered, refused := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			delivered++
		case hitlservice.IsAgentAnswerRefusal(err):
			refused++
		default:
			t.Fatalf("racer %d failed for a reason that is not the envelope: %v", i, err)
		}
	}
	require.Equal(t, cap, delivered, "the cap is spendable and not overrunnable under concurrency")
	require.Equal(t, racers-cap, refused, "every loser is refused by the envelope, not by an error")

	used, err := svc.AgentAnswerCount(ctx, missionID)
	require.NoError(t, err)
	require.Equal(t, cap, used, "the durable count matches the cap exactly")
}

// TestUnit_AgentAnswerBounds_AnswerAsAgentEnforcesNothingItself pins where the
// bound does NOT live: the service's unbounded agent-answer entry points write
// the durable row with no envelope check at all. They are not a surface's to
// call — every surface goes through AnswerAsAgentWithinBounds — and this test
// is what makes that a rule rather than a habit.
func TestUnit_AgentAnswerBounds_AnswerAsAgentEnforcesNothingItself(t *testing.T) {
	ctx := context.Background()
	// An envelope that forbids agent answers outright.
	svc, store, _ := boundsFixture(t, `{"default_action":"deny","rules":[]}`)

	const missionID = "m-unbounded"
	for _, row := range seedAsks(t, ctx, store, missionID, 5) {
		require.NoError(t, svc.AnswerAsAgent(ctx, row.ID, "agent reply"),
			"AnswerAsAgent honours no envelope; the grant is AnswerAsAgentWithinBounds's to check")
	}

	used, err := svc.AgentAnswerCount(ctx, missionID)
	require.NoError(t, err)
	require.Equal(t, 5, used, "five agent answers landed on a mission whose envelope grants none")
}
