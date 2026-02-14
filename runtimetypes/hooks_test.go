package runtimetypes_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUnit_RemoteHooks_CreateAndGet(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	hook := &runtimetypes.RemoteHook{
		ID:          uuid.New().String(),
		Name:        "test-hook",
		EndpointURL: "https://example.com/hook",
		TimeoutMs:   5000,
		Headers:     map[string]string{"X-Trace-ID": "test"},
		Properties: runtimetypes.InjectionArg{
			Name:  "access_token",
			Value: "secret-token",
			In:    "body",
		},
	}

	// Create the hook
	err := s.CreateRemoteHook(ctx, hook)
	require.NoError(t, err)

	// Retrieve by ID
	retrieved, err := s.GetRemoteHook(ctx, hook.ID)
	require.NoError(t, err)
	require.Equal(t, hook.ID, retrieved.ID)
	require.Equal(t, hook.Name, retrieved.Name)
	require.Equal(t, hook.EndpointURL, retrieved.EndpointURL)
	require.Equal(t, hook.TimeoutMs, retrieved.TimeoutMs)
	require.Equal(t, hook.Headers, retrieved.Headers)
	require.Equal(t, hook.Properties, retrieved.Properties)
	require.WithinDuration(t, time.Now().UTC(), retrieved.CreatedAt, 1*time.Second)
	require.WithinDuration(t, time.Now().UTC(), retrieved.UpdatedAt, 1*time.Second)

	// Retrieve by name
	retrievedByName, err := s.GetRemoteHookByName(ctx, hook.Name)
	require.NoError(t, err)
	require.Equal(t, hook.ID, retrievedByName.ID)
}

func TestUnit_RemoteHooks_WithHeaders(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	t.Run("create with headers", func(t *testing.T) {
		headers := map[string]string{
			"Content-Type":  "application/json",
			"Authorization": "Bearer some-token",
		}
		hook := &runtimetypes.RemoteHook{
			ID:          uuid.New().String(),
			Name:        "hook-with-headers",
			EndpointURL: "https://example.com/hook",
			TimeoutMs:   5000,
			Headers:     headers,
			Properties: runtimetypes.InjectionArg{
				Name:  "api_key",
				Value: "12345",
				In:    "header",
			},
		}

		err := s.CreateRemoteHook(ctx, hook)
		require.NoError(t, err)

		retrieved, err := s.GetRemoteHook(ctx, hook.ID)
		require.NoError(t, err)
		require.NotNil(t, retrieved.Headers)
		require.Equal(t, headers, retrieved.Headers)
	})

	t.Run("create with nil headers", func(t *testing.T) {
		hook := &runtimetypes.RemoteHook{
			ID:          uuid.New().String(),
			Name:        "hook-with-nil-headers",
			EndpointURL: "https://example.com/nil-hook",
			TimeoutMs:   5000,
			Headers:     nil,
			Properties: runtimetypes.InjectionArg{
				Name:  "default_prop",
				Value: true,
				In:    "body",
			},
		}

		err := s.CreateRemoteHook(ctx, hook)
		require.NoError(t, err)

		retrieved, err := s.GetRemoteHook(ctx, hook.ID)
		require.NoError(t, err)
		require.Nil(t, retrieved.Headers)
	})

	t.Run("update headers", func(t *testing.T) {
		initialHeaders := map[string]string{"Initial": "Value"}
		hook := &runtimetypes.RemoteHook{
			ID:          uuid.New().String(),
			Name:        "hook-to-update-headers",
			EndpointURL: "https://example.com/update-hook",
			TimeoutMs:   3000,
			Headers:     initialHeaders,
			Properties: runtimetypes.InjectionArg{
				Name:  "initial_prop",
				Value: "initial",
				In:    "body",
			},
		}
		require.NoError(t, s.CreateRemoteHook(ctx, hook))

		updatedHeaders := map[string]string{"Updated": "NewValue", "Another": "Header"}
		hook.Headers = updatedHeaders
		err := s.UpdateRemoteHook(ctx, hook)
		require.NoError(t, err)

		retrieved, err := s.GetRemoteHook(ctx, hook.ID)
		require.NoError(t, err)
		require.Equal(t, updatedHeaders, retrieved.Headers)

		// Test updating to nil
		hook.Headers = nil
		err = s.UpdateRemoteHook(ctx, hook)
		require.NoError(t, err)

		retrieved, err = s.GetRemoteHook(ctx, hook.ID)
		require.NoError(t, err)
		require.Nil(t, retrieved.Headers)
	})
}

func TestUnit_RemoteHooks_Update(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	original := &runtimetypes.RemoteHook{
		ID:          uuid.New().String(),
		Name:        "original-hook",
		EndpointURL: "https://original.com",
		TimeoutMs:   3000,
		Headers:     map[string]string{"Version": "1"},
		Properties: runtimetypes.InjectionArg{
			Name:  "prop1",
			Value: "value1",
			In:    "body",
		},
	}

	require.NoError(t, s.CreateRemoteHook(ctx, original))

	// Update the hook
	updated := *original
	updated.Name = "updated-hook"
	updated.EndpointURL = "https://updated.com"
	updated.TimeoutMs = 10000
	updated.Headers = map[string]string{"Version": "2"}
	updated.Properties = runtimetypes.InjectionArg{
		Name:  "prop2",
		Value: "value2",
		In:    "header",
	}

	err := s.UpdateRemoteHook(ctx, &updated)
	require.NoError(t, err)

	// Verify updates
	retrieved, err := s.GetRemoteHook(ctx, original.ID)
	require.NoError(t, err)
	require.Equal(t, updated.Name, retrieved.Name)
	require.Equal(t, updated.EndpointURL, retrieved.EndpointURL)
	require.Equal(t, updated.TimeoutMs, retrieved.TimeoutMs)
	require.Equal(t, updated.Headers, retrieved.Headers)
	require.Equal(t, updated.Properties, retrieved.Properties)
	require.True(t, retrieved.UpdatedAt.After(original.UpdatedAt), "UpdatedAt should change")
}

func TestUnit_RemoteHooks_Delete(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	hook := &runtimetypes.RemoteHook{
		ID:          uuid.New().String(),
		Name:        "hook-to-delete",
		EndpointURL: "https://delete.com",
		TimeoutMs:   2000,
		Properties: runtimetypes.InjectionArg{
			Name:  "temp",
			Value: "delete_me",
			In:    "body",
		},
	}

	require.NoError(t, s.CreateRemoteHook(ctx, hook))

	// Delete the hook
	err := s.DeleteRemoteHook(ctx, hook.ID)
	require.NoError(t, err)

	// Verify deletion
	_, err = s.GetRemoteHook(ctx, hook.ID)
	require.Error(t, err, "Should return error after deletion")
	require.True(t, errors.Is(err, libdb.ErrNotFound))
}

func TestUnit_RemoteHooks_List(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	// Create multiple hooks with slight delay
	hooks := []*runtimetypes.RemoteHook{
		{
			ID:          uuid.New().String(),
			Name:        "hook-1",
			EndpointURL: "https://hook1.com",
			TimeoutMs:   1000,
			Properties: runtimetypes.InjectionArg{
				Name:  "hook1_prop",
				Value: "val1",
				In:    "body",
			},
		},
		{
			ID:          uuid.New().String(),
			Name:        "hook-2",
			EndpointURL: "https://hook2.com",
			TimeoutMs:   2000,
			Properties: runtimetypes.InjectionArg{
				Name:  "hook2_prop",
				Value: "val2",
				In:    "body",
			},
		},
		{
			ID:          uuid.New().String(),
			Name:        "hook-3",
			EndpointURL: "https://hook3.com",
			TimeoutMs:   3000,
			Properties: runtimetypes.InjectionArg{
				Name:  "hook3_prop",
				Value: "val3",
				In:    "body",
			},
		},
	}

	for _, hook := range hooks {
		require.NoError(t, s.CreateRemoteHook(ctx, hook))
	}

	// List all hooks using a large limit to simulate a non-paginated call
	list, err := s.ListRemoteHooks(ctx, nil, 100)
	require.NoError(t, err)
	require.Len(t, list, 3)

	// Verify reverse chronological order (newest first)
	require.Equal(t, hooks[2].ID, list[0].ID)
	require.Equal(t, hooks[1].ID, list[1].ID)
	require.Equal(t, hooks[0].ID, list[2].ID)
}

func TestUnit_RemoteHooks_ListPagination(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	// Create 5 hooks with a small delay to ensure distinct creation times.
	var createdHooks []*runtimetypes.RemoteHook
	for i := range 5 {
		hook := &runtimetypes.RemoteHook{
			ID:          uuid.New().String(),
			Name:        fmt.Sprintf("pagination-hook-%d", i),
			EndpointURL: "https://example.com",
			TimeoutMs:   1000,
			Properties: runtimetypes.InjectionArg{
				Name:  fmt.Sprintf("prop-%d", i),
				Value: i,
				In:    "body",
			},
		}
		err := s.CreateRemoteHook(ctx, hook)
		require.NoError(t, err)
		createdHooks = append(createdHooks, hook)
	}

	// Paginate through the results with a limit of 2.
	var receivedHooks []*runtimetypes.RemoteHook
	var lastCursor *time.Time
	limit := 2

	// Fetch first page
	page1, err := s.ListRemoteHooks(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	receivedHooks = append(receivedHooks, page1...)

	lastCursor = &page1[len(page1)-1].CreatedAt

	// Fetch second page
	page2, err := s.ListRemoteHooks(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	receivedHooks = append(receivedHooks, page2...)

	lastCursor = &page2[len(page2)-1].CreatedAt

	// Fetch third page (the last one)
	page3, err := s.ListRemoteHooks(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	receivedHooks = append(receivedHooks, page3...)

	// Fetch a fourth page, which should be empty
	page4, err := s.ListRemoteHooks(ctx, &page3[0].CreatedAt, limit)
	require.NoError(t, err)
	require.Empty(t, page4)

	// Verify all hooks were retrieved in the correct order.
	require.Len(t, receivedHooks, 5)

	// The order is newest to oldest, so the last created hook should be first.
	require.Equal(t, createdHooks[4].ID, receivedHooks[0].ID)
	require.Equal(t, createdHooks[3].ID, receivedHooks[1].ID)
	require.Equal(t, createdHooks[2].ID, receivedHooks[2].ID)
	require.Equal(t, createdHooks[1].ID, receivedHooks[3].ID)
	require.Equal(t, createdHooks[0].ID, receivedHooks[4].ID)
}

func TestUnit_RemoteHooks_UniqueNameConstraint(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	hook1 := &runtimetypes.RemoteHook{
		ID:          uuid.New().String(),
		Name:        "unique-hook",
		EndpointURL: "https://unique1.com",
		TimeoutMs:   1000,
		Properties: runtimetypes.InjectionArg{
			Name:  "unique_prop",
			Value: "val1",
			In:    "body",
		},
	}

	hook2 := *hook1
	hook2.ID = uuid.New().String()
	hook2.EndpointURL = "https://unique2.com"
	hook2.Properties = runtimetypes.InjectionArg{
		Name:  "unique_prop2",
		Value: "val2",
		In:    "body",
	}

	// First creation should succeed
	require.NoError(t, s.CreateRemoteHook(ctx, hook1))

	// Second with same name should fail
	err := s.CreateRemoteHook(ctx, &hook2)
	require.Error(t, err, "Should violate unique name constraint")
}

func TestUnit_RemoteHooks_NotFoundCases(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	t.Run("get_by_id_not_found", func(t *testing.T) {
		_, err := s.GetRemoteHook(ctx, uuid.New().String())
		require.Error(t, err)
		require.True(t, errors.Is(err, libdb.ErrNotFound))
	})

	t.Run("get_by_name_not_found", func(t *testing.T) {
		_, err := s.GetRemoteHookByName(ctx, "non-existent-hook")
		require.Error(t, err)
		require.True(t, errors.Is(err, libdb.ErrNotFound))
	})

	t.Run("update_non_existent", func(t *testing.T) {
		hook := &runtimetypes.RemoteHook{
			ID: uuid.New().String(),
			Properties: runtimetypes.InjectionArg{
				Name:  "temp",
				Value: "temp",
				In:    "body",
			},
		}
		err := s.UpdateRemoteHook(ctx, hook)
		require.Error(t, err)
	})

	t.Run("delete_non_existent", func(t *testing.T) {
		err := s.DeleteRemoteHook(ctx, uuid.New().String())
		require.Error(t, err)
	})
}

func TestUnit_RemoteHooks_UpdateNonExistent(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	hook := &runtimetypes.RemoteHook{
		ID:          uuid.New().String(), // Doesn't exist
		Name:        "non-existent",
		EndpointURL: "https://update.com",
		TimeoutMs:   5000,
		Properties: runtimetypes.InjectionArg{
			Name:  "non_existent",
			Value: "test",
			In:    "body",
		},
	}

	err := s.UpdateRemoteHook(ctx, hook)
	require.Error(t, err)
	require.True(t, errors.Is(err, libdb.ErrNotFound), "Should return not found error")
}

func TestUnit_RemoteHooks_ListEmpty(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	hooks, err := s.ListRemoteHooks(ctx, nil, 100)
	require.NoError(t, err)
	require.Empty(t, hooks, "Should return empty list when no hooks exist")
}

func TestUnit_RemoteHooks_ConcurrentUpdates(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	// Create initial hook
	hook := &runtimetypes.RemoteHook{
		ID:          uuid.New().String(),
		Name:        "concurrent-hook",
		EndpointURL: "https://concurrent.com",
		TimeoutMs:   1000,
		Properties: runtimetypes.InjectionArg{
			Name:  "initial",
			Value: "value",
			In:    "body",
		},
	}
	require.NoError(t, s.CreateRemoteHook(ctx, hook))

	// Simulate concurrent updates
	updateHook := func(name string) {
		h, err := s.GetRemoteHook(ctx, hook.ID)
		require.NoError(t, err)

		h.Name = name
		err = s.UpdateRemoteHook(ctx, h)
		require.NoError(t, err)
	}

	// Run concurrent updates
	var wg sync.WaitGroup
	names := []string{"update1", "update2", "update3"}
	for _, name := range names {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			updateHook(n)
		}(name)
	}
	wg.Wait()

	// Verify the final state
	final, err := s.GetRemoteHook(ctx, hook.ID)
	require.NoError(t, err)

	// Should have one of the updated names
	require.Contains(t, names, final.Name)
	require.True(t, final.UpdatedAt.After(hook.UpdatedAt))
}

func TestUnit_RemoteHooks_DeleteCascade(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	// Create hook
	hook := &runtimetypes.RemoteHook{
		ID:          uuid.New().String(),
		Name:        "cascade-test",
		EndpointURL: "https://cascade.com",
		TimeoutMs:   5000,
		Properties: runtimetypes.InjectionArg{
			Name:  "cascade_prop",
			Value: "test",
			In:    "body",
		},
	}
	require.NoError(t, s.CreateRemoteHook(ctx, hook))

	// Delete and recreate with same name should work
	require.NoError(t, s.DeleteRemoteHook(ctx, hook.ID))

	newHook := *hook
	newHook.ID = uuid.New().String()
	newHook.Properties = runtimetypes.InjectionArg{
		Name:  "new_cascade_prop",
		Value: "new_test",
		In:    "body",
	}
	require.NoError(t, s.CreateRemoteHook(ctx, &newHook))

	// Verify new hook exists
	retrieved, err := s.GetRemoteHookByName(ctx, hook.Name)
	require.NoError(t, err)
	require.Equal(t, newHook.ID, retrieved.ID)
}

func TestUnit_RemoteHooks_WithProperties(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	t.Run("create with different property types", func(t *testing.T) {
		testCases := []struct {
			name  string
			value any
			in    string
		}{
			{"string_prop", "secret-token", "body"},
			{"bool_prop", true, "header"},
			{"int_prop", 42, "query"},
			{"float_prop", 3.14, "path"},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				hook := &runtimetypes.RemoteHook{
					ID:          uuid.New().String(),
					Name:        fmt.Sprintf("hook-with-%s", tc.name),
					EndpointURL: "https://example.com/hook",
					TimeoutMs:   5000,
					Properties: runtimetypes.InjectionArg{
						Name:  tc.name,
						Value: tc.value,
						In:    tc.in,
					},
				}

				err := s.CreateRemoteHook(ctx, hook)
				require.NoError(t, err)

				retrieved, err := s.GetRemoteHook(ctx, hook.ID)
				require.NoError(t, err)
				require.NotNil(t, retrieved.Properties)
				require.Equal(t, tc.name, retrieved.Properties.Name)
				require.Equal(t, tc.value, retrieved.Properties.Value)
				require.Equal(t, tc.in, retrieved.Properties.In)
			})
		}
	})

	t.Run("create with nil properties", func(t *testing.T) {
		// Note: Properties is not a pointer, so it can't be nil.
		// Instead, we can test with zero values.
		hook := &runtimetypes.RemoteHook{
			ID:          uuid.New().String(),
			Name:        "hook-with-zero-props",
			EndpointURL: "https://example.com/zero-hook",
			TimeoutMs:   5000,
			Properties:  runtimetypes.InjectionArg{}, // Zero value
		}

		err := s.CreateRemoteHook(ctx, hook)
		require.NoError(t, err)

		retrieved, err := s.GetRemoteHook(ctx, hook.ID)
		require.NoError(t, err)
		require.Equal(t, runtimetypes.InjectionArg{}, retrieved.Properties)
	})

	t.Run("update properties", func(t *testing.T) {
		hook := &runtimetypes.RemoteHook{
			ID:          uuid.New().String(),
			Name:        "hook-to-update-props",
			EndpointURL: "https://example.com/update-hook",
			TimeoutMs:   3000,
			Properties: runtimetypes.InjectionArg{
				Name:  "initial",
				Value: "old_value",
				In:    "body",
			},
		}
		require.NoError(t, s.CreateRemoteHook(ctx, hook))

		updatedProps := runtimetypes.InjectionArg{
			Name:  "updated",
			Value: "new_value",
			In:    "header",
		}
		hook.Properties = updatedProps
		err := s.UpdateRemoteHook(ctx, hook)
		require.NoError(t, err)

		retrieved, err := s.GetRemoteHook(ctx, hook.ID)
		require.NoError(t, err)
		require.Equal(t, updatedProps, retrieved.Properties)
	})
}
