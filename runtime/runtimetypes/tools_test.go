package runtimetypes_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUnit_RemoteTools_CreateAndGet(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	tools := &runtimetypes.RemoteTools{
		ID:          uuid.New().String(),
		Name:        "test-tools",
		EndpointURL: "https://example.com/tools",
		TimeoutMs:   5000,
		Headers:     map[string]string{"X-Trace-ID": "test"},
		Properties: runtimetypes.InjectionArg{
			Name:  "access_token",
			Value: "secret-token",
			In:    "body",
		},
	}

	// Create the tools
	err := s.CreateRemoteTools(ctx, tools)
	require.NoError(t, err)

	// Retrieve by ID
	retrieved, err := s.GetRemoteTools(ctx, tools.ID)
	require.NoError(t, err)
	require.Equal(t, tools.ID, retrieved.ID)
	require.Equal(t, tools.Name, retrieved.Name)
	require.Equal(t, tools.EndpointURL, retrieved.EndpointURL)
	require.Equal(t, tools.TimeoutMs, retrieved.TimeoutMs)
	require.Equal(t, tools.Headers, retrieved.Headers)
	require.Equal(t, tools.Properties, retrieved.Properties)
	require.WithinDuration(t, time.Now().UTC(), retrieved.CreatedAt, 1*time.Second)
	require.WithinDuration(t, time.Now().UTC(), retrieved.UpdatedAt, 1*time.Second)

	// Retrieve by name
	retrievedByName, err := s.GetRemoteToolsByName(ctx, tools.Name)
	require.NoError(t, err)
	require.Equal(t, tools.ID, retrievedByName.ID)
}

func TestUnit_RemoteTools_WithHeaders(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	t.Run("create with headers", func(t *testing.T) {
		headers := map[string]string{
			"Content-Type":  "application/json",
			"Authorization": "Bearer some-token",
		}
		tools := &runtimetypes.RemoteTools{
			ID:          uuid.New().String(),
			Name:        "tools-with-headers",
			EndpointURL: "https://example.com/tools",
			TimeoutMs:   5000,
			Headers:     headers,
			Properties: runtimetypes.InjectionArg{
				Name:  "api_key",
				Value: "12345",
				In:    "header",
			},
		}

		err := s.CreateRemoteTools(ctx, tools)
		require.NoError(t, err)

		retrieved, err := s.GetRemoteTools(ctx, tools.ID)
		require.NoError(t, err)
		require.NotNil(t, retrieved.Headers)
		require.Equal(t, headers, retrieved.Headers)
	})

	t.Run("create with nil headers", func(t *testing.T) {
		tools := &runtimetypes.RemoteTools{
			ID:          uuid.New().String(),
			Name:        "tools-with-nil-headers",
			EndpointURL: "https://example.com/nil-tools",
			TimeoutMs:   5000,
			Headers:     nil,
			Properties: runtimetypes.InjectionArg{
				Name:  "default_prop",
				Value: true,
				In:    "body",
			},
		}

		err := s.CreateRemoteTools(ctx, tools)
		require.NoError(t, err)

		retrieved, err := s.GetRemoteTools(ctx, tools.ID)
		require.NoError(t, err)
		require.Nil(t, retrieved.Headers)
	})

	t.Run("update headers", func(t *testing.T) {
		initialHeaders := map[string]string{"Initial": "Value"}
		tools := &runtimetypes.RemoteTools{
			ID:          uuid.New().String(),
			Name:        "tools-to-update-headers",
			EndpointURL: "https://example.com/update-tools",
			TimeoutMs:   3000,
			Headers:     initialHeaders,
			Properties: runtimetypes.InjectionArg{
				Name:  "initial_prop",
				Value: "initial",
				In:    "body",
			},
		}
		require.NoError(t, s.CreateRemoteTools(ctx, tools))

		updatedHeaders := map[string]string{"Updated": "NewValue", "Another": "Header"}
		tools.Headers = updatedHeaders
		err := s.UpdateRemoteTools(ctx, tools)
		require.NoError(t, err)

		retrieved, err := s.GetRemoteTools(ctx, tools.ID)
		require.NoError(t, err)
		require.Equal(t, updatedHeaders, retrieved.Headers)

		// Test updating to nil
		tools.Headers = nil
		err = s.UpdateRemoteTools(ctx, tools)
		require.NoError(t, err)

		retrieved, err = s.GetRemoteTools(ctx, tools.ID)
		require.NoError(t, err)
		require.Nil(t, retrieved.Headers)
	})
}

func TestUnit_RemoteTools_Update(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	original := &runtimetypes.RemoteTools{
		ID:          uuid.New().String(),
		Name:        "original-tools",
		EndpointURL: "https://original.com",
		TimeoutMs:   3000,
		Headers:     map[string]string{"Version": "1"},
		Properties: runtimetypes.InjectionArg{
			Name:  "prop1",
			Value: "value1",
			In:    "body",
		},
	}

	require.NoError(t, s.CreateRemoteTools(ctx, original))

	// Update the tools
	updated := *original
	updated.Name = "updated-tools"
	updated.EndpointURL = "https://updated.com"
	updated.TimeoutMs = 10000
	updated.Headers = map[string]string{"Version": "2"}
	updated.Properties = runtimetypes.InjectionArg{
		Name:  "prop2",
		Value: "value2",
		In:    "header",
	}

	err := s.UpdateRemoteTools(ctx, &updated)
	require.NoError(t, err)

	// Verify updates
	retrieved, err := s.GetRemoteTools(ctx, original.ID)
	require.NoError(t, err)
	require.Equal(t, updated.Name, retrieved.Name)
	require.Equal(t, updated.EndpointURL, retrieved.EndpointURL)
	require.Equal(t, updated.TimeoutMs, retrieved.TimeoutMs)
	require.Equal(t, updated.Headers, retrieved.Headers)
	require.Equal(t, updated.Properties, retrieved.Properties)
	require.True(t, retrieved.UpdatedAt.After(original.UpdatedAt), "UpdatedAt should change")
}

func TestUnit_RemoteTools_Delete(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	tools := &runtimetypes.RemoteTools{
		ID:          uuid.New().String(),
		Name:        "tools-to-delete",
		EndpointURL: "https://delete.com",
		TimeoutMs:   2000,
		Properties: runtimetypes.InjectionArg{
			Name:  "temp",
			Value: "delete_me",
			In:    "body",
		},
	}

	require.NoError(t, s.CreateRemoteTools(ctx, tools))

	// Delete the tools
	err := s.DeleteRemoteTools(ctx, tools.ID)
	require.NoError(t, err)

	// Verify deletion
	_, err = s.GetRemoteTools(ctx, tools.ID)
	require.Error(t, err, "Should return error after deletion")
	require.True(t, errors.Is(err, libdb.ErrNotFound))
}

func TestUnit_RemoteTools_List(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	// Create multiple tools with slight delay
	tools := []*runtimetypes.RemoteTools{
		{
			ID:          uuid.New().String(),
			Name:        "tools-1",
			EndpointURL: "https://tools1.com",
			TimeoutMs:   1000,
			Properties: runtimetypes.InjectionArg{
				Name:  "tools1_prop",
				Value: "val1",
				In:    "body",
			},
		},
		{
			ID:          uuid.New().String(),
			Name:        "tools-2",
			EndpointURL: "https://tools2.com",
			TimeoutMs:   2000,
			Properties: runtimetypes.InjectionArg{
				Name:  "tools2_prop",
				Value: "val2",
				In:    "body",
			},
		},
		{
			ID:          uuid.New().String(),
			Name:        "tools-3",
			EndpointURL: "https://tools3.com",
			TimeoutMs:   3000,
			Properties: runtimetypes.InjectionArg{
				Name:  "tools3_prop",
				Value: "val3",
				In:    "body",
			},
		},
	}

	for _, tools := range tools {
		require.NoError(t, s.CreateRemoteTools(ctx, tools))
	}

	// List all tools using a large limit to simulate a non-paginated call
	list, err := s.ListRemoteTools(ctx, nil, 100)
	require.NoError(t, err)
	require.Len(t, list, 3)

	// Verify reverse chronological order (newest first)
	require.Equal(t, tools[2].ID, list[0].ID)
	require.Equal(t, tools[1].ID, list[1].ID)
	require.Equal(t, tools[0].ID, list[2].ID)
}

func TestUnit_RemoteTools_ListPagination(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	// Create 5 tools with a small delay to ensure distinct creation times.
	var createdTools []*runtimetypes.RemoteTools
	for i := range 5 {
		tools := &runtimetypes.RemoteTools{
			ID:          uuid.New().String(),
			Name:        fmt.Sprintf("pagination-tools-%d", i),
			EndpointURL: "https://example.com",
			TimeoutMs:   1000,
			Properties: runtimetypes.InjectionArg{
				Name:  fmt.Sprintf("prop-%d", i),
				Value: i,
				In:    "body",
			},
		}
		err := s.CreateRemoteTools(ctx, tools)
		require.NoError(t, err)
		createdTools = append(createdTools, tools)
	}

	// Paginate through the results with a limit of 2.
	var receivedTools []*runtimetypes.RemoteTools
	var lastCursor *time.Time
	limit := 2

	// Fetch first page
	page1, err := s.ListRemoteTools(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	receivedTools = append(receivedTools, page1...)

	lastCursor = &page1[len(page1)-1].CreatedAt

	// Fetch second page
	page2, err := s.ListRemoteTools(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	receivedTools = append(receivedTools, page2...)

	lastCursor = &page2[len(page2)-1].CreatedAt

	// Fetch third page (the last one)
	page3, err := s.ListRemoteTools(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	receivedTools = append(receivedTools, page3...)

	// Fetch a fourth page, which should be empty
	page4, err := s.ListRemoteTools(ctx, &page3[0].CreatedAt, limit)
	require.NoError(t, err)
	require.Empty(t, page4)

	// Verify all tools were retrieved in the correct order.
	require.Len(t, receivedTools, 5)

	// The order is newest to oldest, so the last created tools should be first.
	require.Equal(t, createdTools[4].ID, receivedTools[0].ID)
	require.Equal(t, createdTools[3].ID, receivedTools[1].ID)
	require.Equal(t, createdTools[2].ID, receivedTools[2].ID)
	require.Equal(t, createdTools[1].ID, receivedTools[3].ID)
	require.Equal(t, createdTools[0].ID, receivedTools[4].ID)
}

func TestUnit_RemoteTools_UniqueNameConstraint(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	tools1 := &runtimetypes.RemoteTools{
		ID:          uuid.New().String(),
		Name:        "unique-tools",
		EndpointURL: "https://unique1.com",
		TimeoutMs:   1000,
		Properties: runtimetypes.InjectionArg{
			Name:  "unique_prop",
			Value: "val1",
			In:    "body",
		},
	}

	tools2 := *tools1
	tools2.ID = uuid.New().String()
	tools2.EndpointURL = "https://unique2.com"
	tools2.Properties = runtimetypes.InjectionArg{
		Name:  "unique_prop2",
		Value: "val2",
		In:    "body",
	}

	// First creation should succeed
	require.NoError(t, s.CreateRemoteTools(ctx, tools1))

	// Second with same name should fail
	err := s.CreateRemoteTools(ctx, &tools2)
	require.Error(t, err, "Should violate unique name constraint")
}

func TestUnit_RemoteTools_NotFoundCases(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	t.Run("get_by_id_not_found", func(t *testing.T) {
		_, err := s.GetRemoteTools(ctx, uuid.New().String())
		require.Error(t, err)
		require.True(t, errors.Is(err, libdb.ErrNotFound))
	})

	t.Run("get_by_name_not_found", func(t *testing.T) {
		_, err := s.GetRemoteToolsByName(ctx, "non-existent-tools")
		require.Error(t, err)
		require.True(t, errors.Is(err, libdb.ErrNotFound))
	})

	t.Run("update_non_existent", func(t *testing.T) {
		tools := &runtimetypes.RemoteTools{
			ID: uuid.New().String(),
			Properties: runtimetypes.InjectionArg{
				Name:  "temp",
				Value: "temp",
				In:    "body",
			},
		}
		err := s.UpdateRemoteTools(ctx, tools)
		require.Error(t, err)
	})

	t.Run("delete_non_existent", func(t *testing.T) {
		err := s.DeleteRemoteTools(ctx, uuid.New().String())
		require.Error(t, err)
	})
}

func TestUnit_RemoteTools_UpdateNonExistent(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	tools := &runtimetypes.RemoteTools{
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

	err := s.UpdateRemoteTools(ctx, tools)
	require.Error(t, err)
	require.True(t, errors.Is(err, libdb.ErrNotFound), "Should return not found error")
}

func TestUnit_RemoteTools_ListEmpty(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	tools, err := s.ListRemoteTools(ctx, nil, 100)
	require.NoError(t, err)
	require.Empty(t, tools, "Should return empty list when no tools exist")
}

func TestUnit_RemoteTools_ConcurrentUpdates(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	// Create initial tools
	tools := &runtimetypes.RemoteTools{
		ID:          uuid.New().String(),
		Name:        "concurrent-tools",
		EndpointURL: "https://concurrent.com",
		TimeoutMs:   1000,
		Properties: runtimetypes.InjectionArg{
			Name:  "initial",
			Value: "value",
			In:    "body",
		},
	}
	require.NoError(t, s.CreateRemoteTools(ctx, tools))

	// Simulate concurrent updates
	updateTools := func(name string) {
		h, err := s.GetRemoteTools(ctx, tools.ID)
		require.NoError(t, err)

		h.Name = name
		err = s.UpdateRemoteTools(ctx, h)
		require.NoError(t, err)
	}

	// Run concurrent updates
	var wg sync.WaitGroup
	names := []string{"update1", "update2", "update3"}
	for _, name := range names {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			updateTools(n)
		}(name)
	}
	wg.Wait()

	// Verify the final state
	final, err := s.GetRemoteTools(ctx, tools.ID)
	require.NoError(t, err)

	// Should have one of the updated names
	require.Contains(t, names, final.Name)
	require.True(t, final.UpdatedAt.After(tools.UpdatedAt))
}

func TestUnit_RemoteTools_DeleteCascade(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	// Create tools
	tools := &runtimetypes.RemoteTools{
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
	require.NoError(t, s.CreateRemoteTools(ctx, tools))

	// Delete and recreate with same name should work
	require.NoError(t, s.DeleteRemoteTools(ctx, tools.ID))

	newTools := *tools
	newTools.ID = uuid.New().String()
	newTools.Properties = runtimetypes.InjectionArg{
		Name:  "new_cascade_prop",
		Value: "new_test",
		In:    "body",
	}
	require.NoError(t, s.CreateRemoteTools(ctx, &newTools))

	// Verify new tools exists
	retrieved, err := s.GetRemoteToolsByName(ctx, tools.Name)
	require.NoError(t, err)
	require.Equal(t, newTools.ID, retrieved.ID)
}

func TestUnit_RemoteTools_WithProperties(t *testing.T) {
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
				tools := &runtimetypes.RemoteTools{
					ID:          uuid.New().String(),
					Name:        fmt.Sprintf("tools-with-%s", tc.name),
					EndpointURL: "https://example.com/tools",
					TimeoutMs:   5000,
					Properties: runtimetypes.InjectionArg{
						Name:  tc.name,
						Value: tc.value,
						In:    tc.in,
					},
				}

				err := s.CreateRemoteTools(ctx, tools)
				require.NoError(t, err)

				retrieved, err := s.GetRemoteTools(ctx, tools.ID)
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
		tools := &runtimetypes.RemoteTools{
			ID:          uuid.New().String(),
			Name:        "tools-with-zero-props",
			EndpointURL: "https://example.com/zero-tools",
			TimeoutMs:   5000,
			Properties:  runtimetypes.InjectionArg{}, // Zero value
		}

		err := s.CreateRemoteTools(ctx, tools)
		require.NoError(t, err)

		retrieved, err := s.GetRemoteTools(ctx, tools.ID)
		require.NoError(t, err)
		require.Equal(t, runtimetypes.InjectionArg{}, retrieved.Properties)
	})

	t.Run("update properties", func(t *testing.T) {
		tools := &runtimetypes.RemoteTools{
			ID:          uuid.New().String(),
			Name:        "tools-to-update-props",
			EndpointURL: "https://example.com/update-tools",
			TimeoutMs:   3000,
			Properties: runtimetypes.InjectionArg{
				Name:  "initial",
				Value: "old_value",
				In:    "body",
			},
		}
		require.NoError(t, s.CreateRemoteTools(ctx, tools))

		updatedProps := runtimetypes.InjectionArg{
			Name:  "updated",
			Value: "new_value",
			In:    "header",
		}
		tools.Properties = updatedProps
		err := s.UpdateRemoteTools(ctx, tools)
		require.NoError(t, err)

		retrieved, err := s.GetRemoteTools(ctx, tools.ID)
		require.NoError(t, err)
		require.Equal(t, updatedProps, retrieved.Properties)
	})
}

func TestUnit_RemoteTools_SpecURL_RoundTrip(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	tools := &runtimetypes.RemoteTools{
		ID:          uuid.New().String(),
		Name:        "spec-url-tools",
		EndpointURL: "https://erp.example.com",
		SpecURL:     "file:///home/user/.contenox/erp-subset.yaml",
		TimeoutMs:   5000,
	}

	require.NoError(t, s.CreateRemoteTools(ctx, tools))

	// GetRemoteTools by ID
	got, err := s.GetRemoteTools(ctx, tools.ID)
	require.NoError(t, err)
	require.Equal(t, tools.SpecURL, got.SpecURL)

	// GetRemoteToolsByName
	gotByName, err := s.GetRemoteToolsByName(ctx, tools.Name)
	require.NoError(t, err)
	require.Equal(t, tools.SpecURL, gotByName.SpecURL)
}

func TestUnit_RemoteTools_SpecURL_EmptyByDefault(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	tools := &runtimetypes.RemoteTools{
		ID:          uuid.New().String(),
		Name:        "no-spec-url-tools",
		EndpointURL: "https://api.example.com",
		TimeoutMs:   5000,
	}
	require.NoError(t, s.CreateRemoteTools(ctx, tools))

	got, err := s.GetRemoteTools(ctx, tools.ID)
	require.NoError(t, err)
	require.Empty(t, got.SpecURL, "SpecURL should be empty when not set")
}

func TestUnit_RemoteTools_SpecURL_Update(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	tools := &runtimetypes.RemoteTools{
		ID:          uuid.New().String(),
		Name:        "spec-url-update-tools",
		EndpointURL: "https://api.example.com",
		TimeoutMs:   5000,
	}
	require.NoError(t, s.CreateRemoteTools(ctx, tools))

	// Set spec URL
	tools.SpecURL = "https://raw.githubusercontent.com/example/repo/main/openapi.yaml"
	require.NoError(t, s.UpdateRemoteTools(ctx, tools))

	got, err := s.GetRemoteTools(ctx, tools.ID)
	require.NoError(t, err)
	require.Equal(t, tools.SpecURL, got.SpecURL)

	// Clear spec URL
	tools.SpecURL = ""
	require.NoError(t, s.UpdateRemoteTools(ctx, tools))

	got, err = s.GetRemoteTools(ctx, tools.ID)
	require.NoError(t, err)
	require.Empty(t, got.SpecURL, "SpecURL should be empty after clearing")
}

func TestUnit_RemoteTools_SpecURL_ListIncludes(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	tools := &runtimetypes.RemoteTools{
		ID:          uuid.New().String(),
		Name:        "spec-url-list-tools",
		EndpointURL: "https://api.example.com",
		SpecURL:     "file:///tmp/spec.json",
		TimeoutMs:   5000,
	}
	require.NoError(t, s.CreateRemoteTools(ctx, tools))

	list, err := s.ListRemoteTools(ctx, nil, 100)
	require.NoError(t, err)

	var found *runtimetypes.RemoteTools
	for _, item := range list {
		if item.ID == tools.ID {
			found = item
			break
		}
	}
	require.NotNil(t, found, "created tools should appear in list")
	require.Equal(t, tools.SpecURL, found.SpecURL)
}
