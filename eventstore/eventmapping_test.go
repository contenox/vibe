package eventstore_test

import (
	"testing"

	"github.com/contenox/vibe/eventstore"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUnit_MappingCRUD(t *testing.T) {
	ctx, store := SetupStore(t)

	uniquePath := func(prefix string) string {
		return prefix + uuid.NewString()[:8]
	}

	t.Run("CreateMapping", func(t *testing.T) {
		path := uniquePath("/webhooks/github/pr-")
		config := &eventstore.MappingConfig{
			Path:               path,
			EventType:          "github.pull_request",
			EventSource:        "github.com",
			AggregateType:      "github.webhook",
			AggregateIDField:   "pull_request.id",
			AggregateTypeField: "repository.type",
			EventTypeField:     "action",
			EventSourceField:   "repository.full_name",
			EventIDField:       "delivery",
			Version:            1,
			MetadataMapping: map[string]string{
				"signature": "headers.X-Hub-Signature",
				"received":  "metadata.received_at",
			},
		}

		err := store.CreateMapping(ctx, config)
		require.NoError(t, err)

		// Verify it exists
		retrieved, err := store.GetMapping(ctx, config.Path)
		require.NoError(t, err)
		require.NotNil(t, retrieved)
		require.Equal(t, config.Path, retrieved.Path)
		require.Equal(t, config.EventType, retrieved.EventType)
		require.Equal(t, config.EventSource, retrieved.EventSource)
		require.Equal(t, config.AggregateType, retrieved.AggregateType)
		require.Equal(t, config.AggregateIDField, retrieved.AggregateIDField)
		require.Equal(t, config.EventTypeField, retrieved.EventTypeField)
		require.Equal(t, config.EventSourceField, retrieved.EventSourceField)
		require.Equal(t, config.EventIDField, retrieved.EventIDField)
		require.Equal(t, config.Version, retrieved.Version)
		require.Equal(t, config.MetadataMapping, retrieved.MetadataMapping)
		require.Equal(t, config.AggregateTypeField, retrieved.AggregateTypeField)
	})

	t.Run("CreateMapping_Duplicate", func(t *testing.T) {
		path := uniquePath("/duplicate-")
		config := &eventstore.MappingConfig{
			Path:          path,
			EventType:     "test.event",
			EventSource:   "test.source",
			AggregateType: "test.aggregate",
			Version:       1,
		}

		err := store.CreateMapping(ctx, config)
		require.NoError(t, err)

		err = store.CreateMapping(ctx, config)
		require.Error(t, err)
	})

	t.Run("GetMapping_NotFound", func(t *testing.T) {
		_, err := store.GetMapping(ctx, uniquePath("/nonexistent-"))
		require.ErrorIs(t, err, eventstore.ErrNotFound)
	})

	t.Run("UpdateMapping", func(t *testing.T) {
		path := uniquePath("/update/test-")
		original := &eventstore.MappingConfig{
			Path:          path,
			EventType:     "original.type",
			EventSource:   "original.source",
			AggregateType: "original.aggregate",
			Version:       1,
			MetadataMapping: map[string]string{
				"old": "value",
			},
		}

		err := store.CreateMapping(ctx, original)
		require.NoError(t, err)

		updated := &eventstore.MappingConfig{
			Path:          original.Path,
			EventType:     "updated.type",
			EventSource:   "updated.source",
			AggregateType: "updated.aggregate",
			Version:       2,
			MetadataMapping: map[string]string{
				"new": "value",
			},
		}

		err = store.UpdateMapping(ctx, updated)
		require.NoError(t, err)

		retrieved, err := store.GetMapping(ctx, updated.Path)
		require.NoError(t, err)
		require.NotNil(t, retrieved)
		require.Equal(t, updated.EventType, retrieved.EventType)
		require.Equal(t, updated.EventSource, retrieved.EventSource)
		require.Equal(t, updated.AggregateType, retrieved.AggregateType)
		require.Equal(t, updated.Version, retrieved.Version)
		require.Equal(t, updated.MetadataMapping, retrieved.MetadataMapping)
	})

	t.Run("UpdateMapping_NotFound", func(t *testing.T) {
		config := &eventstore.MappingConfig{
			Path:          uniquePath("/nonexistent-"),
			EventType:     "some.type",
			EventSource:   "some.source",
			AggregateType: "some.aggregate",
			Version:       1,
		}

		err := store.UpdateMapping(ctx, config)
		require.ErrorIs(t, err, eventstore.ErrNotFound)
	})

	t.Run("DeleteMapping", func(t *testing.T) {
		path := uniquePath("/delete/me-")
		config := &eventstore.MappingConfig{
			Path:          path,
			EventType:     "delete.type",
			EventSource:   "delete.source",
			AggregateType: "delete.aggregate",
			Version:       1,
		}

		err := store.CreateMapping(ctx, config)
		require.NoError(t, err)

		err = store.DeleteMapping(ctx, config.Path)
		require.NoError(t, err)

		_, err = store.GetMapping(ctx, config.Path)
		require.ErrorIs(t, err, eventstore.ErrNotFound)
	})

	t.Run("DeleteMapping_NotFound", func(t *testing.T) {
		err := store.DeleteMapping(ctx, uniquePath("/nonexistent-"))
		require.ErrorIs(t, err, eventstore.ErrNotFound)
	})

	t.Run("ListMappings", func(t *testing.T) {
		path1 := uniquePath("/list/1-")
		path2 := uniquePath("/list/2-")

		configs := []*eventstore.MappingConfig{
			{
				Path:          path1,
				EventType:     "list.type1",
				EventSource:   "list.source1",
				AggregateType: "list.aggregate1",
				Version:       1,
			},
			{
				Path:          path2,
				EventType:     "list.type2",
				EventSource:   "list.source2",
				AggregateType: "list.aggregate2",
				Version:       2,
			},
		}

		for _, c := range configs {
			err := store.CreateMapping(ctx, c)
			require.NoError(t, err)
		}

		listed, err := store.ListMappings(ctx)
		require.NoError(t, err)

		var filtered []*eventstore.MappingConfig
		for _, m := range listed {
			if m.Path == path1 || m.Path == path2 {
				filtered = append(filtered, m)
			}
		}

		require.Len(t, filtered, 2)

		if filtered[0].Path > filtered[1].Path {
			filtered[0], filtered[1] = filtered[1], filtered[0]
		}

		require.Equal(t, path1, filtered[0].Path)
		require.Equal(t, path2, filtered[1].Path)
		require.Equal(t, "list.type1", filtered[0].EventType)
		require.Equal(t, "list.type2", filtered[1].EventType)
	})

	t.Run("MetadataMapping_JSONRoundtrip", func(t *testing.T) {
		path := uniquePath("/metadata/test-")
		mm := map[string]string{
			"key1":         "value1",
			"key2":         "value2",
			"":             "empty_key",
			"key.with.dot": "complex",
		}

		config := &eventstore.MappingConfig{
			Path:            path,
			EventType:       "meta.event",
			EventSource:     "meta.source",
			AggregateType:   "meta.aggregate",
			Version:         1,
			MetadataMapping: mm,
		}

		err := store.CreateMapping(ctx, config)
		require.NoError(t, err)

		retrieved, err := store.GetMapping(ctx, config.Path)
		require.NoError(t, err)
		require.NotNil(t, retrieved)
		require.Equal(t, mm, retrieved.MetadataMapping)

		// Test empty map
		emptyPath := uniquePath("/metadata/empty-")
		emptyConfig := &eventstore.MappingConfig{
			Path:          emptyPath,
			EventType:     "empty.event",
			EventSource:   "empty.source",
			AggregateType: "empty.aggregate",
			Version:       1,
		}

		err = store.CreateMapping(ctx, emptyConfig)
		require.NoError(t, err)

		retrievedEmpty, err := store.GetMapping(ctx, emptyConfig.Path)
		require.NoError(t, err)
		require.NotNil(t, retrievedEmpty)
		require.NotNil(t, retrievedEmpty.MetadataMapping)
		require.Len(t, retrievedEmpty.MetadataMapping, 0)
	})
}

func TestMappingValidation(t *testing.T) {
	ctx, store := SetupStore(t)

	t.Run("CreateMapping_NilConfig", func(t *testing.T) {
		err := store.CreateMapping(ctx, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "cannot be nil")
	})

	t.Run("CreateMapping_EmptyPath", func(t *testing.T) {
		config := &eventstore.MappingConfig{
			EventType:     "test",
			EventSource:   "test",
			AggregateType: "test",
		}
		err := store.CreateMapping(ctx, config)
		require.Error(t, err)
		require.Contains(t, err.Error(), "path is required")
	})

	t.Run("GetMapping_EmptyPath", func(t *testing.T) {
		_, err := store.GetMapping(ctx, "")
		require.Error(t, err)
		require.Contains(t, err.Error(), "path is required")
	})

	t.Run("UpdateMapping_NilConfig", func(t *testing.T) {
		err := store.UpdateMapping(ctx, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "cannot be nil")
	})

	t.Run("UpdateMapping_EmptyPath", func(t *testing.T) {
		config := &eventstore.MappingConfig{
			EventType:     "test",
			EventSource:   "test",
			AggregateType: "test",
		}
		err := store.UpdateMapping(ctx, config)
		require.Error(t, err)
		require.Contains(t, err.Error(), "path is required")
	})

	t.Run("DeleteMapping_EmptyPath", func(t *testing.T) {
		err := store.DeleteMapping(ctx, "")
		require.Error(t, err)
		require.Contains(t, err.Error(), "path is required")
	})
}
