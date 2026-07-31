package taskengine

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/libbus"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/libkvstore"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

func testKVManager(t *testing.T) *libkvstore.SQLiteManager {
	t.Helper()
	db, err := libdb.NewSQLiteDBManager(context.Background(), filepath.Join(t.TempDir(), "state.db"), libkvstore.SQLiteSchema)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return libkvstore.NewSQLiteManager(db)
}

func TestUnit_SanitizeCapturedStateForPersistence_CapsLargePayloads(t *testing.T) {
	large := strings.Repeat("x", capturedPayloadMaxJSONBytes+1024)
	step := CapturedStateUnit{
		TaskID:      "large",
		TaskHandler: HandleChatCompletion.String(),
		InputType:   DataTypeString,
		OutputType:  DataTypeString,
		Input:       "small",
		Output:      large,
	}

	got := sanitizeCapturedStateForPersistence(step)

	require.Equal(t, "small", got.Input)
	summary, ok := got.Output.(CapturedPayloadSummary)
	require.True(t, ok)
	require.True(t, summary.Truncated)
	require.Equal(t, "payload_exceeds_limit", summary.Reason)
	require.Equal(t, "string", summary.OriginalType)
	require.Greater(t, summary.OriginalJSONBytes, capturedPayloadMaxJSONBytes)
	require.NotEmpty(t, summary.SHA256)
	require.LessOrEqual(t, summary.PreviewBytes, capturedPayloadPreviewBytes)
}

func TestUnit_KVInspector_PersistsSanitizedStateButKeepsInnerHistoryFull(t *testing.T) {
	const reqID = "req-kv-sanitized"
	ctx := context.WithValue(context.Background(), libtracker.ContextKeyRequestID, reqID)
	kv := testKVManager(t)
	inspector := NewKVInspector(NewSimpleInspector(), kv, libtracker.NoopTracker{})
	stack := inspector.Start(ctx)
	large := strings.Repeat("x", capturedPayloadMaxJSONBytes+2048)

	stack.RecordStep(CapturedStateUnit{
		TaskID:      "task",
		TaskHandler: HandleNoop.String(),
		InputType:   DataTypeString,
		OutputType:  DataTypeString,
		Input:       large,
		Output:      "ok",
	})

	history := stack.GetExecutionHistory()
	require.Len(t, history, 1)
	require.Equal(t, large, history[0].Input)

	persisted, err := inspector.GetExecutionStateByRequestID(ctx, reqID)
	require.NoError(t, err)
	require.Len(t, persisted, 1)
	require.Equal(t, "ok", persisted[0].Output)

	summary, ok := persisted[0].Input.(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, summary["truncated"])
	require.Equal(t, "payload_exceeds_limit", summary["reason"])
	require.Equal(t, "string", summary["originalType"])
}

func TestUnit_BusInspector_PublishesSanitizedStateButKeepsInnerHistoryFull(t *testing.T) {
	const reqID = "req-bus-sanitized"
	ctx := context.WithValue(context.Background(), libtracker.ContextKeyRequestID, reqID)
	bus := libbus.NewInMem()
	ch := make(chan []byte, 1)
	_, err := bus.Stream(ctx, StateSubject(reqID), ch)
	require.NoError(t, err)
	inspector := NewBusInspector(NewSimpleInspector(), bus, libtracker.NoopTracker{})
	stack := inspector.Start(ctx)
	large := strings.Repeat("y", capturedPayloadMaxJSONBytes+2048)

	stack.RecordStep(CapturedStateUnit{
		TaskID:      "task",
		TaskHandler: HandleNoop.String(),
		InputType:   DataTypeString,
		OutputType:  DataTypeString,
		Input:       "ok",
		Output:      large,
	})

	history := stack.GetExecutionHistory()
	require.Len(t, history, 1)
	require.Equal(t, large, history[0].Output)

	var payload []byte
	select {
	case payload = <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for state bus payload")
	}
	var published CapturedStateUnit
	require.NoError(t, json.Unmarshal(payload, &published))
	require.Equal(t, "ok", published.Input)
	summary, ok := published.Output.(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, summary["truncated"])
	require.Equal(t, "payload_exceeds_limit", summary["reason"])
}
