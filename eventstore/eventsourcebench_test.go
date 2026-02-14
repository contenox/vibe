package eventstore_test

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/contenox/vibe/eventstore"
	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const (
	benchmarkEventSize = 512 // bytes of JSON data
)

// setupEventStoreBenchmark initializes a fresh event store for benchmarking
func setupEventStoreBenchmark(ctx context.Context, b testing.TB) (eventstore.Store, func()) {
	b.Helper()

	unquiet := quiet()
	b.Cleanup(unquiet)

	connStr, _, cleanup, err := libdb.SetupLocalInstance(ctx, "bench", "bench", "bench")
	require.NoError(b, err)

	dbManager, err := libdb.NewPostgresDBManager(ctx, connStr, "")
	require.NoError(b, err)

	// Apply schema
	err = eventstore.InitSchema(ctx, dbManager.WithoutTransaction())
	require.NoError(b, err)

	store := eventstore.New(dbManager.WithoutTransaction())
	err = store.EnsurePartitionExists(ctx, time.Now().UTC())
	err = store.EnsurePartitionExists(ctx, time.Now().UTC().Add(time.Hour*24))

	require.NoError(b, err)
	return store, func() {
		require.NoError(b, dbManager.Close())
		cleanup()
		unquiet()
	}
}

// generateBenchmarkEventData creates random but valid JSON event data
func generateBenchmarkEventData() []byte {
	id := uuid.NewString()
	return fmt.Appendf(nil, `{"id": "%s", "value": %d, "payload": "%s"}`, id, rand.Intn(1000), randomString(50))
}

// generateBenchmarkMetadata creates random metadata
func generateBenchmarkMetadata() []byte {
	return fmt.Appendf(nil, `{"trace_id": "%s", "source": "benchmark"}`, uuid.NewString())
}

// randomString generates a random string of given length
func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

// showOpsPerSecond reports operations per second in benchmark
func showOpsPerSecond(b *testing.B, ops int64) {
	elapsed := b.Elapsed().Seconds()
	if elapsed > 0 {
		opsPerSec := float64(ops) / elapsed
		b.ReportMetric(opsPerSec, "ops/s")
	}
}

// createEventsForBenchmarkWithID creates N events for a specific aggregate ID
func createEventsForBenchmarkWithID(ctx context.Context, b *testing.B, store eventstore.Store, eventType, aggregateType, aggregateID string, count int) []eventstore.Event {
	b.Helper()

	events := make([]eventstore.Event, 0, count)
	now := time.Now().UTC()

	for i := 0; i < count; i++ {
		event := eventstore.Event{
			ID:            uuid.NewString(),
			EventType:     eventType,
			AggregateID:   aggregateID,
			AggregateType: aggregateType,
			Version:       i + 1,
			Data:          generateBenchmarkEventData(),
			Metadata:      generateBenchmarkMetadata(),
			CreatedAt:     now.Add(time.Duration(i) * time.Millisecond),
		}

		err := store.AppendEvent(ctx, &event)
		if err != nil {
			b.Fatalf("AppendEvent failed at index %d: %v", i, err)
		}
		events = append(events, event)
	}

	return events
}

// createEventsForBenchmark creates N events for benchmarking queries
func createEventsForBenchmark(ctx context.Context, b *testing.B, store eventstore.Store, eventType, aggregateType string, count int) []eventstore.Event {
	b.Helper()

	events := make([]eventstore.Event, 0, count)
	now := time.Now().UTC()

	for i := 0; i < count; i++ {
		event := eventstore.Event{
			ID:            uuid.NewString(),
			EventType:     eventType,
			AggregateID:   uuid.NewString(),
			AggregateType: aggregateType,
			Version:       i + 1,
			Data:          generateBenchmarkEventData(),
			Metadata:      generateBenchmarkMetadata(),
			CreatedAt:     now.Add(time.Duration(i) * time.Millisecond),
		}

		err := store.AppendEvent(ctx, &event)
		if err != nil {
			b.Fatalf("AppendEvent failed at index %d: %v", i, err)
		}
		events = append(events, event)
	}

	return events
}

func BenchmarkAppendEvent(b *testing.B) {
	ctx := context.Background()
	store, teardown := setupEventStoreBenchmark(ctx, b)
	defer teardown()

	eventType := "bench.event"
	aggregateType := "bench.aggregate"

	// Pre-generate event to avoid allocation noise
	eventTemplate := eventstore.Event{
		EventType:     eventType,
		AggregateType: aggregateType,
		AggregateID:   uuid.NewString(),
		Version:       1,
		Data:          generateBenchmarkEventData(),
		Metadata:      generateBenchmarkMetadata(),
		CreatedAt:     time.Now().UTC(),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		event := eventTemplate
		event.AggregateID = uuid.NewString() // vary per iteration
		event.ID = uuid.NewString()
		event.CreatedAt = time.Now().UTC()

		err := store.AppendEvent(ctx, &event)
		if err != nil {
			b.Fatalf("AppendEvent failed: %v", err)
		}
	}
	showOpsPerSecond(b, int64(b.N))
}

func BenchmarkGetEventsByAggregate(b *testing.B) {
	ctx := context.Background()
	store, teardown := setupEventStoreBenchmark(ctx, b)
	defer teardown()

	eventType := "bench.query.aggregate"
	aggregateType := "user"
	aggregateID := uuid.NewString()

	createEventsForBenchmarkWithID(ctx, b, store, eventType, aggregateType, aggregateID, 100)

	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now.Add(time.Hour)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		events, err := store.GetEventsByAggregate(ctx, eventType, from, to, aggregateType, aggregateID, 10)
		if err != nil {
			b.Fatalf("GetEventsByAggregate failed: %v", err)
		}
		if len(events) == 0 {
			b.Fatal("Expected events, got none")
		}
	}
	showOpsPerSecond(b, int64(b.N))
}

func BenchmarkGetEventsByType(b *testing.B) {
	ctx := context.Background()
	store, teardown := setupEventStoreBenchmark(ctx, b)
	defer teardown()

	eventType := "bench.query.type"
	now := time.Now().UTC()

	// Pre-populate with 500 events of this type
	createEventsForBenchmark(ctx, b, store, eventType, "any_aggregate", 500)

	from := now.Add(-time.Hour)
	to := now.Add(time.Hour)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		events, err := store.GetEventsByType(ctx, eventType, from, to, 10)
		if err != nil {
			b.Fatalf("GetEventsByType failed: %v", err)
		}
		if len(events) == 0 {
			b.Fatal("Expected events, got none")
		}
	}
	showOpsPerSecond(b, int64(b.N))
}

func BenchmarkDeleteEventsByTypeInRange(b *testing.B) {
	ctx := context.Background()
	store, teardown := setupEventStoreBenchmark(ctx, b)
	defer teardown()

	eventType := "bench.delete"
	now := time.Now().UTC()

	// Pre-populate with 100 events
	createEventsForBenchmark(ctx, b, store, eventType, "bench", 100)

	from := now.Add(-time.Hour)
	to := now.Add(time.Hour)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := store.DeleteEventsByTypeInRange(ctx, eventType, from, to)
		if err != nil {
			b.Fatalf("DeleteEventsByTypeInRange failed: %v", err)
		}

		// Re-insert for next iteration
		createEventsForBenchmark(ctx, b, store, eventType, "bench", 100)
	}
	showOpsPerSecond(b, int64(b.N))
}
