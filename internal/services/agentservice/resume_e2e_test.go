package agentservice_test

// A chain suspends past the fast window on a gated call, and a verdict
// delivered to a completely fresh instance (same on-disk database) completes
// it end to end, exactly once.

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/models/llmrepo"
	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/chatservice"
	"github.com/contenox/contenox/internal/services/execservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/messagestore"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/require"
)

// stubModelRepo satisfies llmrepo.ModelRepo for chains that never reach a model.
type stubModelRepo struct{}

func (stubModelRepo) Tokenize(context.Context, string, string) ([]int, error) { return []int{1}, nil }
func (stubModelRepo) CountTokens(context.Context, string, string) (int, error) {
	return 1, nil
}
func (stubModelRepo) PromptExecute(context.Context, llmrepo.Request, string, float32, string) (string, llmrepo.Meta, error) {
	return "", llmrepo.Meta{}, errors.New("stub: no model")
}
func (stubModelRepo) Chat(context.Context, llmrepo.Request, []libmodelprovider.Message, ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
	return libmodelprovider.ChatResult{}, llmrepo.Meta{}, errors.New("stub: no model")
}
func (stubModelRepo) Embed(context.Context, llmrepo.EmbedRequest, string) ([]float64, llmrepo.Meta, error) {
	return nil, llmrepo.Meta{}, errors.New("stub: no model")
}
func (stubModelRepo) Stream(context.Context, llmrepo.Request, []libmodelprovider.Message, ...libmodelprovider.ChatArgument) (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error) {
	return nil, llmrepo.Meta{}, errors.New("stub: no model")
}

// e2eInnerTools is the raw tool provider under the HITL wrapper.
type e2eInnerTools struct {
	mu    sync.Mutex
	execs []string
}

func (e *e2eInnerTools) Exec(_ context.Context, _ time.Time, _ any, _ bool, args *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	e.mu.Lock()
	e.execs = append(e.execs, args.ToolName)
	e.mu.Unlock()
	return "executed:" + args.ToolName, taskengine.DataTypeString, nil
}

func (e *e2eInnerTools) Supports(context.Context) ([]string, error) { return []string{"gate"}, nil }
func (e *e2eInnerTools) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}
func (e *e2eInnerTools) GetToolsForToolsByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	if name != "gate" {
		return nil, taskengine.ErrToolsNotFound
	}
	return []taskengine.Tool{{Type: "function", Function: taskengine.FunctionTool{Name: "write"}}}, nil
}

// approveAllPolicy gates every call and delegates the durable recorder half
// (RecordPendingApproval/ResolveApprovalInline, via the embedded
// ApprovalRecorder) and the late-answer half (Respond, via Service) to the
// same real hitlservice instance.
type approveAllPolicy struct {
	hitlservice.ApprovalRecorder
	Service hitlservice.Service
}

func (approveAllPolicy) Evaluate(context.Context, string, string, map[string]any) (hitlservice.EvaluationResult, error) {
	return hitlservice.EvaluationResult{Action: hitlservice.ActionApprove}, nil
}

// Respond satisfies localtools' approvalResponder so a verdict delivered
// after the park window (through the SAME ask() callback, not a separate
// out-of-process call) still resumes the checkpointed run.
func (p approveAllPolicy) Respond(ctx context.Context, approvalID string, approved bool) error {
	return p.Service.Respond(ctx, approvalID, approved)
}

type recordingSink struct {
	mu     sync.Mutex
	events []taskengine.TaskEvent
}

func (s *recordingSink) PublishTaskEvent(_ context.Context, ev taskengine.TaskEvent) error {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
	return nil
}
func (s *recordingSink) Wants(taskengine.TaskEventKind) bool { return true }

func (s *recordingSink) kinds() []taskengine.TaskEventKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]taskengine.TaskEventKind, 0, len(s.events))
	for _, ev := range s.events {
		out = append(out, ev.Kind)
	}
	return out
}

// e2eInstance is one "process": engine + hitlservice + agent over a shared DB file.
type e2eInstance struct {
	db    libdb.DBManager
	store runtimetypes.Store
	hitl  hitlservice.Service
	deps  agentservice.Deps
	agent agentservice.Agent
	inner *e2eInnerTools
	sink  *recordingSink
}

// awayAsk simulates a human who never answers within this process's
// lifetime: the ask blocks only until its context ends, then reports that
// context error. It forces every verdict through Respond, as if delivered by
// a restarted process or a separate `contenox approvals answer` invocation —
// the shape newE2EInstance's original callers exercise.
func awayAsk(ctx context.Context, _ hitlservice.ApprovalRequest) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

func newE2EInstance(t *testing.T, dbPath string, parkWindow time.Duration, ask localtools.AskApproval) *e2eInstance {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	store := runtimetypes.New(db.WithoutTransaction())

	hitl := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(t.TempDir()), "e2e-tenant", store, libtracker.NoopTracker{}, "")
	recorder, ok := hitl.(hitlservice.ApprovalRecorder)
	require.True(t, ok)

	sink := &recordingSink{}
	inner := &e2eInnerTools{}
	wrapper := localtools.NewHITLWrapper(inner, ask, approveAllPolicy{ApprovalRecorder: recorder, Service: hitl}, libtracker.NoopTracker{}, sink)
	wrapper.SetParkWindow(parkWindow)

	cctx := taskengine.WithTaskEventSink(ctx, sink)
	exec, err := taskengine.NewExec(cctx, stubModelRepo{}, wrapper, libtracker.NoopTracker{})
	require.NoError(t, err)
	env, err := taskengine.NewEnv(cctx, libtracker.NoopTracker{}, exec, taskengine.NewSimpleInspector(), wrapper)
	require.NoError(t, err)

	engine := &enginesvc.Engine{
		TaskService:   execservice.NewTasksEnv(ctx, env, wrapper),
		Tracker:       libtracker.NoopTracker{},
		TaskEventSink: sink,
	}
	deps := agentservice.Deps{Engine: engine, DB: db, WorkspaceID: "", Identity: "e2e"}
	inst := &e2eInstance{
		db:    db,
		store: store,
		hitl:  hitl,
		deps:  deps,
		agent: agentservice.New(deps),
		inner: inner,
		sink:  sink,
	}
	// A verdict landing with no waiter resumes the suspended run here.
	hitlservice.SetResumeHook(hitl, agentservice.ResumeHook(deps))
	return inst
}

func (i *e2eInstance) close() { _ = i.db.Close() }

func e2eChain() *taskengine.TaskChainDefinition {
	return &taskengine.TaskChainDefinition{
		ID:         "chain.e2e",
		TokenLimit: 4096, // a zero budget would cap every tool result to the too-large stub
		Tasks: []taskengine.TaskDefinition{
			{
				ID:            "exec",
				Handler:       taskengine.HandleExecuteToolCalls,
				ExecuteConfig: &taskengine.LLMExecutionConfig{Model: "m", Tools: []string{"gate"}},
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd}},
				},
			},
		},
	}
}

func e2eInput() taskengine.ChatHistory {
	return taskengine.ChatHistory{Messages: []taskengine.Message{
		{ID: "m-user", Role: "user", Content: "write the file", Timestamp: time.Now().UTC()},
		{ID: "m-asst", Role: "assistant", Timestamp: time.Now().UTC(), CallTools: []taskengine.ToolCall{
			{ID: "call-w1", Type: "function", Function: taskengine.FunctionCall{Name: "gate.write", Arguments: `{"path":"/tmp/e2e"}`}},
		}},
	}}
}

// createSession registers the session's message index, as sessionservice's SessionNew does.
func createSession(t *testing.T, db libdb.DBManager, sessionID string) {
	t.Helper()
	require.NoError(t, messagestore.New(db.WithoutTransaction(), "").CreateMessageIndex(context.Background(), sessionID, "e2e"))
}

func loadSessionMessages(t *testing.T, db libdb.DBManager, sessionID string) []taskengine.Message {
	t.Helper()
	msgs, err := chatservice.NewManager("").ListMessages(context.Background(), db.WithoutTransaction(), sessionID)
	require.NoError(t, err)
	return msgs
}

func TestSystem_S6Gate_ApprovalOutlivesEngine_VerdictAfterRestartCompletesChain(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "s6-gate.db")
	ctx := context.Background()
	const sessionID = "sess-e2e"

	a := newE2EInstance(t, dbPath, 20*time.Millisecond, awayAsk)
	createSession(t, a.db, sessionID)
	resp, err := a.agent.Prompt(ctx, agentservice.PromptRequest{
		SessionID:  sessionID,
		InputValue: e2eInput(),
		InputType:  taskengine.DataTypeChatHistory,
		Chain:      e2eChain(),
		ChainRef:   "e2e-chain.json",
	})
	require.NoError(t, err, "a suspension is a typed outcome, not an error")
	require.Equal(t, agentservice.StopSuspended, resp.StopReason)
	require.Equal(t, "call-w1", resp.SuspendedApprovalID)
	require.Empty(t, a.inner.execs, "the gated tool must not have run")

	row, err := a.store.GetHITLApproval(ctx, "call-w1")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State)
	cpRow, err := a.store.GetChainCheckpoint(ctx, "call-w1")
	require.NoError(t, err)
	require.Equal(t, sessionID, cpRow.SessionID)

	require.Empty(t, loadSessionMessages(t, a.db, sessionID))

	kindsA := a.sink.kinds()
	require.Equal(t, taskengine.TaskEventChainSuspended, kindsA[len(kindsA)-1])

	a.close()

	b := newE2EInstance(t, dbPath, 20*time.Millisecond, awayAsk)
	defer b.close()

	require.NoError(t, b.hitl.Respond(ctx, "call-w1", true),
		"Respond runs the resume synchronously via the registered hook")

	require.Equal(t, []string{"write"}, b.inner.execs)

	kindsB := b.sink.kinds()
	require.Equal(t, taskengine.TaskEventChainCompleted, kindsB[len(kindsB)-1])

	_, err = b.store.GetChainCheckpoint(ctx, "call-w1")
	require.ErrorIs(t, err, libdb.ErrNotFound, "a successful terminal deletes the checkpoint")
	row, err = b.store.GetHITLApproval(ctx, "call-w1")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, row.State)

	// History must persist once: no stubs, no duplicates.
	msgs := loadSessionMessages(t, b.db, sessionID)
	var roles []string
	resultCount := map[string]int{}
	seenIDs := map[string]int{}
	for _, m := range msgs {
		roles = append(roles, m.Role)
		seenIDs[m.ID]++
		if m.Role == "tool" {
			resultCount[m.ToolCallID]++
			require.Contains(t, m.Content, "executed:write", "the REAL result, not an interruption stub")
		}
	}
	require.Equal(t, []string{"user", "assistant", "tool"}, roles)
	require.Equal(t, map[string]int{"call-w1": 1}, resultCount)
	for id, n := range seenIDs {
		require.Equal(t, 1, n, "message %s persisted more than once", id)
	}

	require.ErrorIs(t, b.hitl.Respond(ctx, "call-w1", true), hitlservice.ErrApprovalAlreadyResolved)
	_, err = agentservice.ResumeFromCheckpoint(ctx, b.deps, "call-w1")
	require.ErrorIs(t, err, agentservice.ErrNoCheckpoint)
}

func TestSystem_S6Gate_DenyAfterRestart_CompletesWithDenySemantics(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "s6-gate-deny.db")
	ctx := context.Background()
	const sessionID = "sess-deny"

	a := newE2EInstance(t, dbPath, 20*time.Millisecond, awayAsk)
	createSession(t, a.db, sessionID)
	resp, err := a.agent.Prompt(ctx, agentservice.PromptRequest{
		SessionID:  sessionID,
		InputValue: e2eInput(),
		InputType:  taskengine.DataTypeChatHistory,
		Chain:      e2eChain(),
	})
	require.NoError(t, err)
	require.Equal(t, agentservice.StopSuspended, resp.StopReason)
	a.close()

	b := newE2EInstance(t, dbPath, 20*time.Millisecond, awayAsk)
	defer b.close()
	require.NoError(t, b.hitl.Respond(ctx, "call-w1", false))

	// Deny completes the chain with the standard deny message as the result.
	require.Empty(t, b.inner.execs, "a denied call must never execute")
	msgs := loadSessionMessages(t, b.db, sessionID)
	var toolResults []string
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == "call-w1" {
			toolResults = append(toolResults, m.Content)
		}
	}
	require.Len(t, toolResults, 1)
	require.Contains(t, toolResults[0], localtools.DenyMessage)

	_, err = b.store.GetChainCheckpoint(ctx, "call-w1")
	require.ErrorIs(t, err, libdb.ErrNotFound)
}
