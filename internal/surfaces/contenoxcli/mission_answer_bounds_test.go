// mission_answer_bounds_test.go pins the bounded-delegation guarantee at the
// mission_answer write. A session agent can reach a live askId from
// mission_list or a delivered ask and call the tool unprompted, so the offer
// path's check never runs for it; the envelope has to hold at the write or it
// holds nowhere.
package contenoxcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// parentSessionID is the supervising session mission_answer authorizes on.
const boundsParentSession = "cnx-supervisor"

// missionAnswerFixture is the ACP host's own composition: one db, the real
// mission and hitl services, the supervision adapter acp_toolset.go wires,
// and the mission tools provider on top of it.
type missionAnswerFixture struct {
	ctx      context.Context
	store    runtimetypes.Store
	hitl     hitlservice.Service
	missions missionservice.Service
	sup      missionSupervision
	tools    taskengine.ToolsRepo
	mission  *missionservice.Mission
}

func newMissionAnswerFixture(t *testing.T, envelope string) *missionAnswerFixture {
	t.Helper()
	ctx := context.Background()
	policyDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(policyDir, "envelope.json"), []byte(envelope), 0o644))
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "mission-answer.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	store := runtimetypes.New(db.WithoutTransaction())
	hitl := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(policyDir),
		runtimetypes.LocalTenantID, store, libtracker.NoopTracker{}, "")
	missions := missionservice.New(db)
	m := &missionservice.Mission{
		Intent: "write the haiku", AgentName: "agent-x",
		HITLPolicyName: "envelope.json", ParentSessionID: boundsParentSession,
	}
	require.NoError(t, missions.Create(ctx, m))

	sup := missionSupervision{missions: missions, hitl: hitl, db: db, tracker: libtracker.NoopTracker{}}
	return &missionAnswerFixture{
		ctx: ctx, store: store, hitl: hitl, missions: missions, sup: sup,
		tools:   missiontools.New(missions, nil, missiontools.WithSupervision(sup, sup)),
		mission: m,
	}
}

// ask creates one pending attention row against the fixture's mission.
func (fx *missionAnswerFixture) ask(t *testing.T) string {
	t.Helper()
	askID := uuid.NewString()
	now := time.Now().UTC()
	missionID := fx.mission.ID
	require.NoError(t, fx.store.CreateHITLApproval(fx.ctx, &runtimetypes.HITLApproval{
		ID: askID, ToolsName: hitlservice.AttentionToolsName, ToolName: hitlservice.AttentionToolName,
		ArgsSummary: "which project?", OnTimeout: string(hitlservice.ActionDeny),
		State: runtimetypes.HITLApprovalPending, MissionID: &missionID,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}))
	return askID
}

// answer drives the real mission_answer tool the way the engine does.
func (fx *missionAnswerFixture) answer(t *testing.T, askID, text string) (any, error) {
	t.Helper()
	out, _, err := fx.tools.Exec(
		missiontools.WithParentSessionID(fx.ctx, boundsParentSession), time.Now(), nil, false,
		&taskengine.ToolsCall{
			Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNameAnswer,
			Args: map[string]string{"askId": askID, "answer": text},
		})
	return out, err
}

func deniedResult(askID string) string {
	return fmt.Sprintf("answer denied per policy for ask %s.", askID)
}

// TestUnit_MissionAnswer_RefusedWhenEnvelopeForbidsAgentAnswers pins the
// security fix: an unprompted mission_answer under allowAgentAnswers:false is
// refused at the write, so the ask stays pending for a human.
func TestUnit_MissionAnswer_RefusedWhenEnvelopeForbidsAgentAnswers(t *testing.T) {
	fx := newMissionAnswerFixture(t, `{"default_action":"deny","rules":[]}`)
	askID := fx.ask(t)

	out, err := fx.answer(t, askID, "just do it")
	require.NoError(t, err, "a policy denial is a result the model reads, not a tool error")
	require.Equal(t, deniedResult(askID), out,
		"the model gets the plain denial; the reason stays on the operator's trace")

	row, err := fx.store.GetHITLApproval(fx.ctx, askID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State, "the question still waits for a human")
	require.Empty(t, hitlservice.AnsweredByOf(row))
}

// TestUnit_MissionAnswer_WithinBoundsSucceedsAndIsAttributed pins the allowed
// path: the answer lands and the durable row records that an agent gave it,
// which is what the cap counts.
func TestUnit_MissionAnswer_WithinBoundsSucceedsAndIsAttributed(t *testing.T) {
	fx := newMissionAnswerFixture(t, `{"default_action":"deny","rules":[],"attention":{"allowAgentAnswers":true,"maxAgentAnswers":2}}`)
	askID := fx.ask(t)

	out, err := fx.answer(t, askID, "the runtime repo, docs/ only")
	require.NoError(t, err)
	require.Contains(t, out.(string), askID)
	require.NotEqual(t, deniedResult(askID), out)

	row, err := fx.store.GetHITLApproval(fx.ctx, askID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, row.State)
	require.NotEmpty(t, hitlservice.AnsweredByOf(row), "an agent answer must be attributed, never look human")
	require.Equal(t, "the runtime repo, docs/ only", hitlservice.AnswerOf(row))

	used, err := fx.hitl.AgentAnswerCount(fx.ctx, fx.mission.ID)
	require.NoError(t, err)
	require.Equal(t, 1, used)
}

// TestUnit_MissionAnswer_RefusedBeyondMaxAgentAnswers pins the cap: the
// budgeted answers land, the next one does not.
func TestUnit_MissionAnswer_RefusedBeyondMaxAgentAnswers(t *testing.T) {
	fx := newMissionAnswerFixture(t, `{"default_action":"deny","rules":[],"attention":{"allowAgentAnswers":true,"maxAgentAnswers":2}}`)

	for i := range 2 {
		askID := fx.ask(t)
		out, err := fx.answer(t, askID, "within budget")
		require.NoError(t, err)
		require.NotEqualf(t, deniedResult(askID), out, "answer %d is inside the bound", i+1)
	}

	spent := fx.ask(t)
	out, err := fx.answer(t, spent, "one too many")
	require.NoError(t, err)
	require.Equal(t, deniedResult(spent), out, "the third answer exceeds maxAgentAnswers")

	row, err := fx.store.GetHITLApproval(fx.ctx, spent)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State)

	used, err := fx.hitl.AgentAnswerCount(fx.ctx, fx.mission.ID)
	require.NoError(t, err)
	require.Equal(t, 2, used, "the refusal spends nothing")
}

// TestUnit_MissionAnswer_SharesTheBoundWithTheCLIAgentPath pins that the two
// agent-answer write paths draw on one durable budget: a mix of `approvals
// respond --as-agent` and mission_answer cannot exceed the envelope's total.
// Enforcing per-path would let a supervisor double the grant by alternating.
func TestUnit_MissionAnswer_SharesTheBoundWithTheCLIAgentPath(t *testing.T) {
	fx := newMissionAnswerFixture(t, `{"default_action":"deny","rules":[],"attention":{"allowAgentAnswers":true,"maxAgentAnswers":2}}`)

	// One answer through the CLI's --as-agent composition (the enforcement
	// approvals_cmd.go runs, then the same named delivery).
	cliAsk := fx.ask(t)
	cliRow, err := fx.store.GetHITLApproval(fx.ctx, cliAsk)
	require.NoError(t, err)
	require.NoError(t, hitlservice.EnforceAgentAnswerBounds(fx.ctx, fx.missions, fx.hitl, cliRow))
	require.NoError(t, fx.hitl.AnswerAsAgentNamed(fx.ctx, cliAsk, "reviewer", "from the CLI"))

	// One through mission_answer: still inside the shared bound of 2.
	toolAsk := fx.ask(t)
	out, err := fx.answer(t, toolAsk, "from the tool")
	require.NoError(t, err)
	require.NotEqual(t, deniedResult(toolAsk), out)

	used, err := fx.hitl.AgentAnswerCount(fx.ctx, fx.mission.ID)
	require.NoError(t, err)
	require.Equal(t, 2, used, "both paths count against one durable budget")

	// The bound is now spent for BOTH surfaces.
	thirdAsk := fx.ask(t)
	out, err = fx.answer(t, thirdAsk, "over the shared bound")
	require.NoError(t, err)
	require.Equal(t, deniedResult(thirdAsk), out, "mission_answer sees the CLI's spend")

	thirdRow, err := fx.store.GetHITLApproval(fx.ctx, thirdAsk)
	require.NoError(t, err)
	require.Error(t, hitlservice.EnforceAgentAnswerBounds(fx.ctx, fx.missions, fx.hitl, thirdRow),
		"the CLI path sees mission_answer's spend too")
}

// TestUnit_MissionAnswer_HumanPathIsUntouched pins that hardening the agent
// write changed nothing for a human: Answer still resolves an ask under an
// envelope that grants agents nothing.
func TestUnit_MissionAnswer_HumanPathIsUntouched(t *testing.T) {
	fx := newMissionAnswerFixture(t, `{"default_action":"deny","rules":[]}`)
	askID := fx.ask(t)

	require.NoError(t, fx.hitl.Answer(fx.ctx, askID, "the human decides"))

	row, err := fx.store.GetHITLApproval(fx.ctx, askID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, row.State)
	require.Equal(t, "the human decides", hitlservice.AnswerOf(row))
	require.Empty(t, hitlservice.AnsweredByOf(row), "a human answer records no actor")

	used, err := fx.hitl.AgentAnswerCount(fx.ctx, fx.mission.ID)
	require.NoError(t, err)
	require.Equal(t, 0, used, "a human answer never spends the agent budget")
}

// TestUnit_MissionAnswer_RefusesWithoutAStore pins the degraded wiring: no
// store means the envelope cannot be read, and an unreadable bound refuses
// rather than answering unbounded.
func TestUnit_MissionAnswer_RefusesWithoutAStore(t *testing.T) {
	fx := newMissionAnswerFixture(t, `{"default_action":"deny","rules":[],"attention":{"allowAgentAnswers":true}}`)
	askID := fx.ask(t)

	unwired := missionSupervision{missions: fx.missions, hitl: fx.hitl}
	err := unwired.AnswerAsAgent(fx.ctx, askID, "unbounded")
	require.Error(t, err)
	var refused *missiontools.AnswerRefusedError
	require.ErrorAs(t, err, &refused)

	row, err := fx.store.GetHITLApproval(fx.ctx, askID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State)
}
