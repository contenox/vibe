package taskengine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/libkvstore"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

func journalTestKV(t *testing.T) *libkvstore.SQLiteManager {
	t.Helper()
	db, err := libdb.NewSQLiteDBManager(context.Background(), filepath.Join(t.TempDir(), "journal.db"), libkvstore.SQLiteSchema)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return libkvstore.NewSQLiteManager(db)
}

func TestUnit_KVJournalTaskEventSink_JournalsAndReplays(t *testing.T) {
	kv := journalTestKV(t)
	sink := NewKVJournalTaskEventSink(nil, kv, libtracker.NoopTracker{})
	ctx := context.Background()

	events := []TaskEvent{
		{Kind: TaskEventChainStarted, RequestID: "req-j1", ChainID: "c"},
		{Kind: TaskEventApprovalRequested, RequestID: "req-j1", ApprovalID: "a1", ToolName: "write_file"},
		{Kind: TaskEventToolCall, RequestID: "req-j1", ToolName: "local_fs.write_file", ToolDiffPath: "x.txt", ToolDiffNewText: "new"},
		{Kind: TaskEventChainCompleted, RequestID: "req-j1"},
	}
	for _, ev := range events {
		require.NoError(t, sink.PublishTaskEvent(ctx, ev))
	}

	replayed, err := GetJournaledEvents(ctx, kv, "req-j1")
	require.NoError(t, err)
	require.Len(t, replayed, 4)
	require.Equal(t, TaskEventApprovalRequested, replayed[1].Kind)
	require.Equal(t, "x.txt", replayed[2].ToolDiffPath)
	require.Equal(t, TaskEventChainCompleted, replayed[3].Kind)
}

func TestUnit_KVJournalTaskEventSink_SkipsChunksAndAnonymousEvents(t *testing.T) {
	kv := journalTestKV(t)
	sink := NewKVJournalTaskEventSink(nil, kv, libtracker.NoopTracker{})
	ctx := context.Background()

	require.NoError(t, sink.PublishTaskEvent(ctx, TaskEvent{Kind: TaskEventStepChunk, RequestID: "req-j2", Content: "streaming"}))
	require.NoError(t, sink.PublishTaskEvent(ctx, TaskEvent{Kind: TaskEventChainStarted /* no RequestID */}))

	replayed, err := GetJournaledEvents(ctx, kv, "req-j2")
	require.NoError(t, err)
	require.Empty(t, replayed)
}

// TestUnit_KVJournalTaskEventSink_JournalingMatrix pins the per-kind
// journaling decision for every kind in AllTaskEventKinds: step_chunk is the
// only unjournaled kind; step_stream_end is journaled (replay carries stream
// brackets).
func TestUnit_KVJournalTaskEventSink_JournalingMatrix(t *testing.T) {
	kv := journalTestKV(t)
	sink := NewKVJournalTaskEventSink(nil, kv, libtracker.NoopTracker{})
	ctx := context.Background()

	for _, kind := range AllTaskEventKinds() {
		require.NoError(t, sink.PublishTaskEvent(ctx, TaskEvent{
			Kind:      kind,
			RequestID: "req-matrix",
			Scope:     EventScope{Chain: "c", Task: "t"},
		}))
	}

	replayed, err := GetJournaledEvents(ctx, kv, "req-matrix")
	require.NoError(t, err)

	var want []TaskEventKind
	for _, kind := range AllTaskEventKinds() {
		if kind == TaskEventStepChunk {
			continue
		}
		want = append(want, kind)
	}
	var got []TaskEventKind
	for _, ev := range replayed {
		got = append(got, ev.Kind)
		require.Equal(t, EventScope{Chain: "c", Task: "t"}, ev.Scope,
			"the hierarchical address must survive the journal round-trip")
	}
	require.Equal(t, want, got, "journal must keep arrival order and drop exactly step_chunk")
}

// TestUnit_KVJournalTaskEventSink_StreamEndBracketFieldsSurvive asserts the
// replayed stream bracket keeps its payload — the reason step_stream_end
// exists (a replayed run could not previously tell streaming happened).
func TestUnit_KVJournalTaskEventSink_StreamEndBracketFieldsSurvive(t *testing.T) {
	kv := journalTestKV(t)
	sink := NewKVJournalTaskEventSink(nil, kv, libtracker.NoopTracker{})
	ctx := context.Background()

	require.NoError(t, sink.PublishTaskEvent(ctx, TaskEvent{
		Kind:      TaskEventStepChunk,
		RequestID: "req-bracket",
		Content:   "streamed text",
	}))
	require.NoError(t, sink.PublishTaskEvent(ctx, TaskEvent{
		Kind:         TaskEventStepStreamEnd,
		RequestID:    "req-bracket",
		Scope:        EventScope{Chain: "c", Task: "t"},
		ChunkCount:   3,
		FinishReason: "length",
		Usage:        &TokenUsage{Prompt: 5, Completion: 9, Total: 14},
	}))

	replayed, err := GetJournaledEvents(ctx, kv, "req-bracket")
	require.NoError(t, err)
	require.Len(t, replayed, 1, "chunks are dropped; the bracket survives")
	ev := replayed[0]
	require.Equal(t, TaskEventStepStreamEnd, ev.Kind)
	require.Equal(t, 3, ev.ChunkCount)
	require.Equal(t, "length", ev.FinishReason)
	require.NotNil(t, ev.Usage)
	require.Equal(t, TokenUsage{Prompt: 5, Completion: 9, Total: 14}, *ev.Usage)
}

func TestUnit_KVJournalTaskEventSink_CapsLargeTextFields(t *testing.T) {
	kv := journalTestKV(t)
	sink := NewKVJournalTaskEventSink(nil, kv, libtracker.NoopTracker{})
	ctx := context.Background()

	large := strings.Repeat("x", journalTextFieldCap+4096)
	require.NoError(t, sink.PublishTaskEvent(ctx, TaskEvent{
		Kind: TaskEventToolCall, RequestID: "req-j3", ToolDiffNewText: large,
	}))

	replayed, err := GetJournaledEvents(ctx, kv, "req-j3")
	require.NoError(t, err)
	require.Len(t, replayed, 1)
	require.LessOrEqual(t, len(replayed[0].ToolDiffNewText), journalTextFieldCap+64)
	require.Contains(t, replayed[0].ToolDiffNewText, "[truncated]")
}
