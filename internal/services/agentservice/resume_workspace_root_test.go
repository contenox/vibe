package agentservice_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/execservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

type fsE2EInstance struct {
	db    libdb.DBManager
	store runtimetypes.Store
	hitl  hitlservice.Service
	deps  agentservice.Deps
	agent agentservice.Agent
}

func newFSE2EInstance(t *testing.T, dbPath string, cwdResolver func(context.Context) string) *fsE2EInstance {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	store := runtimetypes.New(db.WithoutTransaction())

	hitl := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(t.TempDir()), "e2e-tenant", store, libtracker.NoopTracker{}, "")
	recorder, ok := hitl.(hitlservice.ApprovalRecorder)
	require.True(t, ok)

	// db=nil degrades the read-before-write guard to a no-op; irrelevant here since write_file targets a file that does not exist yet
	inner := localtools.NewLocalFSToolsWith("", nil, hostFileIO{}, localtools.LocalFSToolsName, cwdResolver)
	wrapper := localtools.NewHITLWrapper(inner, awayAsk, approveAllPolicy{ApprovalRecorder: recorder, Service: hitl}, libtracker.NoopTracker{})

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

func TestSystem_S6Gate_ResumeAcrossProcesses_WritesUnderSessionWorkspaceNotResumerCwd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fs-walkaway.db")
	sessionWorkspace := t.TempDir()
	resumerWrongDir := t.TempDir()
	const relPath = "artifacts/contenox-walkaway-test.txt"
	const content = "hello from across the restart"
	ctx := context.Background()

	a := newFSE2EInstance(t, dbPath, func(context.Context) string { return sessionWorkspace })
	promptCtx := vfs.WithSessionCwd(ctx, sessionWorkspace)
	resp, err := a.agent.Prompt(detachedRun(promptCtx), agentservice.PromptRequest{
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

	b := newFSE2EInstance(t, dbPath, func(context.Context) string { return resumerWrongDir })
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

type hostFileIO struct{}

func (hostFileIO) ReadFile(_ context.Context, path string) ([]byte, error) { return os.ReadFile(path) }

func (hostFileIO) WriteFile(_ context.Context, path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}
