package contenoxcli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/oracletools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// oracleDriverFixture is one host-shaped composition: shared db, real
// mission and hitl services, and the driver's answerer over them.
type oracleDriverFixture struct {
	store    runtimetypes.Store
	hitl     hitlservice.Service
	missions missionservice.Service
	answerer oracleAnswerer
	mission  *missionservice.Mission
}

func newOracleDriverFixture(t *testing.T, envelope string) *oracleDriverFixture {
	t.Helper()
	ctx := context.Background()
	policyDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(policyDir, "envelope.json"), []byte(envelope), 0o644))
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "t.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	store := runtimetypes.New(db.WithoutTransaction())
	hitl := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(policyDir), runtimetypes.LocalTenantID, store, libtracker.NoopTracker{}, "")
	missions := missionservice.New(db)
	m := &missionservice.Mission{Intent: "write the haiku", AgentName: "agent-x", HITLPolicyName: "envelope.json"}
	require.NoError(t, missions.Create(ctx, m))

	return &oracleDriverFixture{
		store:    store,
		hitl:     hitl,
		missions: missions,
		answerer: oracleAnswerer{hitl: hitl, missions: missions, store: store},
		mission:  m,
	}
}

// TestUnit_OracleAnswerer_InWindowAnswerWakesWaiterWithoutCheckpoint pins the
// in-window contract: the parked unit gets the oracle's words as its answer,
// the durable row records answeredBy=oracle, and NO checkpoint row exists —
// the run never suspended.
func TestUnit_OracleAnswerer_InWindowAnswerWakesWaiterWithoutCheckpoint(t *testing.T) {
	fx := newOracleDriverFixture(t, `{"default_action":"deny","rules":[],"attention":{"allowAgentAnswers":true,"maxAgentAnswers":3}}`)
	ctx := context.Background()
	askID := uuid.NewString()

	type waitResult struct {
		answer string
		err    error
	}
	got := make(chan waitResult, 1)
	go func() {
		answer, err := fx.hitl.RequestAttention(ctx, hitlservice.AttentionRequest{
			Summary:    "proceed?",
			MissionID:  fx.mission.ID,
			AskID:      askID,
			ParkWindow: 30 * time.Second,
		}, taskengine.NoopTaskEventSink{})
		got <- waitResult{answer: answer, err: err}
	}()
	require.Eventually(t, func() bool {
		_, err := fx.store.GetHITLApproval(ctx, askID)
		return err == nil
	}, 5*time.Second, 5*time.Millisecond, "the ask row exists before the driver answers")

	require.NoError(t, fx.answerer.Answer(ctx, askID, "Yes, proceed."))

	select {
	case res := <-got:
		require.NoError(t, res.err)
		require.Equal(t, "Yes, proceed.", res.answer, "the waiter wakes with the oracle's words")
	case <-time.After(5 * time.Second):
		t.Fatal("the parked waiter never woke")
	}

	row, err := fx.store.GetHITLApproval(ctx, askID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, row.State)
	require.Equal(t, oracleAgentName, hitlservice.AnsweredByOf(row), "the durable record attributes the answer to the oracle")

	_, err = fx.store.GetChainCheckpoint(ctx, askID)
	require.Error(t, err, "an in-window answer means the run never checkpointed")
}

// TestUnit_OracleAnswerer_BoundsRefusalLeavesTheNormalPath pins the refusal:
// the envelope holding maps to the typed denial and the ask stays pending —
// the untouched human path proceeds.
func TestUnit_OracleAnswerer_BoundsRefusalLeavesTheNormalPath(t *testing.T) {
	fx := newOracleDriverFixture(t, `{"default_action":"deny","rules":[]}`)
	ctx := context.Background()
	askID := uuid.NewString()
	missionID := fx.mission.ID
	now := time.Now().UTC()
	require.NoError(t, fx.store.CreateHITLApproval(ctx, &runtimetypes.HITLApproval{
		ID: askID, ToolsName: hitlservice.AttentionToolsName, ToolName: hitlservice.AttentionToolName,
		ArgsSummary: "proceed?", OnTimeout: string(hitlservice.ActionDeny),
		State: runtimetypes.HITLApprovalPending, MissionID: &missionID,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}))

	err := fx.answerer.Answer(ctx, askID, "Yes, proceed.")
	require.Error(t, err)
	var refused *oracletools.AnswerRefusedError
	require.ErrorAs(t, err, &refused, "a bounds refusal is the typed policy denial")
	require.Contains(t, refused.Reason, "does not allow agent answers")

	row, err := fx.store.GetHITLApproval(ctx, askID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State, "the question keeps waiting for a human")
}

// TestUnit_OracleAnswerer_GoneAskIsARefusalNotAFault pins the ask-gone
// mapping: not-found and already-resolved both map to the typed denial.
func TestUnit_OracleAnswerer_GoneAskIsARefusalNotAFault(t *testing.T) {
	fx := newOracleDriverFixture(t, `{"default_action":"deny","rules":[],"attention":{"allowAgentAnswers":true}}`)
	ctx := context.Background()

	err := fx.answerer.Answer(ctx, uuid.NewString(), "yes")
	require.Error(t, err)
	var refused *oracletools.AnswerRefusedError
	require.ErrorAs(t, err, &refused)
	require.Contains(t, refused.Reason, "no longer exists")

	askID := uuid.NewString()
	missionID := fx.mission.ID
	now := time.Now().UTC()
	require.NoError(t, fx.store.CreateHITLApproval(ctx, &runtimetypes.HITLApproval{
		ID: askID, ToolsName: hitlservice.AttentionToolsName, ToolName: hitlservice.AttentionToolName,
		ArgsSummary: "q", OnTimeout: string(hitlservice.ActionDeny),
		State: runtimetypes.HITLApprovalPending, MissionID: &missionID,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}))
	require.NoError(t, fx.hitl.Answer(ctx, askID, "a human got there first"))

	err = fx.answerer.Answer(ctx, askID, "yes")
	require.Error(t, err)
	require.ErrorAs(t, err, &refused)
}

// TestUnit_OracleAnswerer_RefusalStatesItsReasonToTheOperatorOnly pins both
// halves of the denial seam through the real tool: the operator gets one line
// naming WHY (without it, an envelope that forbids agent answers is
// indistinguishable from a genuine WAIT verdict), and the model-facing result
// stays the plain denial — no reason, no counts, no remedy.
func TestUnit_OracleAnswerer_RefusalStatesItsReasonToTheOperatorOnly(t *testing.T) {
	fx := newOracleDriverFixture(t, `{"default_action":"deny","rules":[]}`)
	ctx := context.Background()
	askID := uuid.NewString()
	missionID := fx.mission.ID
	now := time.Now().UTC()
	require.NoError(t, fx.store.CreateHITLApproval(ctx, &runtimetypes.HITLApproval{
		ID: askID, ToolsName: hitlservice.AttentionToolsName, ToolName: hitlservice.AttentionToolName,
		ArgsSummary: "proceed?", OnTimeout: string(hitlservice.ActionDeny),
		State: runtimetypes.HITLApprovalPending, MissionID: &missionID,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}))

	var trace bytes.Buffer
	p := oracletools.New(oracleAnswerer{hitl: fx.hitl, missions: fx.missions, store: fx.store, out: &trace})
	binding := oracletools.NewAskBinding(askID, `{"askId":"`+askID+`"}`)
	out, _, err := p.Exec(oracletools.WithBinding(ctx, binding), time.Now(),
		map[string]any{"verdict": "answer", "answer": "Yes, proceed.", "askId": askID}, false,
		&taskengine.ToolsCall{Name: oracletools.ToolsProviderName, ToolName: oracletools.ToolNameSubmitVerdict})
	require.NoError(t, err)

	res, ok := out.(map[string]any)
	require.True(t, ok)
	require.Equal(t, false, res["accepted"])
	require.Equal(t, fmt.Sprintf("answer denied per policy for ask %s.", askID), res["message"],
		"the model-facing denial is one plain statement: no reason, no bound counts, no remedy")

	line := trace.String()
	require.Contains(t, line, fmt.Sprintf("oracle: answer refused for ask %s: ", askID),
		"the operator learns a refusal happened, or it reads as a genuine WAIT")
	require.Contains(t, line, "does not allow agent answers", "and why")
	require.Equal(t, 1, strings.Count(line, "\n"), "exactly one line per refusal")
	require.Equal(t, oracletools.OutcomeNone, binding.Outcome(), "a denial never settles the contract")
}

// TestUnit_OracleDriver_DeclinesParentedAsks pins the sibling split on the
// shared supervisor seam: a parented ask belongs to the firing-agent offer,
// so the driver runs no chain at all for it.
func TestUnit_OracleDriver_DeclinesParentedAsks(t *testing.T) {
	d := &oracleAttentionDriver{} // a nil agent would panic if a chain ran
	require.NoError(t, d.OfferToSupervisingAgent(context.Background(), missionservice.AttentionAskedEvent{
		MissionID: "m-1", AskID: "ask-1", ParentSessionID: "cnx-parent", Summary: "q",
	}))
	require.NoError(t, d.OfferToSupervisingAgent(context.Background(), missionservice.AttentionAskedEvent{
		MissionID: "m-1", Summary: "q",
	}), "an event with no ask id is declined too")
}

// TestUnit_AttentionParkWindow_UnchangedWithoutOracle pins the WAIT/normal
// path: nobody answers, the park window elapses, and the caller gets the
// typed pending error exactly as before — the checkpoint path's front door.
func TestUnit_AttentionParkWindow_UnchangedWithoutOracle(t *testing.T) {
	fx := newOracleDriverFixture(t, `{"default_action":"deny","rules":[]}`)
	askID := uuid.NewString()
	_, err := fx.hitl.RequestAttention(context.Background(), hitlservice.AttentionRequest{
		Summary:    "proceed?",
		MissionID:  fx.mission.ID,
		AskID:      askID,
		ParkWindow: 50 * time.Millisecond,
	}, taskengine.NoopTaskEventSink{})
	var pending *hitlservice.AttentionPendingError
	require.ErrorAs(t, err, &pending)
	require.Equal(t, askID, pending.AskID)

	row, rerr := fx.store.GetHITLApproval(context.Background(), askID)
	require.NoError(t, rerr)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State)
}
