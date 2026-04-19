package contenoxcli

import (
	"context"
	"testing"
	"time"

	libbus "github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/stretchr/testify/require"
)

func TestEngineWatchTaskEvents_RequestScoped(t *testing.T) {
	bus := libbus.NewInMem()
	engine := &Engine{Bus: bus}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan taskengine.TaskEvent, 4)
	_, err := engine.WatchTaskEvents(ctx, "req-1", events)
	require.NoError(t, err)

	sink := taskengine.NewBusTaskEventSink(bus)
	require.NoError(t, sink.PublishTaskEvent(context.Background(), taskengine.TaskEvent{
		Kind:      taskengine.TaskEventStepChunk,
		RequestID: "req-2",
		Content:   "ignored",
	}))
	require.NoError(t, sink.PublishTaskEvent(context.Background(), taskengine.TaskEvent{
		Kind:      taskengine.TaskEventStepChunk,
		RequestID: "req-1",
		Content:   "hello",
	}))

	select {
	case event := <-events:
		require.Equal(t, "req-1", event.RequestID)
		require.Equal(t, "hello", event.Content)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for task event")
	}

	select {
	case event := <-events:
		t.Fatalf("unexpected extra event: %+v", event)
	case <-time.After(150 * time.Millisecond):
	}
}
