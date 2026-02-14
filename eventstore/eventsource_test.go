package eventstore_test

import (
	"testing"
	"time"

	"github.com/contenox/vibe/eventstore"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUnit_EventStore_AppendAndRetrieveByAggregate(t *testing.T) {
	ctx, store := SetupStore(t)

	eventType := "user.created"
	aggregateType := "user"
	aggregateID := uuid.NewString()

	event := eventstore.Event{
		EventType:     eventType,
		AggregateID:   aggregateID,
		AggregateType: aggregateType,
		Version:       1,
		Data:          []byte(`{"name": "Alice"}`),
		Metadata:      []byte(`{"source": "api"}`),
		CreatedAt:     time.Now().UTC(),
	}
	err := store.EnsurePartitionExists(ctx, time.Now().UTC())
	require.NoError(t, err)
	err = store.AppendEvent(ctx, &event)
	require.NoError(t, err)
	require.NotEmpty(t, event.ID) // auto-generated

	// Retrieve
	events, err := store.GetEventsByAggregate(ctx,
		eventType,
		event.CreatedAt.Add(-time.Hour),
		event.CreatedAt.Add(time.Hour),
		aggregateType,
		aggregateID,
		10,
	)
	require.NoError(t, err)
	require.Len(t, events, 1)

	got := events[0]
	require.Equal(t, event.EventType, got.EventType)
	require.Equal(t, event.AggregateID, got.AggregateID)
	require.Equal(t, event.AggregateType, got.AggregateType)
	require.Equal(t, event.Version, got.Version)
	require.JSONEq(t, string(event.Data), string(got.Data))
	require.JSONEq(t, string(event.Metadata), string(got.Metadata))
	require.Equal(t, event.ID, got.ID)
	require.WithinDuration(t, event.CreatedAt, got.CreatedAt, time.Second)
}

func TestUnit_EventStore_GetEventsByType(t *testing.T) {
	ctx, store := SetupStore(t)

	eventType := "order.placed"
	now := time.Now().UTC()
	err := store.EnsurePartitionExists(ctx, now)
	require.NoError(t, err)
	// Append 3 events
	for i := 1; i <= 3; i++ {
		event := eventstore.Event{
			EventType:     eventType,
			AggregateID:   uuid.NewString(),
			AggregateType: "order",
			Version:       i,
			Data:          []byte(`{"total": 100}`),
			Metadata:      []byte(`{"currency": "USD"}`),
			CreatedAt:     now.Add(time.Duration(i) * time.Minute),
		}
		err := store.AppendEvent(ctx, &event)
		require.NoError(t, err)
	}

	// Query
	events, err := store.GetEventsByType(ctx, eventType,
		now.Add(-time.Hour),
		now.Add(time.Hour),
		10,
	)
	require.NoError(t, err)
	require.Len(t, events, 3)

	// Should be in DESC order by CreatedAt
	require.True(t, events[0].CreatedAt.After(events[1].CreatedAt))
	require.True(t, events[1].CreatedAt.After(events[2].CreatedAt))
}

func TestUnit_EventStore_GetEventTypesInRange(t *testing.T) {
	ctx, store := SetupStore(t)

	now := time.Now().UTC()
	err := store.EnsurePartitionExists(ctx, now)
	require.NoError(t, err)

	// Append events of different types
	types := []string{"user.login", "order.created", "payment.processed"}
	for _, et := range types {
		event := eventstore.Event{
			EventType:     et,
			AggregateID:   uuid.NewString(),
			AggregateType: "test",
			Version:       1,
			Data:          []byte(`{}`),
			CreatedAt:     now,
		}
		err := store.AppendEvent(ctx, &event)
		require.NoError(t, err)
	}

	// Query
	eventTypes, err := store.GetEventTypesInRange(ctx,
		now.Add(-time.Hour),
		now.Add(time.Hour),
		10,
	)
	require.NoError(t, err)
	require.Len(t, eventTypes, 3)

	// Should be sorted (ORDER BY event_type)
	require.Equal(t, []string{"order.created", "payment.processed", "user.login"}, eventTypes)
}

func TestUnit_EventStore_DeleteEventsByTypeInRange(t *testing.T) {
	ctx, store := SetupStore(t)

	eventType := "session.expired"
	now := time.Now().UTC()
	err := store.EnsurePartitionExists(ctx, now)
	require.NoError(t, err)
	// Insert 2 events
	for i := 0; i < 2; i++ {
		event := eventstore.Event{
			EventType:     eventType,
			AggregateID:   uuid.NewString(),
			AggregateType: "session",
			Version:       1,
			Data:          []byte(`{}`),
			CreatedAt:     now.Add(time.Duration(i) * time.Minute),
		}
		err := store.AppendEvent(ctx, &event)
		require.NoError(t, err)
	}

	// Verify they exist
	events, err := store.GetEventsByType(ctx, eventType,
		now.Add(-time.Hour),
		now.Add(time.Hour),
		10,
	)
	require.NoError(t, err)
	require.Len(t, events, 2)

	// Delete
	err = store.DeleteEventsByTypeInRange(ctx, eventType,
		now.Add(-time.Hour),
		now.Add(time.Hour),
	)
	require.NoError(t, err)

	// Verify gone
	events, err = store.GetEventsByType(ctx, eventType,
		now.Add(-time.Hour),
		now.Add(time.Hour),
		10,
	)
	require.NoError(t, err)
	require.Len(t, events, 0)
}

func TestUnit_EventStore_EmptyResults(t *testing.T) {
	ctx, store := SetupStore(t)

	// Query non-existent aggregate
	events, err := store.GetEventsByAggregate(ctx,
		"non.existent",
		time.Now().Add(-time.Hour),
		time.Now().Add(time.Hour),
		"none",
		uuid.NewString(),
		10,
	)
	require.NoError(t, err)
	require.Len(t, events, 0)

	// Query non-existent event type
	types, err := store.GetEventTypesInRange(ctx,
		time.Now().AddDate(0, 0, -1),
		time.Now().AddDate(0, 0, -1),
		10,
	)
	require.NoError(t, err)
	require.Len(t, types, 0)
}

func TestUnit_EventStore_DeleteInvalidRange(t *testing.T) {
	ctx, store := SetupStore(t)

	err := store.DeleteEventsByTypeInRange(ctx, "test.event",
		time.Now().Add(time.Hour), // from > to
		time.Now(),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid time range")
}

func TestUnit_EventStore_GetEventsBySource(t *testing.T) {
	ctx, store := SetupStore(t)

	eventType := "user.action"
	eventSource1 := "web-api"
	eventSource2 := "mobile-app"
	now := time.Now().UTC()

	err := store.EnsurePartitionExists(ctx, now)
	require.NoError(t, err)

	// Append events with different sources
	for i := 1; i <= 3; i++ {
		event := eventstore.Event{
			EventType:     eventType,
			EventSource:   eventSource1, // web-api source
			AggregateID:   uuid.NewString(),
			AggregateType: "user",
			Version:       i,
			Data:          []byte(`{"action": "login"}`),
			Metadata:      []byte(`{"device": "browser"}`),
			CreatedAt:     now.Add(time.Duration(i) * time.Minute),
		}
		err := store.AppendEvent(ctx, &event)
		require.NoError(t, err)
	}

	// Append events with a different source
	for i := 1; i <= 2; i++ {
		event := eventstore.Event{
			EventType:     eventType,
			EventSource:   eventSource2, // mobile-app source
			AggregateID:   uuid.NewString(),
			AggregateType: "user",
			Version:       i,
			Data:          []byte(`{"action": "logout"}`),
			Metadata:      []byte(`{"device": "phone"}`),
			CreatedAt:     now.Add(time.Duration(i+3) * time.Minute), // different timestamps
		}
		err := store.AppendEvent(ctx, &event)
		require.NoError(t, err)
	}

	// Query events by source 1 (web-api)
	events, err := store.GetEventsBySource(ctx, eventType,
		now.Add(-time.Hour),
		now.Add(time.Hour),
		eventSource1,
		10,
	)
	require.NoError(t, err)
	require.Len(t, events, 3)

	// Verify all returned events have the correct source
	for _, event := range events {
		require.Equal(t, eventSource1, event.EventSource)
		require.Equal(t, eventType, event.EventType)
	}

	// Query events by source 2 (mobile-app)
	events, err = store.GetEventsBySource(ctx, eventType,
		now.Add(-time.Hour),
		now.Add(time.Hour),
		eventSource2,
		10,
	)
	require.NoError(t, err)
	require.Len(t, events, 2)

	// Verify all returned events have the correct source
	for _, event := range events {
		require.Equal(t, eventSource2, event.EventSource)
		require.Equal(t, eventType, event.EventType)
	}

	// Query with non-existent source
	events, err = store.GetEventsBySource(ctx, eventType,
		now.Add(-time.Hour),
		now.Add(time.Hour),
		"non-existent-source",
		10,
	)
	require.NoError(t, err)
	require.Len(t, events, 0)
}
