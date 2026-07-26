// Integration tests for the persistence pipeline: ExecEnv → SynthesizeHistory
// → PersistDiff → ListMessages. Each function in this pipeline is unit-tested
// in isolation, but the bugs we hit today (success-path execution-history drop;
// PersistDiff within-batch dedup gap) lived at the *joins* between units. The
// tests here run a real chain through the real env, hand its output to the
// real synthesizer, persist via the real PersistDiff to a real SQLite DB, and
// verify the rows that land match what the user expects.
//
// Adding a new shape of bug? Add a test case here. The cost of one round-trip
// test is much lower than another forensic-debugging session.
package chatservice_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/kernel/tools"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/chatservice"
	"github.com/contenox/beam/internal/services/messagestore"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// ── test scaffolding ────────────────────────────────────────────────────────

func setupDB(t *testing.T) (context.Context, libdb.DBManager) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "integ.db")
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db
}

// chatExecutor mimics the real chat_completion handler's wire shape: it
// prepends a system_instruction message at index 0, copies the input messages,
// and appends an assistant message. This is the *real* shape SynthesizeHistory
// must handle — and the shape that exposed the within-batch-dedup bug because
// outHist[startIdx:] re-includes the last prior message.
//
// All timestamps come from a monotonic counter rather than time.Now() so tests
// have deterministic chronological ordering (ListMessages orders by added_at)
// and replay-idempotency tests get stable hashes.
type chatExecutor struct {
	systemInstruction string
	assistantContent  string
	assistantToolCall *taskengine.ToolCall
	err               error

	// Fixed timestamps for deterministic outputs. systemTime is used for the
	// prepended system message; assistantTime for the appended assistant
	// message. Both must be set or both unset.
	systemTime    time.Time
	assistantTime time.Time
}

func (c *chatExecutor) TaskExec(_ context.Context, _ time.Time, _ int, _ *taskengine.ChainContext, _ *taskengine.TaskDefinition, input any, _ taskengine.DataType) (any, taskengine.DataType, string, error) {
	if c.err != nil {
		return nil, taskengine.DataTypeAny, "", c.err
	}
	in, ok := input.(taskengine.ChatHistory)
	if !ok {
		return nil, taskengine.DataTypeAny, "", errors.New("chatExecutor: input must be ChatHistory")
	}
	systemTime := c.systemTime
	if systemTime.IsZero() {
		systemTime = time.Now().UTC()
	}
	assistantTime := c.assistantTime
	if assistantTime.IsZero() {
		assistantTime = time.Now().UTC()
	}
	msgs := make([]taskengine.Message, 0, len(in.Messages)+2)
	if c.systemInstruction != "" {
		msgs = append(msgs, taskengine.Message{Role: "system", Content: c.systemInstruction, Timestamp: systemTime})
	}
	msgs = append(msgs, in.Messages...)
	assistant := taskengine.Message{Role: "assistant", Content: c.assistantContent, Timestamp: assistantTime}
	if c.assistantToolCall != nil {
		assistant.CallTools = []taskengine.ToolCall{*c.assistantToolCall}
	}
	msgs = append(msgs, assistant)
	transition := "executed"
	if c.assistantToolCall != nil {
		transition = "tool_call"
	}
	return taskengine.ChatHistory{Messages: msgs}, taskengine.DataTypeChatHistory, transition, nil
}

// fixedClock returns successive Unix-second timestamps starting at base, so
// every loadContents/loadRoles call sees a strict chronological order.
type fixedClock struct {
	base    time.Time
	stepIdx int
}

func newFixedClock() *fixedClock { return &fixedClock{base: time.Unix(1_700_000_000, 0).UTC()} }

func (f *fixedClock) Next() time.Time {
	f.stepIdx++
	return f.base.Add(time.Duration(f.stepIdx) * time.Second)
}

func chatChain() *taskengine.TaskChainDefinition {
	return &taskengine.TaskChainDefinition{
		ID: "chat",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:                "chat",
				Handler:           taskengine.HandleChatCompletion,
				SystemInstruction: "you are a helpful assistant",
				ExecuteConfig:     &taskengine.LLMExecutionConfig{Model: "test"},
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}
}

func roundTrip(t *testing.T, ctx context.Context, db libdb.DBManager, sessionID string, exec taskengine.TaskExecutor, chainInput taskengine.ChatHistory) {
	t.Helper()
	env, err := taskengine.NewEnv(ctx, libtracker.NoopTracker{}, exec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	_, _, units, runErr := env.ExecEnv(ctx, chatChain(), chainInput, taskengine.DataTypeChatHistory)

	// Regression: execution history must be returned on the success path.
	// taskenv.go:509 previously returned nil, silently dropping the assistant
	// message from the persisted transcript even when the chain succeeded.
	require.NotNil(t, units, "ExecEnv must return execution history (success or error)")

	synthesized := taskengine.SynthesizeHistory(chainInput.Messages, units, runErr)

	mgr := chatservice.NewManager("")
	// Regression: PersistDiff must succeed even when synthesized output
	// contains overlapping prefixes (the synthesizer's documented contract).
	// Earlier it tripped a UNIQUE constraint because dedup only checked
	// against DB-stored rows, not within the new batch.
	require.NoError(t, mgr.PersistDiff(ctx, db.WithoutTransaction(), sessionID, synthesized))
}

func loadRoles(t *testing.T, ctx context.Context, db libdb.DBManager, sessionID string) []string {
	t.Helper()
	store := messagestore.New(db.WithoutTransaction(), "")
	rows, err := store.ListMessages(ctx, sessionID)
	require.NoError(t, err)
	roles := make([]string, 0, len(rows))
	for _, r := range rows {
		var m taskengine.Message
		require.NoError(t, json.Unmarshal(r.Payload, &m))
		roles = append(roles, m.Role)
	}
	return roles
}

func loadContents(t *testing.T, ctx context.Context, db libdb.DBManager, sessionID string) []string {
	t.Helper()
	store := messagestore.New(db.WithoutTransaction(), "")
	rows, err := store.ListMessages(ctx, sessionID)
	require.NoError(t, err)
	contents := make([]string, 0, len(rows))
	for _, r := range rows {
		var m taskengine.Message
		require.NoError(t, json.Unmarshal(r.Payload, &m))
		contents = append(contents, m.Content)
	}
	return contents
}

// ── tests ───────────────────────────────────────────────────────────────────

// Round-trip a single chat turn against an empty session. This is the
// canonical regression for the two bugs we hit today.
func TestIntegration_ChatRoundTrip_FreshSession(t *testing.T) {
	ctx, db := setupDB(t)
	store := messagestore.New(db.WithoutTransaction(), "")
	require.NoError(t, store.CreateMessageIndex(ctx, "s-fresh", "alice"))

	chainInput := taskengine.ChatHistory{
		Messages: []taskengine.Message{
			{Role: "user", Content: "hello", Timestamp: time.Now().UTC()},
		},
	}
	exec := &chatExecutor{systemInstruction: "you are helpful", assistantContent: "hi there"}
	roundTrip(t, ctx, db, "s-fresh", exec, chainInput)

	// User + assistant. The system_instruction message is intentionally not
	// persisted (it's a chain artefact, re-prepended on every turn). The
	// duplicate user message produced by the synthesizer's overlapping
	// prefix is deduped within the PersistDiff batch.
	roles := loadRoles(t, ctx, db, "s-fresh")
	require.Equal(t, []string{"user", "assistant"}, roles)
}

// Subsequent turn: history non-empty when the chain runs. Verifies that prior
// messages are not re-inserted (PersistDiff dedups against existing DB rows)
// and only the new turn's user + assistant land.
func TestIntegration_ChatRoundTrip_SubsequentTurn(t *testing.T) {
	ctx, db := setupDB(t)
	store := messagestore.New(db.WithoutTransaction(), "")
	require.NoError(t, store.CreateMessageIndex(ctx, "s-cont", "alice"))
	clock := newFixedClock()

	// Turn 1.
	exec := &chatExecutor{
		systemInstruction: "sysprompt",
		assistantContent:  "answer 1",
		systemTime:        clock.Next(),
	}
	chainInput1 := taskengine.ChatHistory{
		Messages: []taskengine.Message{{Role: "user", Content: "first", Timestamp: clock.Next()}},
	}
	exec.assistantTime = clock.Next()
	roundTrip(t, ctx, db, "s-cont", exec, chainInput1)

	// Turn 2 — prior history loaded from DB, plus the new user msg.
	mgr := chatservice.NewManager("")
	prior, err := mgr.ListMessages(ctx, db.WithoutTransaction(), "s-cont")
	require.NoError(t, err)
	require.Len(t, prior, 2, "first turn should have persisted 2 messages")

	chainInput2 := taskengine.ChatHistory{
		Messages: append(prior, taskengine.Message{Role: "user", Content: "second", Timestamp: clock.Next()}),
	}
	exec.assistantContent = "answer 2"
	exec.systemTime = clock.Next()
	exec.assistantTime = clock.Next()
	roundTrip(t, ctx, db, "s-cont", exec, chainInput2)

	contents := loadContents(t, ctx, db, "s-cont")
	require.Len(t, contents, 4, "prior messages must not re-insert; expected first-turn (2) + second-turn (2) = 4 rows")
	require.Equal(t, []string{"first", "answer 1", "second", "answer 2"}, contents, "messages must persist in chronological order with no duplicates")
}

// Chain failure: the model errors hard. Verify the transcript still persists
// what was attempted (the user message + the failure annotation), and that
// PersistDiff doesn't choke on the failure path.
func TestIntegration_ChatRoundTrip_FailurePath(t *testing.T) {
	ctx, db := setupDB(t)
	store := messagestore.New(db.WithoutTransaction(), "")
	require.NoError(t, store.CreateMessageIndex(ctx, "s-fail", "alice"))

	exec := &chatExecutor{err: errors.New("model exploded")}
	chainInput := taskengine.ChatHistory{
		Messages: []taskengine.Message{{Role: "user", Content: "hello", Timestamp: time.Now().UTC()}},
	}

	env, err := taskengine.NewEnv(ctx, libtracker.NoopTracker{}, exec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	_, _, units, runErr := env.ExecEnv(ctx, chatChain(), chainInput, taskengine.DataTypeChatHistory)
	require.Error(t, runErr, "chain with no on_failure must surface the error")
	require.NotNil(t, units, "execution history must come back even on hard failure (every error return path uses stack.GetExecutionHistory())")

	synthesized := taskengine.SynthesizeHistory(chainInput.Messages, units, runErr)
	mgr := chatservice.NewManager("")
	require.NoError(t, mgr.PersistDiff(ctx, db.WithoutTransaction(), "s-fail", synthesized))

	roles := loadRoles(t, ctx, db, "s-fail")
	require.GreaterOrEqual(t, len(roles), 2, "expected at least the user message + a failure annotation")
	require.Equal(t, "user", roles[0])
	// The synthesizer emits a failure annotation as an assistant message.
	require.Equal(t, "assistant", roles[len(roles)-1])
}

// Re-running the same turn should be idempotent: PersistDiff sees the prior
// rows in the DB, dedups against them, and adds nothing. Requires a
// deterministic executor (fixed timestamps) — under real time.Now() each
// replay produces a new assistant timestamp and therefore a new row, which is
// correct but doesn't test the cross-run dedup contract.
func TestIntegration_ChatRoundTrip_IdempotentReplay(t *testing.T) {
	ctx, db := setupDB(t)
	store := messagestore.New(db.WithoutTransaction(), "")
	require.NoError(t, store.CreateMessageIndex(ctx, "s-replay", "alice"))

	stableUserTime := time.Unix(1_700_000_000, 0).UTC()
	stableSystemTime := stableUserTime.Add(time.Second)
	stableAssistantTime := stableUserTime.Add(2 * time.Second)

	chainInput := taskengine.ChatHistory{
		Messages: []taskengine.Message{{Role: "user", Content: "stable input", Timestamp: stableUserTime}},
	}
	exec := &chatExecutor{
		systemInstruction: "sysprompt",
		assistantContent:  "stable output",
		systemTime:        stableSystemTime,
		assistantTime:     stableAssistantTime,
	}

	roundTrip(t, ctx, db, "s-replay", exec, chainInput)
	contentsAfterFirst := loadContents(t, ctx, db, "s-replay")

	// Replay with the SAME timestamps so the message hashes match the
	// already-persisted rows. PersistDiff must dedupe across runs.
	roundTrip(t, ctx, db, "s-replay", exec, chainInput)
	contentsAfterSecond := loadContents(t, ctx, db, "s-replay")

	require.Equal(t, contentsAfterFirst, contentsAfterSecond, "replaying the same chain input with identical timestamps must not duplicate rows")
	require.Equal(t, []string{"stable input", "stable output"}, contentsAfterSecond)
}

// Tool-call shape: assistant emits a tool call, then a (mock) downstream tool
// turn happens. Ensures the tool-call path round-trips cleanly. We use a
// single chain task here; the real two-task workflow loop is exercised via
// the contenox CLI integration but the within-batch dedup contract for
// PersistDiff applies the same way.
func TestIntegration_ChatRoundTrip_AssistantWithToolCall(t *testing.T) {
	ctx, db := setupDB(t)
	store := messagestore.New(db.WithoutTransaction(), "")
	require.NoError(t, store.CreateMessageIndex(ctx, "s-tool", "alice"))

	exec := &chatExecutor{
		systemInstruction: "sysprompt",
		assistantContent:  "",
		assistantToolCall: &taskengine.ToolCall{
			ID:   "call-1",
			Type: "function",
			Function: taskengine.FunctionCall{
				Name:      "list_dir",
				Arguments: `{"path":"."}`,
			},
		},
	}
	chainInput := taskengine.ChatHistory{
		Messages: []taskengine.Message{{Role: "user", Content: "list cwd", Timestamp: time.Now().UTC()}},
	}
	roundTrip(t, ctx, db, "s-tool", exec, chainInput)

	// The chain ends without executing the call, so synthesis pairs it with a
	// stub tool result: strict providers (OpenAI Responses, Bedrock) reject a
	// transcript containing an unanswered tool call on every later turn.
	roles := loadRoles(t, ctx, db, "s-tool")
	require.Equal(t, []string{"user", "assistant", "tool"}, roles, "assistant tool call persists with a pairing stub result")
}
