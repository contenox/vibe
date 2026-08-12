package agentservice_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/execservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/require"
)

type sideEffectTools struct {
	path string
	mu   sync.Mutex
}

func (s *sideEffectTools) Exec(_ context.Context, _ time.Time, _ any, _ bool, args *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s\n", args.ToolName); err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	return "executed:" + args.ToolName, taskengine.DataTypeString, nil
}

func (s *sideEffectTools) Supports(context.Context) ([]string, error) { return []string{"gate"}, nil }

func (s *sideEffectTools) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}

func (s *sideEffectTools) GetToolsForToolsByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	if name != "gate" {
		return nil, taskengine.ErrToolsNotFound
	}
	return []taskengine.Tool{{Type: "function", Function: taskengine.FunctionTool{Name: "write"}}}, nil
}

func sideEffects(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	require.NoError(t, err)
	return len(strings.Fields(string(raw)))
}

type seInstance struct {
	db    libdb.DBManager
	store runtimetypes.Store
	hitl  hitlservice.Service
	deps  agentservice.Deps
	agent agentservice.Agent
}

func (i *seInstance) close() { _ = i.db.Close() }

func newSEInstance(t *testing.T, dbPath, effectPath string, withHook bool) *seInstance {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	store := runtimetypes.New(db.WithoutTransaction())

	hitl := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(t.TempDir()), "se-tenant", store, libtracker.NoopTracker{}, "")
	recorder, ok := hitl.(hitlservice.ApprovalRecorder)
	require.True(t, ok)

	sink := &recordingSink{}
	wrapper := localtools.NewHITLWrapper(&sideEffectTools{path: effectPath}, awayAsk,
		approveAllPolicy{ApprovalRecorder: recorder, Service: hitl}, libtracker.NoopTracker{}, sink)
	// short enough that the ask parks and the run checkpoints within the test
	wrapper.SetParkWindow(20 * time.Millisecond)

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
	deps := agentservice.Deps{Engine: engine, DB: db, Identity: "se"}
	inst := &seInstance{db: db, store: store, hitl: hitl, deps: deps, agent: agentservice.New(deps)}
	if withHook {
		hitlservice.SetResumeHook(hitl, agentservice.ResumeHook(deps))
	}
	return inst
}

func seChain() *taskengine.TaskChainDefinition { return e2eChain() }

func seChainFailingAfterTheTool() *taskengine.TaskChainDefinition {
	return &taskengine.TaskChainDefinition{
		ID:         "chain.se.boom",
		TokenLimit: 4096,
		Tasks: []taskengine.TaskDefinition{
			{
				ID:            "exec",
				Handler:       taskengine.HandleExecuteToolCalls,
				ExecuteConfig: &taskengine.LLMExecutionConfig{Model: "m", Tools: []string{"gate"}},
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpDefault, Goto: "boom"}},
				},
			},
			{
				ID:      "boom",
				Handler: taskengine.HandleRaiseError,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd}},
				},
			},
		},
	}
}

func suspendOne(t *testing.T, inst *seInstance, effectPath, sessionID string, chain *taskengine.TaskChainDefinition) {
	t.Helper()
	ctx := context.Background()
	resp, err := inst.agent.Prompt(ctx, agentservice.PromptRequest{
		SessionID:  sessionID,
		InputValue: e2eInput(),
		InputType:  taskengine.DataTypeChatHistory,
		Chain:      chain,
		ChainRef:   "se-chain.json",
	})
	require.NoError(t, err, "a suspension is a typed outcome, not an error")
	require.Equal(t, agentservice.StopSuspended, resp.StopReason)
	require.Equal(t, "call-w1", resp.SuspendedApprovalID)

	row, err := inst.store.GetHITLApproval(ctx, "call-w1")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State)
	_, err = inst.store.GetChainCheckpoint(ctx, "call-w1")
	require.NoError(t, err, "an unanswered ask must leave a durable checkpoint")
	require.Equal(t, 0, sideEffects(t, effectPath), "the gated call must not have run")
}

// TestSystem_Resume_ConcurrentResponds_ResolveOnceAndRunOnce pins that many terminals answering the same ask at once record exactly one verdict, every loser gets ErrApprovalAlreadyResolved, and the world is touched once.
func TestSystem_Resume_ConcurrentResponds_ResolveOnceAndRunOnce(t *testing.T) {
	dir := t.TempDir()
	dbPath, effectPath := filepath.Join(dir, "race.db"), filepath.Join(dir, "effects.log")
	ctx := context.Background()
	const sessionID = "sess-race"

	a := newSEInstance(t, dbPath, effectPath, true)
	createSession(t, a.db, sessionID)
	suspendOne(t, a, effectPath, sessionID, seChain())
	a.close()

	b := newSEInstance(t, dbPath, effectPath, true)
	defer b.close()

	const racers = 8
	start := make(chan struct{})
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = b.hitl.Respond(ctx, "call-w1", true)
		}(i)
	}
	close(start)
	wg.Wait()

	won, alreadyResolved := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, hitlservice.ErrApprovalAlreadyResolved):
			alreadyResolved++
		default:
			t.Fatalf("responder %d failed with something other than a clean already-answered: %v", i, err)
		}
	}
	require.Equal(t, 1, won, "exactly one verdict may be recorded")
	require.Equal(t, racers-1, alreadyResolved, "every loser gets the clean typed error, not a fault")

	require.Equal(t, 1, sideEffects(t, effectPath), "the resumed call must touch the world exactly once")

	_, err := b.store.GetChainCheckpoint(ctx, "call-w1")
	require.ErrorIs(t, err, libdb.ErrNotFound, "a completed resume deletes its checkpoint")
	msgs := loadSessionMessages(t, b.db, sessionID)
	results := 0
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == "call-w1" {
			results++
			require.Contains(t, m.Content, "executed:write")
		}
	}
	require.Equal(t, 1, results, "the tool result is persisted once")
}

// TestSystem_Resume_ConcurrentResumesOfOneVerdict_ClaimAdmitsExactlyOne pins that with the verdict already durable, the checkpoint claim alone (not the row CAS) admits exactly one resumer among N concurrent ones.
func TestSystem_Resume_ConcurrentResumesOfOneVerdict_ClaimAdmitsExactlyOne(t *testing.T) {
	dir := t.TempDir()
	dbPath, effectPath := filepath.Join(dir, "claim.db"), filepath.Join(dir, "effects.log")
	ctx := context.Background()
	const sessionID = "sess-claim"

	a := newSEInstance(t, dbPath, effectPath, false)
	createSession(t, a.db, sessionID)
	suspendOne(t, a, effectPath, sessionID, seChain())
	a.close()

	// the verdict is durable with nothing driving it, the state a responder that died right after its CAS leaves behind; written at the store since the service verb refuses this composition (ErrVerdictNeedsResumer)
	b := newSEInstance(t, dbPath, effectPath, false)
	defer b.close()
	require.NoError(t, b.store.ResolveHITLApproval(ctx, "call-w1", runtimetypes.HITLApprovalApproved, json.RawMessage(`{"approved":true}`), time.Now().UTC()))
	require.Equal(t, 0, sideEffects(t, effectPath), "a verdict alone must not run the work")

	const racers = 6
	start := make(chan struct{})
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = agentservice.ResumeFromCheckpoint(ctx, b.deps, "call-w1")
		}(i)
	}
	close(start)
	wg.Wait()

	resumed, refused := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			resumed++
		case errors.Is(err, agentservice.ErrNoCheckpoint):
			refused++
		default:
			t.Fatalf("resumer %d failed with something other than a clean no-checkpoint: %v", i, err)
		}
	}
	require.Equal(t, 1, resumed, "the checkpoint claim admits exactly one resumer")
	require.Equal(t, racers-1, refused)
	require.Equal(t, 1, sideEffects(t, effectPath), "N concurrent resumers, one execution")
}

// TestSystem_Resume_VerdictWithoutAResumerIsRefusedBeforeRecording pins that a process unable to resume is refused before the one-shot verdict is recorded, leaving the ask pending for a capable terminal to answer later.
func TestSystem_Resume_VerdictWithoutAResumerIsRefusedBeforeRecording(t *testing.T) {
	dir := t.TempDir()
	dbPath, effectPath := filepath.Join(dir, "strand.db"), filepath.Join(dir, "effects.log")
	ctx := context.Background()
	const sessionID = "sess-strand"

	a := newSEInstance(t, dbPath, effectPath, true)
	createSession(t, a.db, sessionID)
	suspendOne(t, a, effectPath, sessionID, seChain())
	a.close()

	blind := newSEInstance(t, dbPath, effectPath, false)
	require.ErrorIs(t, blind.hitl.Respond(ctx, "call-w1", true), hitlservice.ErrVerdictNeedsResumer,
		"an unresumable process must not spend the one-shot verdict")
	require.Equal(t, 0, sideEffects(t, effectPath))

	row, err := blind.store.GetHITLApproval(ctx, "call-w1")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State, "the ask is left pending, answerable elsewhere")
	cp, err := blind.store.GetChainCheckpoint(ctx, "call-w1")
	require.NoError(t, err, "the checkpoint is untouched")
	require.Nil(t, cp.ClaimedAt)
	require.Nil(t, cp.Failure)
	blind.close()

	capable := newSEInstance(t, dbPath, effectPath, true)
	defer capable.close()
	require.NoError(t, capable.hitl.Respond(ctx, "call-w1", true))
	require.Equal(t, 1, sideEffects(t, effectPath), "answer and resume, exactly once")
	_, err = capable.store.GetChainCheckpoint(ctx, "call-w1")
	require.ErrorIs(t, err, libdb.ErrNotFound, "a completed resume deletes its checkpoint")
}

// TestSystem_Resume_CrashedClaimIsReclaimedByTheStrandedSweep pins that a resumer dying mid-claim excludes everyone until the claim goes stale, after which SweepStrandedCheckpoints reclaims and completes the run exactly once.
func TestSystem_Resume_CrashedClaimIsReclaimedByTheStrandedSweep(t *testing.T) {
	dir := t.TempDir()
	dbPath, effectPath := filepath.Join(dir, "crash.db"), filepath.Join(dir, "effects.log")
	ctx := context.Background()
	const sessionID = "sess-crash"

	a := newSEInstance(t, dbPath, effectPath, false)
	createSession(t, a.db, sessionID)
	suspendOne(t, a, effectPath, sessionID, seChain())
	a.close()

	b := newSEInstance(t, dbPath, effectPath, false)
	defer b.close()
	// the crash, in two durable writes: a responder recorded the verdict, a resumer claimed the checkpoint, and neither came back
	require.NoError(t, b.store.ResolveHITLApproval(ctx, "call-w1", runtimetypes.HITLApprovalApproved, json.RawMessage(`{"approved":true}`), time.Now().UTC()))
	now := time.Now().UTC()
	require.NoError(t, b.store.ClaimChainCheckpoint(ctx, "call-w1", now, now.Add(-10*time.Minute)))

	resumed, failed, err := agentservice.SweepStrandedCheckpoints(ctx, b.deps, 100)
	require.NoError(t, err)
	require.Zero(t, resumed, "a live claim excludes the sweep")
	require.Zero(t, failed)
	require.Equal(t, 0, sideEffects(t, effectPath))

	stale := now.Add(-11 * time.Minute)
	require.NoError(t, b.store.ClaimChainCheckpoint(ctx, "call-w1", stale, now.Add(time.Minute)))

	resumed, failed, err = agentservice.SweepStrandedCheckpoints(ctx, b.deps, 100)
	require.NoError(t, err)
	require.Equal(t, 1, resumed, "the stale claim is reclaimed by the production sweep")
	require.Zero(t, failed)
	require.Equal(t, 1, sideEffects(t, effectPath))
	_, err = b.store.GetChainCheckpoint(ctx, "call-w1")
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

// TestSystem_Resume_PartiallyCompletedResumeReplaysItsRecordedResult pins that a resume recording its gated call's result before the chain continues lets a later retry replay that record instead of re-executing, keeping the side effect exactly-once.
func TestSystem_Resume_PartiallyCompletedResumeReplaysItsRecordedResult(t *testing.T) {
	dir := t.TempDir()
	dbPath, effectPath := filepath.Join(dir, "partial.db"), filepath.Join(dir, "effects.log")
	ctx := context.Background()
	const sessionID = "sess-partial"

	a := newSEInstance(t, dbPath, effectPath, false)
	createSession(t, a.db, sessionID)
	suspendOne(t, a, effectPath, sessionID, seChainFailingAfterTheTool())
	a.close()

	b := newSEInstance(t, dbPath, effectPath, false)
	defer b.close()
	require.NoError(t, b.store.ResolveHITLApproval(ctx, "call-w1", runtimetypes.HITLApprovalApproved, json.RawMessage(`{"approved":true}`), time.Now().UTC()))

	_, err := agentservice.ResumeFromCheckpoint(ctx, b.deps, "call-w1")
	require.Error(t, err, "the chain fails after the gated call succeeds")
	require.Equal(t, 1, sideEffects(t, effectPath), "the side effect DID happen")

	cp, err := b.store.GetChainCheckpoint(ctx, "call-w1")
	require.NoError(t, err, "a failed resume retains its checkpoint for retry")
	require.NotNil(t, cp.Failure)
	require.NotNil(t, cp.ClaimedAt)

	// once the claim goes stale, retry still fails (raise_error), but the gated call is replayed from its record, never run again
	stale := time.Now().UTC().Add(-11 * time.Minute)
	require.NoError(t, b.store.ClaimChainCheckpoint(ctx, "call-w1", stale, time.Now().UTC().Add(time.Minute)))
	_, err = agentservice.ResumeFromCheckpoint(ctx, b.deps, "call-w1")
	require.Error(t, err)
	require.Equal(t, 1, sideEffects(t, effectPath),
		"the retry replays the recorded result — the resume path is exactly-once for the gated call")

	stale = time.Now().UTC().Add(-11 * time.Minute)
	require.NoError(t, b.store.ClaimChainCheckpoint(ctx, "call-w1", stale, time.Now().UTC().Add(time.Minute)))
	resumed, failed, err := agentservice.SweepStrandedCheckpoints(ctx, b.deps, 100)
	require.NoError(t, err)
	require.Zero(t, resumed)
	require.Equal(t, 1, failed, "the sweep drives the retry and reports the chain's own failure")
	require.Equal(t, 1, sideEffects(t, effectPath), "still exactly once")
}
