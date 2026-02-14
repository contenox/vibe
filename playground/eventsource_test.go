package playground_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/contenox/vibe/eventstore"
	"github.com/contenox/vibe/playground"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystem_EventSourceService_Subscribe(t *testing.T) {
	ctx := context.Background()

	p := playground.New()
	p.WithPostgresTestContainer(ctx).
		WithNats(ctx).
		WithEventSourceInit(ctx).
		WithFunctionService(ctx).
		WithGojaExecutor(ctx).
		WithEventDispatcher(ctx, func(ctx context.Context, err error) {
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}, time.Millisecond*5).
		WithEventSourceService(ctx)

	require.NoError(t, p.GetError())
	defer p.CleanUp()

	eventService, err := p.GetEventSourceService()
	require.NoError(t, err)

	// Create a channel to receive events
	eventCh := make(chan []byte, 10)
	sub, err := eventService.SubscribeToEvents(ctx, "test.event", eventCh)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// Create test data in a way that ensures consistent JSON formatting
	testData := map[string]string{"key": "value"}
	dataBytes, err := json.Marshal(testData)
	require.NoError(t, err)

	// Append an event
	event := &eventstore.Event{
		EventType:     "test.event",
		AggregateType: "test",
		AggregateID:   "123",
		Version:       1,
		Data:          dataBytes,
		CreatedAt:     time.Now().UTC(),
	}
	err = eventService.AppendEvent(ctx, event)
	require.NoError(t, err)

	// Wait for the event to be received
	select {
	case msg := <-eventCh:
		// Unmarshal the message and check if it's the same event
		var receivedEvent eventstore.Event
		err = json.Unmarshal(msg, &receivedEvent)
		require.NoError(t, err)

		assert.Equal(t, event.EventType, receivedEvent.EventType)
		assert.Equal(t, event.AggregateID, receivedEvent.AggregateID)
		assert.Equal(t, event.AggregateType, receivedEvent.AggregateType)
		assert.Equal(t, event.Version, receivedEvent.Version)

		// Compare the unmarshaled data content rather than the raw bytes
		var expectedData, receivedData map[string]string
		err = json.Unmarshal(event.Data, &expectedData)
		require.NoError(t, err)
		err = json.Unmarshal(receivedEvent.Data, &receivedData)
		require.NoError(t, err)
		assert.Equal(t, expectedData, receivedData)

		assert.WithinDuration(t, event.CreatedAt, receivedEvent.CreatedAt, time.Second)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}
