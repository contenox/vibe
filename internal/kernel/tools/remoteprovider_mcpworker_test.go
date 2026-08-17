package tools

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/mcpworker"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libbus"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
)

func mcpToolsCall(name, toolName string) *taskengine.ToolsCall {
	return &taskengine.ToolsCall{Name: name, ToolName: toolName}
}

func TestUnit_PersistentRepo_ListToolsWaitsForALateWorkerSubscription(t *testing.T) {
	ctx, db, store := setupRemoteProviderACPDB(t)
	require.NoError(t, store.CreateMCPServer(ctx, testProviderMCP("late-list")))

	bus := libbus.NewInMem()
	t.Cleanup(func() { _ = bus.Close() })
	repo := NewPersistentRepo(nil, db, nil, bus, nil)

	go func() {
		time.Sleep(150 * time.Millisecond)
		_, _ = bus.Serve(ctx, mcpworker.SubjectListTools("late-list"), func(context.Context, []byte) ([]byte, error) {
			return json.Marshal(mcpworker.MCPToolListReply{Tools: []runtimetypes.MCPTool{{
				Name:        "ping",
				Description: "answers",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}}})
		})
	}()

	found, err := repo.GetToolsForToolsByName(ctx, "late-list")
	require.NoError(t, err)
	require.Len(t, found, 1)
	require.Equal(t, "ping", found[0].Function.Name)
}

func TestUnit_PersistentRepo_ExecWaitsForALateWorkerSubscription(t *testing.T) {
	ctx, db, store := setupRemoteProviderACPDB(t)
	require.NoError(t, store.CreateMCPServer(ctx, testProviderMCP("late-exec")))

	bus := libbus.NewInMem()
	t.Cleanup(func() { _ = bus.Close() })
	repo := NewPersistentRepo(nil, db, nil, bus, nil)

	var calls atomic.Int64
	go func() {
		time.Sleep(150 * time.Millisecond)
		_, _ = bus.Serve(ctx, mcpworker.SubjectExecute("late-exec"), func(context.Context, []byte) ([]byte, error) {
			calls.Add(1)
			return json.Marshal(mcpworker.MCPToolReply{Result: "pong"})
		})
	}()

	out, _, err := repo.Exec(ctx, time.Now(), nil, false, mcpToolsCall("late-exec", "late-exec.ping"))
	require.NoError(t, err)
	require.Equal(t, "pong", out)
	require.Equal(t, int64(1), calls.Load())
}

func TestUnit_PersistentRepo_ExecDoesNotRetryAWorkerThatAnswered(t *testing.T) {
	ctx, db, store := setupRemoteProviderACPDB(t)
	require.NoError(t, store.CreateMCPServer(ctx, testProviderMCP("answers-error")))

	bus := libbus.NewInMem()
	t.Cleanup(func() { _ = bus.Close() })
	repo := NewPersistentRepo(nil, db, nil, bus, nil)

	var calls atomic.Int64
	_, err := bus.Serve(ctx, mcpworker.SubjectExecute("answers-error"), func(context.Context, []byte) ([]byte, error) {
		calls.Add(1)
		return json.Marshal(mcpworker.MCPToolReply{Error: "upstream refused"})
	})
	require.NoError(t, err)

	_, _, err = repo.Exec(ctx, time.Now(), nil, false, mcpToolsCall("answers-error", "answers-error.ping"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "upstream refused")
	require.Equal(t, int64(1), calls.Load(), "a worker that answered must never be asked twice")
}

func TestUnit_PersistentRepo_ExecStopsWaitingForAWorkerThatNeverArrives(t *testing.T) {
	ctx, db, store := setupRemoteProviderACPDB(t)
	require.NoError(t, store.CreateMCPServer(ctx, testProviderMCP("no-worker")))

	bus := libbus.NewInMem()
	t.Cleanup(func() { _ = bus.Close() })
	repo := NewPersistentRepo(nil, db, nil, bus, nil)

	started := time.Now()
	_, _, err := repo.Exec(ctx, started, nil, false, mcpToolsCall("no-worker", "no-worker.ping"))
	elapsed := time.Since(started)

	require.Error(t, err)
	require.ErrorIs(t, err, libbus.ErrNoResponders)
	require.GreaterOrEqual(t, elapsed, mcpWorkerWait)
	require.Less(t, elapsed, 4*mcpWorkerWait)
}

func TestSystem_PersistentRepo_MCPToolCallOutlivesAWorkerThatSubscribesLateOnNATS(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx, db, store := setupRemoteProviderACPDB(t)
	require.NoError(t, store.CreateMCPServer(ctx, testProviderMCP("nats-late")))

	natsURL, _, cleanup, err := libbus.SetupNatsInstance(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	caller, err := libbus.NewPubSub(ctx, &libbus.Config{NATSURL: natsURL})
	require.NoError(t, err)
	t.Cleanup(func() { _ = caller.Close() })

	workerBus, err := libbus.NewPubSub(ctx, &libbus.Config{NATSURL: natsURL})
	require.NoError(t, err)
	t.Cleanup(func() { _ = workerBus.Close() })

	repo := NewPersistentRepo(nil, db, nil, caller, nil)

	var calls atomic.Int64
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, _ = workerBus.Serve(ctx, mcpworker.SubjectExecute("nats-late"), func(context.Context, []byte) ([]byte, error) {
			calls.Add(1)
			return json.Marshal(mcpworker.MCPToolReply{Result: "pong"})
		})
	}()

	out, _, err := repo.Exec(ctx, time.Now(), nil, false, mcpToolsCall("nats-late", "nats-late.ping"))
	require.NoError(t, err)
	require.Equal(t, "pong", out)
	require.Equal(t, int64(1), calls.Load())
}
