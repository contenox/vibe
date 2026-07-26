package taskengine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libkvstore"
	"github.com/contenox/beam/internal/libtracker"
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
