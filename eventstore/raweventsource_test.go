package eventstore_test

import (
	"testing"
	"time"

	"github.com/contenox/vibe/eventstore"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUnit_RawEvent_AppendAndRetrieve(t *testing.T) {
	ctx, store := SetupStore(t)

	now := time.Now().UTC()
	err := store.EnsurePartitionExists(ctx, now)
	require.NoError(t, err)
	err = store.EnsureRawEventPartitionExists(ctx, now)
	require.NoError(t, err)

	// Create raw event
	rawEvent := &eventstore.RawEvent{
		ReceivedAt: now,
		Path:       "/webhooks/github/push",
		Headers: map[string]string{
			"User-Agent": "GitHub-Hookshot",
		},
		Payload: map[string]interface{}{
			"ref": "refs/heads/main",
			"repository": map[string]interface{}{
				"id": 123,
			},
		},
	}

	// Append
	err = store.AppendRawEvent(ctx, rawEvent)
	require.NoError(t, err)
	require.NotEmpty(t, rawEvent.ID)

	// Retrieve by NID + time range
	retrieved, err := store.GetRawEvent(ctx,
		now.Add(-time.Hour),
		now.Add(time.Hour),
		rawEvent.NID,
	)
	require.NoError(t, err)
	require.NotNil(t, retrieved)
	require.Equal(t, rawEvent.ID, retrieved.ID)
	require.Equal(t, rawEvent.Path, retrieved.Path)
	require.Equal(t, rawEvent.Headers, retrieved.Headers)
	// require.Equal(t, rawEvent.Payload, retrieved.Payload)
	require.WithinDuration(t, rawEvent.ReceivedAt, retrieved.ReceivedAt, time.Second)
}

func TestUnit_RawEvent_ListRawEvents(t *testing.T) {
	ctx, store := SetupStore(t)

	now := time.Now().UTC()
	err := store.EnsurePartitionExists(ctx, now)
	require.NoError(t, err)

	// Append 3 raw events
	var events []*eventstore.RawEvent
	for i := 1; i <= 3; i++ {
		event := &eventstore.RawEvent{
			ReceivedAt: now.Add(time.Duration(i) * time.Minute),
			Path:       "/test/path-" + uuid.NewString()[:8],
			Headers: map[string]string{
				"X-Test": "value",
			},
			Payload: map[string]interface{}{
				"counter": i,
			},
		}
		err := store.AppendRawEvent(ctx, event)
		require.NoError(t, err)
		events = append(events, event)
	}

	// List all in range
	listed, err := store.ListRawEvents(ctx,
		now.Add(-time.Hour),
		now.Add(time.Hour),
		10,
	)
	require.NoError(t, err)
	require.Len(t, listed, 3)

	// Should be in DESC order by ReceivedAt
	require.True(t, listed[0].ReceivedAt.After(listed[1].ReceivedAt))
	require.True(t, listed[1].ReceivedAt.After(listed[2].ReceivedAt))

	// Verify content
	for i, listedEvent := range listed {
		original := events[2-i] // because DESC order
		require.Equal(t, original.ID, listedEvent.ID)
		require.Equal(t, original.Path, listedEvent.Path)
		require.Equal(t, original.Headers, listedEvent.Headers)
		// require.Equal(t, original.Payload, listedEvent.Payload)
	}
}

func TestUnit_RawEvent_GetRawEvent_NotFound(t *testing.T) {
	ctx, store := SetupStore(t)

	now := time.Now().UTC()
	err := store.EnsurePartitionExists(ctx, now)
	require.NoError(t, err)

	// Query non-existent NID
	_, err = store.GetRawEvent(ctx,
		now.Add(-time.Hour),
		now.Add(time.Hour),
		999999999,
	)
	require.ErrorIs(t, err, eventstore.ErrNotFound)
}

func TestUnit_RawEvent_ListRawEvents_Empty(t *testing.T) {
	ctx, store := SetupStore(t)

	// Query empty time range
	listed, err := store.ListRawEvents(ctx,
		time.Now().Add(-2*time.Hour),
		time.Now().Add(-time.Hour),
		10,
	)
	require.NoError(t, err)
	require.Len(t, listed, 0)
}

func TestUnit_RawEvent_AppendRawEvent_AutoGenerateID(t *testing.T) {
	ctx, store := SetupStore(t)

	now := time.Now().UTC()
	err := store.EnsurePartitionExists(ctx, now)
	require.NoError(t, err)

	event := &eventstore.RawEvent{
		ReceivedAt: now,
		Path:       "/auto/id/test",
		Payload: map[string]interface{}{
			"test": true,
		},
	}

	// ID is empty
	require.Empty(t, event.ID)

	err = store.AppendRawEvent(ctx, event)
	require.NoError(t, err)

	// Should be auto-generated
	require.NotEmpty(t, event.ID)
	require.Len(t, event.ID, 36) // UUID length

	// Verify it's retrievable
	retrieved, err := store.GetRawEvent(ctx,
		now.Add(-time.Hour),
		now.Add(time.Hour),
		event.NID,
	)
	require.NoError(t, err)
	require.Equal(t, event.ID, retrieved.ID)
}

func TestUnit_RawEvent_AppendRawEvent_ZeroTime(t *testing.T) {
	ctx, store := SetupStore(t)

	// Don't set ReceivedAt
	event := &eventstore.RawEvent{
		Path: "/zero/time/test",
		Payload: map[string]interface{}{
			"test": true,
		},
	}

	err := store.AppendRawEvent(ctx, event)
	require.NoError(t, err)
	require.NotZero(t, event.ReceivedAt)

	// Should be set to now
	require.WithinDuration(t, time.Now().UTC(), event.ReceivedAt, 2*time.Second)

	// Verify retrievable
	retrieved, err := store.GetRawEvent(ctx,
		event.ReceivedAt.Add(-time.Hour),
		event.ReceivedAt.Add(time.Hour),
		event.NID,
	)
	require.NoError(t, err)
	require.Equal(t, event.ID, retrieved.ID)
}
