package hitlservice_test

import (
	"context"
	"sync"
	"testing"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func answerAsAgentBounded(ctx context.Context, svc hitlservice.Service, missions hitlservice.MissionEnvelopeSource, row *runtimetypes.HITLApproval, text string) error {
	return hitlservice.AnswerAsAgentWithinBounds(ctx, missions, svc, row, "oracle", text)
}

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
// interleaving the check alone cannot survive: every caller reads a stale
// zero count, but the conditional write re-counts inside the same statement.
func TestUnit_AgentAnswerBounds_TheWriteHoldsTheBoundNotTheCheck(t *testing.T) {
	const cap = 2
	const racers = 5
	ctx := context.Background()
	svc, store, policy := boundsFixture(t, `{"default_action":"deny","rules":[],"attention":{"allowAgentAnswers":true,"maxAgentAnswers":2}}`)
	missions := fakeMissions{policy: policy}

	const missionID = "m-interleaved"
	rows := seedAsks(t, ctx, store, missionID, racers)

	// Phase one: every caller checks the envelope while the count is still zero for all five — the stale read the bound must survive.
	for _, row := range rows {
		require.NoError(t, hitlservice.EnforceAgentAnswerBounds(ctx, missions, svc, row),
			"the pre-check reads a count that is still zero for everyone")
	}
	// Phase two: every caller delivers on that stale grant; the cap rides the write, so only the first two find fewer than two prior answers.
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

	// The composed surface renders the same refusal in the operator's words: a stale grant reads as the envelope holding, not plumbing.
	err = answerAsAgentBounded(ctx, svc, missions, rows[racers-1], "agent reply")
	require.True(t, hitlservice.IsAgentAnswerRefusal(err))
	require.Contains(t, err.Error(), "spent its agent-answer bound (2 of 2)")
	require.Contains(t, err.Error(), "this question waits for a human")
}

// TestUnit_AgentAnswerBounds_ConcurrentAnswersStopExactlyAtTheCap races the
// real composition through goroutines; the cap is a database predicate, so
// the outcome is schedule-independent.
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

// TestUnit_AgentAnswerBounds_AnswerAsAgentEnforcesNothingItself pins where
// the bound does NOT live: the service's unbounded agent-answer entry points
// write the durable row with no envelope check at all.
func TestUnit_AgentAnswerBounds_AnswerAsAgentEnforcesNothingItself(t *testing.T) {
	ctx := context.Background()
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
