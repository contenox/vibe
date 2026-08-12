package contenoxcli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestUnit_ApprovalsCommandIsReserved(t *testing.T) {
	require.True(t, reservedSubcommands["approvals"], `"approvals" must be reserved so it dispatches as a subcommand`)
}

func TestUnit_RenderApprovalsTable(t *testing.T) {
	now := time.Now().UTC()

	var empty bytes.Buffer
	require.NoError(t, renderApprovalsTable(&empty, nil, now))
	require.Contains(t, empty.String(), "No pending asks", "an empty inbox renders empty, it does not fail")

	missionID := "m-42"
	diff := "-old\n+new"
	rows := []*runtimetypes.HITLApproval{
		{
			ID: "ask-1", ToolsName: "local_shell", ToolName: "exec", ArgsSummary: "rm -rf ./build",
			State:     runtimetypes.HITLApprovalPending,
			CreatedAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(28 * time.Minute),
			Diff: &diff,
		},
		{
			ID: "ask-2", ToolsName: hitlservice.AttentionToolsName, ToolName: hitlservice.AttentionToolName,
			ArgsSummary: "which project did you mean?", MissionID: &missionID,
			State:     runtimetypes.HITLApprovalPending,
			CreatedAt: now.Add(-30 * time.Second), ExpiresAt: now.Add(29 * time.Minute),
		},
	}
	var buf bytes.Buffer
	require.NoError(t, renderApprovalsTable(&buf, rows, now))
	out := buf.String()
	for _, want := range []string{
		"ID", "KIND", "TOOL", "SUMMARY", "MISSION", "AGE", "EXPIRES-IN",
		"ask-1", "permission", "local_shell.exec", "rm -rf ./build", "2m", "28m",
		"ask-2", "question", "which project did you mean?", "m-42",
		"-old\n+new",
		"approvals respond",
	} {
		require.Contains(t, out, want)
	}
}

// TestUnit_ApprovalsRespond_AsAgentFlagIsBetaGated pins the registration
// seam: without opt-in-beta the flag does not exist — absent from help and
// unparseable — exactly as Main resolves it; with the opt-in it registers.
func TestUnit_ApprovalsRespond_AsAgentFlagIsBetaGated(t *testing.T) {
	defer registerApprovalsRespondFlags(false)

	registerApprovalsRespondFlags(false)
	require.Nil(t, approvalsRespondCmd.Flags().Lookup(asAgentFlagName),
		"a stable user must not see or parse --as-agent")
	var help bytes.Buffer
	approvalsRespondCmd.SetOut(&help)
	require.NoError(t, approvalsRespondCmd.Help())
	require.NotContains(t, help.String(), "as-agent", "stable help must not mention the beta flag")

	registerApprovalsRespondFlags(true)
	require.NotNil(t, approvalsRespondCmd.Flags().Lookup(asAgentFlagName), "opt-in-beta registers the flag")
}

// respondFixture is one seeded database an approvals-respond invocation runs
// against: a mission under the named envelope and a pending attention ask
// attributed to it.
type respondFixture struct {
	dbPath    string
	policyDir string
	missionID string
	askID     string
}

// seedRespondFixture builds the fixture. envelope is the policy document the
// mission names; the file lands in policyDir, which doubles as --data-dir so
// hitlPolicySource resolves it first.
func seedRespondFixture(t *testing.T, envelope string) respondFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	policyDir := t.TempDir()
	const policyName = "hitl-policy-respond-test.json"
	require.NoError(t, os.WriteFile(filepath.Join(policyDir, policyName), []byte(envelope), 0o600))

	dbPath := filepath.Join(t.TempDir(), "asks.db")
	ctx := context.Background()
	db, err := OpenDBAt(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	missions := missionservice.New(db)
	mission := &missionservice.Mission{Intent: "refresh the docs index", AgentName: "docs-unit", HITLPolicyName: policyName}
	require.NoError(t, missions.Create(ctx, mission))

	store := runtimetypes.New(db.WithoutTransaction())
	askID := uuid.NewString()
	missionID := mission.ID
	now := time.Now().UTC()
	require.NoError(t, store.CreateHITLApproval(ctx, &runtimetypes.HITLApproval{
		ID:          askID,
		ToolsName:   hitlservice.AttentionToolsName,
		ToolName:    hitlservice.AttentionToolName,
		ArgsSummary: "which directory holds the docs?",
		OnTimeout:   string(hitlservice.ActionDeny),
		State:       runtimetypes.HITLApprovalPending,
		MissionID:   &missionID,
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
	}))
	return respondFixture{dbPath: dbPath, policyDir: policyDir, missionID: missionID, askID: askID}
}

// runRespond executes the real runApprovalsRespond on an isolated root with
// the beta flag set registered, mirroring Main under opt-in-beta.
func runRespond(t *testing.T, fx respondFixture, extra ...string) (string, error) {
	t.Helper()
	respond := &cobra.Command{Use: "respond", Args: cobra.ExactArgs(1), RunE: runApprovalsRespond, SilenceUsage: true, SilenceErrors: true}
	respond.Flags().Bool("approve", false, "")
	respond.Flags().Bool("deny", false, "")
	respond.Flags().String("answer", "", "")
	respond.Flags().String(asAgentFlagName, "", "")
	root := &cobra.Command{Use: "contenox", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("db", "", "")
	root.PersistentFlags().String("data-dir", "", "")
	root.AddCommand(respond)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"--db", fx.dbPath, "--data-dir", fx.policyDir, "respond"}, extra...))
	err := root.Execute()
	return out.String(), err
}

// readAsk re-opens the fixture database and returns the ask row.
func readAsk(t *testing.T, fx respondFixture) *runtimetypes.HITLApproval {
	t.Helper()
	ctx := context.Background()
	db, err := OpenDBAt(ctx, fx.dbPath)
	require.NoError(t, err)
	defer db.Close()
	row, err := runtimetypes.New(db.WithoutTransaction()).GetHITLApproval(ctx, fx.askID)
	require.NoError(t, err)
	return row
}

const grantingEnvelope = `{"default_action":"deny","rules":[],"attention":{"allowAgentAnswers":true,"maxAgentAnswers":1}}`
const humanOnlyEnvelope = `{"default_action":"deny","rules":[]}`

// TestUnit_ApprovalsRespond_AsAgentRecordsAttributionWithinBounds pins the
// happy path: the envelope grants, the answer lands agent-attributed with the
// agent's name, and it counts against the mission's bound.
func TestUnit_ApprovalsRespond_AsAgentRecordsAttributionWithinBounds(t *testing.T) {
	fx := seedRespondFixture(t, grantingEnvelope)

	out, err := runRespond(t, fx, fx.askID, "--answer", "docs/ at the repo root", "--as-agent", "attention-reviewer")
	require.NoError(t, err)
	require.Contains(t, out, `Answered as agent "attention-reviewer"`)

	row := readAsk(t, fx)
	require.Equal(t, runtimetypes.HITLApprovalApproved, row.State)
	require.Equal(t, "docs/ at the repo root", hitlservice.AnswerOf(row))
	require.Equal(t, "attention-reviewer", hitlservice.AnsweredByOf(row),
		"the durable record must show the agent answered, by name")
}

// TestUnit_ApprovalsRespond_AsAgentRefusedWhenEnvelopeForbids pins the exact
// refusal when the mission's envelope has no attention grant. The envelope
// always wins; the ask stays pending for a human.
func TestUnit_ApprovalsRespond_AsAgentRefusedWhenEnvelopeForbids(t *testing.T) {
	fx := seedRespondFixture(t, humanOnlyEnvelope)

	_, err := runRespond(t, fx, fx.askID, "--answer", "docs/", "--as-agent", "attention-reviewer")
	require.Error(t, err)
	require.Equal(t,
		`agent answer refused: envelope "hitl-policy-respond-test.json" of mission `+fx.missionID+` does not allow agent answers (no attention.allowAgentAnswers grant); a human must answer`,
		err.Error())
	require.Equal(t, runtimetypes.HITLApprovalPending, readAsk(t, fx).State, "a refused agent answer must leave the question waiting")
}

// TestUnit_ApprovalsRespond_AsAgentRefusedWhenBoundSpent pins the exact
// refusal once the mission's agent-answer budget is spent, counted from the
// durable records.
func TestUnit_ApprovalsRespond_AsAgentRefusedWhenBoundSpent(t *testing.T) {
	fx := seedRespondFixture(t, grantingEnvelope)

	// Spend the bound (maxAgentAnswers: 1) on a first agent-answered ask.
	ctx := context.Background()
	db, err := OpenDBAt(ctx, fx.dbPath)
	require.NoError(t, err)
	store := runtimetypes.New(db.WithoutTransaction())
	first := uuid.NewString()
	now := time.Now().UTC()
	missionID := fx.missionID
	require.NoError(t, store.CreateHITLApproval(ctx, &runtimetypes.HITLApproval{
		ID: first, ToolsName: hitlservice.AttentionToolsName, ToolName: hitlservice.AttentionToolName,
		ArgsSummary: "first question", OnTimeout: string(hitlservice.ActionDeny),
		State: runtimetypes.HITLApprovalPending, MissionID: &missionID,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}))
	svc := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(fx.policyDir), runtimetypes.LocalTenantID, store, nil, "")
	require.NoError(t, svc.AnswerAsAgentNamed(ctx, first, "attention-reviewer", "answered once"))
	require.NoError(t, db.Close())

	_, err = runRespond(t, fx, fx.askID, "--answer", "docs/", "--as-agent", "attention-reviewer")
	require.Error(t, err)
	require.Equal(t,
		`agent answer refused: mission `+fx.missionID+` spent its agent-answer bound (1 of 1); this question waits for a human`,
		err.Error())
	require.Equal(t, runtimetypes.HITLApprovalPending, readAsk(t, fx).State)
}

// TestUnit_ApprovalsRespond_AsAgentRequiresAnswer pins that the flag never
// rides a permission verdict.
func TestUnit_ApprovalsRespond_AsAgentRequiresAnswer(t *testing.T) {
	fx := seedRespondFixture(t, grantingEnvelope)
	_, err := runRespond(t, fx, fx.askID, "--approve", "--as-agent", "attention-reviewer")
	require.Error(t, err)
	require.Equal(t, `--as-agent attributes a question's answer; pair it with --answer "..."`, err.Error())
}

// TestUnit_ApprovalsRespond_HumanPathUnchanged pins that a plain --answer
// records a human answer — no actor, no bounds consulted — even under an
// envelope that forbids agent answers.
func TestUnit_ApprovalsRespond_HumanPathUnchanged(t *testing.T) {
	fx := seedRespondFixture(t, humanOnlyEnvelope)

	out, err := runRespond(t, fx, fx.askID, "--answer", "docs/ at the repo root")
	require.NoError(t, err)
	require.NotContains(t, out, "Answered as agent")
	require.Contains(t, out, "Verdict recorded for "+fx.askID)

	row := readAsk(t, fx)
	require.Equal(t, runtimetypes.HITLApprovalApproved, row.State)
	require.Equal(t, "docs/ at the repo root", hitlservice.AnswerOf(row))
	require.Empty(t, hitlservice.AnsweredByOf(row), "a human answer records no non-human actor")
}
