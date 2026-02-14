package playground_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/contenox/vibe/eventstore"
	"github.com/contenox/vibe/functionstore"
	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/playground"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystem_GojaExecutor(t *testing.T) {
	ctx := context.Background()

	// Setup playground with all required services
	p := playground.New()
	p.WithPostgresTestContainer(ctx).
		WithNats(ctx).
		WithRuntimeState(ctx, false).
		WithMockTokenizer().
		WithFunctionService(ctx).
		WithLLMRepo().
		WithMockHookRegistry().
		WithGojaExecutor(ctx).
		WithEventSourceInit(ctx).
		WithFunctionService(ctx).
		WithEventDispatcher(ctx, func(ctx context.Context, err error) {
			if err != nil {
				t.Logf("Event dispatcher error: %v", err)
			}
		}, time.Millisecond*5).
		WithEventSourceService(ctx).
		WithActivityTracker(libtracker.NewLogActivityTracker(slog.Default())).
		WithGojaExecutorBuildIns(ctx).
		StartGojaExecutorSync(ctx, 100*time.Millisecond)

	require.NoError(t, p.GetError())
	defer p.CleanUp()

	// Get the function service to create test functions
	functionService, err := p.GetFunctionService()
	require.NoError(t, err)

	// Get the executor
	exec, err := p.GetGojaExecutor()
	require.NoError(t, err)

	t.Run("ExecuteSimpleFunction", func(t *testing.T) {
		// Create a simple test function
		simpleFunction := &functionstore.Function{
			Name:       "testSimple",
			ScriptType: "goja",
			Script:     `function testSimple(event) { return { message: "Hello " + event.data.name }; }`,
		}

		err := functionService.CreateFunction(ctx, simpleFunction)
		require.NoError(t, err)

		exec.TriggerSync()
		time.Sleep(time.Millisecond * 100)
		// Create test event
		eventData := map[string]interface{}{"name": "World"}
		dataBytes, err := json.Marshal(eventData)
		require.NoError(t, err)

		event := &eventstore.Event{
			ID:            "test-event-1",
			EventType:     "test.event",
			AggregateType: "test",
			AggregateID:   "123",
			Version:       1,
			Data:          dataBytes,
			CreatedAt:     time.Now().UTC(),
		}

		// Execute the function
		result, err := exec.ExecuteFunction(ctx, simpleFunction.Script, "testSimple", event)
		require.NoError(t, err)
		require.NotNil(t, result)

		assert.Equal(t, "Hello World", result["message"])
	})

	t.Run("ExecuteFunctionWithEventSending", func(t *testing.T) {
		// Create a function that sends events
		eventSendingFunction := &functionstore.Function{
			Name: "testEventSending",
			Script: `
				function testEventSending(event) {
					// Send a new event
					var sendResult = sendEvent("response.event", {
						originalEventId: event.id,
						processedAt: new Date().toISOString()
					});

					return {
						eventSent: sendResult.success,
						newEventId: sendResult.event_id
					};
				}
			`,
			ScriptType: "goja",
		}

		err := functionService.CreateFunction(ctx, eventSendingFunction)
		require.NoError(t, err)

		// Wait a bit for sync to pick up the function
		time.Sleep(200 * time.Millisecond)

		// Create test event
		eventData := map[string]interface{}{"value": 42}
		dataBytes, err := json.Marshal(eventData)
		require.NoError(t, err)

		event := &eventstore.Event{
			ID:            "test-event-3",
			EventType:     "test.event",
			AggregateType: "test",
			AggregateID:   "789",
			Version:       1,
			Data:          dataBytes,
			CreatedAt:     time.Now().UTC(),
		}

		// Execute the function
		result, err := exec.ExecuteFunction(ctx, eventSendingFunction.Script, "testEventSending", event)
		require.NoError(t, err)
		require.NotNil(t, result)

		assert.Equal(t, true, result["eventSent"])
		assert.NotEmpty(t, result["newEventId"])
	})

	t.Run("FunctionErrorHandling", func(t *testing.T) {
		// Create a function that will cause an error
		errorFunction := &functionstore.Function{
			Name: "testError",
			Script: `
				function testError(event) {
					// This will cause a reference error
					return undefinedVariable;
				}
			`,
			ScriptType: "goja",
		}

		err := functionService.CreateFunction(ctx, errorFunction)
		require.NoError(t, err)
		exec.TriggerSync()
		// Wait a bit for sync to pick up the function
		time.Sleep(100 * time.Millisecond)

		// Create test event
		eventData := map[string]interface{}{"value": "test"}
		dataBytes, err := json.Marshal(eventData)
		require.NoError(t, err)

		event := &eventstore.Event{
			ID:            "test-event-4",
			EventType:     "test.event",
			AggregateType: "test",
			AggregateID:   "101",
			Version:       1,
			Data:          dataBytes,
			CreatedAt:     time.Now().UTC(),
		}

		// Execute the function - this should return an error
		result, err := exec.ExecuteFunction(ctx, errorFunction.Script, "testError", event)
		require.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "undefinedVariable")
	})
	t.Run("ExecuteFunctionWithEventMutationAndVerification", func(t *testing.T) {
		// Create a function that modifies and sends events
		eventMutatingFunction := &functionstore.Function{
			Name: "testEventMutation",
			Script: `
            function testEventMutation(event) {
                // Modify the event data
                var modifiedData = {
                    originalId: event.id,
                    originalValue: event.data.value,
                    modifiedValue: event.data.value * 2,
                    processedAt: new Date().toISOString()
                };

                // Send the modified event
                var sendResult = sendEvent("modified.event", modifiedData);

                return {
                    eventSent: sendResult.success,
                    newEventId: sendResult.event_id,
                    modifiedData: modifiedData
                };
            }
        `,
			ScriptType: "goja",
		}

		err := functionService.CreateFunction(ctx, eventMutatingFunction)
		require.NoError(t, err)

		// Wait for sync to pick up the function
		exec.TriggerSync()
		time.Sleep(200 * time.Millisecond)

		// Create test event
		eventData := map[string]interface{}{"value": 42}
		dataBytes, err := json.Marshal(eventData)
		require.NoError(t, err)

		originalEvent := &eventstore.Event{
			ID:            "test-event-mutation",
			EventType:     "test.trigger.event",
			AggregateType: "test",
			AggregateID:   "mutation-test",
			Version:       1,
			Data:          dataBytes,
			CreatedAt:     time.Now().UTC(),
		}

		// Execute the function
		result, err := exec.ExecuteFunction(ctx, eventMutatingFunction.Script, "testEventMutation", originalEvent)
		require.NoError(t, err)
		require.NotNil(t, result)

		assert.Equal(t, true, result["eventSent"])
		assert.NotEmpty(t, result["newEventId"])

		// Get the event source service to verify the event was stored
		eventSourceService, err := p.GetEventSourceService()
		require.NoError(t, err)

		// Give some time for the event to be processed and stored
		time.Sleep(100 * time.Millisecond)

		// Retrieve the modified event from the event source
		from := time.Now().Add(-5 * time.Minute)
		to := time.Now().Add(5 * time.Minute)
		events, err := eventSourceService.GetEventsByType(ctx, "modified.event", from, to, 10)
		require.NoError(t, err)

		// Verify we found the modified event
		require.Len(t, events, 1, "Should find exactly one modified event")

		modifiedEvent := events[0]
		assert.Equal(t, "modified.event", modifiedEvent.EventType)
		assert.Equal(t, "function_execution", modifiedEvent.EventSource)

		// Verify the event data was modified correctly
		var modifiedEventData map[string]interface{}
		err = json.Unmarshal(modifiedEvent.Data, &modifiedEventData)
		require.NoError(t, err)

		assert.Equal(t, "test-event-mutation", modifiedEventData["originalId"])
		assert.Equal(t, 42.0, modifiedEventData["originalValue"]) // JSON numbers are float64 in Go
		assert.Equal(t, 84.0, modifiedEventData["modifiedValue"])
		assert.Contains(t, modifiedEventData, "processedAt")
	})
	t.Run("ExecuteFunctionWithEventMutationAndVerification", func(t *testing.T) {
		// Create a function that modifies and sends events
		eventMutatingFunction := &functionstore.Function{
			Name: "testEventMutation1",
			Script: `
            function testEventMutation1(event) {
                // Modify the event data
                var modifiedData = {
                    originalId: event.id,
                    originalValue: event.data.value,
                    modifiedValue: event.data.value * 2,
                    processedAt: new Date().toISOString()
                };

                // Send the modified event with UNIQUE event type
                var sendResult = sendEvent("modified.event.unique", modifiedData);

                return {
                    eventSent: sendResult.success,
                    newEventId: sendResult.event_id,
                    modifiedData: modifiedData
                };
            }
        `,
			ScriptType: "goja",
		}

		err := functionService.CreateFunction(ctx, eventMutatingFunction)
		require.NoError(t, err)

		// Wait for sync to pick up the function
		exec.TriggerSync()
		time.Sleep(3 * time.Second)

		// Create test event
		eventData := map[string]interface{}{"value": 42}
		dataBytes, err := json.Marshal(eventData)
		require.NoError(t, err)

		originalEvent := &eventstore.Event{
			ID:            "test-event-mutation-unique", // Use unique ID too
			EventType:     "test.trigger.event1",
			AggregateType: "test",
			AggregateID:   "mutation-test-unique", // Use unique aggregate ID
			Version:       1,
			Data:          dataBytes,
			CreatedAt:     time.Now().UTC(),
		}

		// Execute the function
		result, err := exec.ExecuteFunction(ctx, eventMutatingFunction.Script, "testEventMutation1", originalEvent)
		require.NoError(t, err)
		require.NotNil(t, result)

		assert.Equal(t, true, result["eventSent"])
		assert.NotEmpty(t, result["newEventId"])

		// Get the event source service to verify the event was stored
		eventSourceService, err := p.GetEventSourceService()
		require.NoError(t, err)

		// Give some time for the event to be processed and stored
		time.Sleep(4 * time.Second)

		// Retrieve the modified event from the event source - use the UNIQUE event type
		from := time.Now().Add(-5 * time.Minute)
		to := time.Now().Add(5 * time.Minute)
		events, err := eventSourceService.GetEventsByType(ctx, "modified.event.unique", from, to, 10)
		require.NoError(t, err)

		// Verify we found exactly one event (only from this test)
		require.Len(t, events, 1, "Should find exactly one modified event from this test")

		modifiedEvent := events[0]
		assert.Equal(t, "modified.event.unique", modifiedEvent.EventType)
		assert.Equal(t, "function_execution", modifiedEvent.EventSource)

		// Verify the event data was modified correctly
		var modifiedEventData map[string]interface{}
		err = json.Unmarshal(modifiedEvent.Data, &modifiedEventData)
		require.NoError(t, err)

		assert.Equal(t, "test-event-mutation-unique", modifiedEventData["originalId"])
		assert.Equal(t, 42.0, modifiedEventData["originalValue"])
		assert.Equal(t, 84.0, modifiedEventData["modifiedValue"])
		assert.Contains(t, modifiedEventData, "processedAt")

	})
}
