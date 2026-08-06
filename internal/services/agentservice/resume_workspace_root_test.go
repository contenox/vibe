package agentservice_test

// A run that parks on a human approval and is later resumed must resolve a
// gated tool's relative path against the SESSION's own workspace root, never
// against whichever process happens to answer the approval. This reproduces
// the walkaway bug end to end: instance "a" is the live session (its
// cwdResolver stands in for the ACP transport reporting the VS Code
// workspace); instance "b" is a completely different process — a different
// cwdResolver standing in for e.g. `contenox approvals respond` run from an
// unrelated shell — answering the same durable approval after "a" is gone.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/execservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// fsE2EInstance is newE2EInstance's local_fs twin: the gated tool is the real
// LocalFSTools (allowedDir "", exactly how the ACP toolset builds it — see
// acp_toolset.go), so this test exercises the actual path-resolution code a
// resumed write_file runs through, not a stand-in.
type fsE2EInstance struct {
	db    libdb.DBManager
	store runtimetypes.Store
	hitl  hitlservice.Service
	deps  agentservice.Deps
	agent agentservice.Agent
}

func newFSE2EInstance(t *testing.T, dbPath string, parkWindow time.Duration, cwdResolver func(context.Context) string) *fsE2EInstance {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	store := runtimetypes.New(db.WithoutTransaction())

	hitl := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(t.TempDir()), "e2e-tenant", store, libtracker.NoopTracker{}, "")
	recorder, ok := hitl.(hitlservice.ApprovalRecorder)
	require.True(t, ok)

	// db=nil: no session, so the read-before-write guard degrades to a no-op
	// (fs.go's documented behaviour) — irrelevant here since write_file
	// targets a file that does not exist yet.
	inner := localtools.NewLocalFSToolsWith("", nil, nil, localtools.LocalFSToolsName, cwdResolver)
	wrapper := localtools.NewHITLWrapper(inner, awayAsk, approveAllPolicy{ApprovalRecorder: recorder, Service: hitl}, libtracker.NoopTracker{})
	wrapper.SetParkWindow(parkWindow)

	cctx := ctx
	exec, err := taskengine.NewExec(cctx, stubModelRepo{}, wrapper, libtracker.NoopTracker{})
	require.NoError(t, err)
	env, err := taskengine.NewEnv(cctx, libtracker.NoopTracker{}, exec, taskengine.NewSimpleInspector(), wrapper)
	require.NoError(t, err)

	engine := &enginesvc.Engine{
		TaskService: execservice.NewTasksEnv(ctx, env, wrapper),
		Tracker:     libtracker.NoopTracker{},
	}
	deps := agentservice.Deps{Engine: engine, DB: db, WorkspaceID: "", Identity: "e2e"}
	inst := &fsE2EInstance{db: db, store: store, hitl: hitl, deps: deps, agent: agentservice.New(deps)}
	hitlservice.SetResumeHook(hitl, agentservice.ResumeHook(deps))
	return inst
}

func (i *fsE2EInstance) close() { _ = i.db.Close() }

func fsE2EChain() *taskengine.TaskChainDefinition {
	return &taskengine.TaskChainDefinition{
		ID:         "chain.fs-e2e",
		TokenLimit: 4096,
		Tasks: []taskengine.TaskDefinition{
			{
				ID:            "exec",
				Handler:       taskengine.HandleExecuteToolCalls,
				ExecuteConfig: &taskengine.LLMExecutionConfig{Model: "m", Tools: []string{"local_fs"}},
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd}},
				},
			},
		},
	}
}

// fsE2EInput asks for write_file on a RELATIVE path — the exact shape of the
// bug's reproduction (packages/vscode/scripts/run-walkaway-acceptance.sh):
// artifacts/<file>, never an absolute path.
func fsE2EInput(t *testing.T, relPath, content string) taskengine.ChatHistory {
	t.Helper()
	args, err := json.Marshal(map[string]string{"path": relPath, "content": content})
	require.NoError(t, err)
	return taskengine.ChatHistory{Messages: []taskengine.Message{
		{ID: "m-user", Role: "user", Content: "write the file", Timestamp: time.Now().UTC()},
		{ID: "m-asst", Role: "assistant", Timestamp: time.Now().UTC(), CallTools: []taskengine.ToolCall{
			{ID: "call-fs1", Type: "function", Function: taskengine.FunctionCall{Name: "local_fs.write_file", Arguments: string(args)}},
		}},
	}}
}

// TestSystem_S6Gate_ResumeAcrossProcesses_WritesUnderSessionWorkspaceNotResumerCwd
// is the walkaway regression: a write_file call gated past the fast window
// suspends and checkpoints; the verdict lands in a DIFFERENT process (a
// different cwdResolver, standing in for a different working directory) —
// the resumed write must still land under the ORIGINAL session's workspace
// root, never under whatever the resuming process's own cwdResolver reports.
func TestSystem_S6Gate_ResumeAcrossProcesses_WritesUnderSessionWorkspaceNotResumerCwd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fs-walkaway.db")
	sessionWorkspace := t.TempDir()
	resumerWrongDir := t.TempDir()
	const relPath = "artifacts/contenox-walkaway-test.txt"
	const content = "hello from across the restart"
	ctx := context.Background()

	// "a" is the live session: its cwdResolver reports the session's own
	// workspace, exactly as acpsvc.NewACPCwdResolver does for a live ACP
	// connection. The Prompt call carries the session's cwd on ctx too (see
	// prompt.go / native_turn.go / chat_cmd.go), which is what the checkpoint
	// persists.
	a := newFSE2EInstance(t, dbPath, 20*time.Millisecond, func(context.Context) string { return sessionWorkspace })
	promptCtx := vfs.WithSessionCwd(ctx, sessionWorkspace)
	resp, err := a.agent.Prompt(promptCtx, agentservice.PromptRequest{
		InputValue: fsE2EInput(t, relPath, content),
		InputType:  taskengine.DataTypeChatHistory,
		Chain:      fsE2EChain(),
		ChainRef:   "fs-e2e-chain.json",
	})
	require.NoError(t, err, "a suspension is a typed outcome, not an error")
	require.Equal(t, agentservice.StopSuspended, resp.StopReason)
	require.Equal(t, "call-fs1", resp.SuspendedApprovalID)

	require.NoFileExists(t, filepath.Join(sessionWorkspace, relPath), "must not exist before the verdict lands")
	a.close()

	// "b" is a different process entirely: its own cwdResolver reports a
	// DIFFERENT directory (standing in for e.g. a shell in
	// packages/vscode answering `contenox approvals respond`), and it never
	// put the session's workspace on ctx — exactly the CLI resume path
	// (approvals_cmd.go / engine.go), which builds local_fs with no
	// cwdResolver at all in the real code; a live one here only makes this
	// assertion strictly harder to satisfy by accident.
	b := newFSE2EInstance(t, dbPath, 20*time.Millisecond, func(context.Context) string { return resumerWrongDir })
	defer b.close()

	require.NoError(t, b.hitl.Respond(ctx, "call-fs1", true),
		"Respond runs the resume synchronously via the registered hook")

	require.NoFileExists(t, filepath.Join(resumerWrongDir, relPath),
		"the walkaway bug: a resumed relative write must never land under the RESUMER's directory")
	wantPath := filepath.Join(sessionWorkspace, relPath)
	require.FileExists(t, wantPath, "the resumed write must land under the SESSION's own workspace root")
	got, err := os.ReadFile(wantPath)
	require.NoError(t, err)
	require.Equal(t, content, string(got))

	_, err = b.store.GetChainCheckpoint(ctx, "call-fs1")
	require.ErrorIs(t, err, libdb.ErrNotFound, "a successful terminal deletes the checkpoint")
}
