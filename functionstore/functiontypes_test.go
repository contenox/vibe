package functionstore_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/contenox/vibe/functionstore"
	"github.com/stretchr/testify/require"
)

func TestUnit_FunctionStore_CreateAndGetFunction(t *testing.T) {
	ctx, store := SetupStore(t)

	fn := &functionstore.Function{
		Name:        "send_welcome_email",
		Description: "Sends welcome email to new users",
		ScriptType:  "goja",
		Script:      `console.log("Hello from Goja!");`,
	}

	err := store.CreateFunction(ctx, fn)
	require.NoError(t, err)
	require.False(t, fn.CreatedAt.IsZero())
	require.False(t, fn.UpdatedAt.IsZero())

	// Retrieve
	got, err := store.GetFunction(ctx, fn.Name)
	require.NoError(t, err)
	require.NotNil(t, got)

	require.Equal(t, fn.Name, got.Name)
	require.Equal(t, fn.Description, got.Description)
	require.Equal(t, fn.ScriptType, got.ScriptType)
	require.Equal(t, fn.Script, got.Script)
	require.WithinDuration(t, fn.CreatedAt, got.CreatedAt, time.Second)
	require.WithinDuration(t, fn.UpdatedAt, got.UpdatedAt, time.Second)
}

func TestUnit_FunctionStore_UpdateFunction(t *testing.T) {
	ctx, store := SetupStore(t)

	fn := &functionstore.Function{
		Name:        "update_me",
		Description: "Original description",
		ScriptType:  "goja",
		Script:      `console.log("original");`,
	}

	err := store.CreateFunction(ctx, fn)
	require.NoError(t, err)

	originalUpdatedAt := fn.UpdatedAt

	// Update
	fn.Description = "Updated description"
	fn.Script = `console.log("updated!");`
	err = store.UpdateFunction(ctx, fn)
	require.NoError(t, err)
	require.True(t, fn.UpdatedAt.After(originalUpdatedAt))

	// Verify
	got, err := store.GetFunction(ctx, fn.Name)
	require.NoError(t, err)
	require.Equal(t, "Updated description", got.Description)
	require.Equal(t, `console.log("updated!");`, got.Script)
	require.True(t, got.UpdatedAt.After(originalUpdatedAt))
}

func TestUnit_FunctionStore_DeleteFunction(t *testing.T) {
	ctx, store := SetupStore(t)

	fn := &functionstore.Function{Name: "to_be_deleted"}
	err := store.CreateFunction(ctx, fn)
	require.NoError(t, err)

	err = store.DeleteFunction(ctx, fn.Name)
	require.NoError(t, err)

	_, err = store.GetFunction(ctx, fn.Name)
	require.ErrorIs(t, err, functionstore.ErrNotFound)
}

func TestUnit_FunctionStore_ListFunctions(t *testing.T) {
	ctx, store := SetupStore(t)

	now := time.Now().UTC()

	// Create 3 functions
	for i := 1; i <= 3; i++ {
		fn := &functionstore.Function{
			Name:        fmt.Sprintf("func_%d", i),
			Description: fmt.Sprintf("Function %d", i),
			ScriptType:  "goja",
			Script:      fmt.Sprintf(`console.log("func_%d");`, i),
		}
		err := store.CreateFunction(ctx, fn)
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond) // ensure distinct CreatedAt
	}

	// List with limit 2
	cursor := now.Add(1 * time.Hour) // future time to get all
	functions, err := store.ListFunctions(ctx, &cursor, 2)
	require.NoError(t, err)
	require.Len(t, functions, 2)
	require.Equal(t, "func_3", functions[0].Name) // DESC by CreatedAt
	require.Equal(t, "func_2", functions[1].Name)

	// List next page
	cursor = functions[1].CreatedAt
	functions, err = store.ListFunctions(ctx, &cursor, 2)
	require.NoError(t, err)
	require.Len(t, functions, 1)
	require.Equal(t, "func_1", functions[0].Name)

	// ListAll
	all, err := store.ListAllFunctions(ctx)
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, "func_3", all[0].Name)
}

func TestUnit_FunctionStore_GetFunctionNotFound(t *testing.T) {
	ctx, store := SetupStore(t)

	_, err := store.GetFunction(ctx, "non_existent")
	require.ErrorIs(t, err, functionstore.ErrNotFound)
}

func TestUnit_FunctionStore_UpdateFunctionNotFound(t *testing.T) {
	ctx, store := SetupStore(t)

	fn := &functionstore.Function{Name: "ghost"}
	err := store.UpdateFunction(ctx, fn)
	require.ErrorIs(t, err, functionstore.ErrNotFound)
}

func TestUnit_FunctionStore_DeleteFunctionNotFound(t *testing.T) {
	ctx, store := SetupStore(t)

	err := store.DeleteFunction(ctx, "ghost")
	require.ErrorIs(t, err, functionstore.ErrNotFound)
}

func TestUnit_FunctionStore_LimitExceeded(t *testing.T) {
	ctx, store := SetupStore(t)

	_, err := store.ListFunctions(ctx, nil, functionstore.MAXLIMIT+1)
	require.ErrorIs(t, err, functionstore.ErrLimitParamExceeded)
}

func TestUnit_FunctionStore_CreateAndGetEventTrigger(t *testing.T) {
	ctx, store := SetupStore(t)

	// First, create a function it depends on
	fn := &functionstore.Function{Name: "handler_func"}
	err := store.CreateFunction(ctx, fn)
	require.NoError(t, err)

	trigger := &functionstore.EventTrigger{
		Name:        "user_created_trigger",
		Description: "Triggers on user.created event",
		ListenFor:   functionstore.Listener{Type: "user.created"},
		Type:        "function",
		Function:    fn.Name,
	}

	err = store.CreateEventTrigger(ctx, trigger)
	require.NoError(t, err)
	require.False(t, trigger.CreatedAt.IsZero())
	require.False(t, trigger.UpdatedAt.IsZero())

	// Retrieve
	got, err := store.GetEventTrigger(ctx, trigger.Name)
	require.NoError(t, err)
	require.NotNil(t, got)

	require.Equal(t, trigger.Name, got.Name)
	require.Equal(t, trigger.Description, got.Description)
	require.Equal(t, trigger.ListenFor.Type, got.ListenFor.Type)
	require.Equal(t, trigger.Type, got.Type)
	require.Equal(t, trigger.Function, got.Function)
	require.WithinDuration(t, trigger.CreatedAt, got.CreatedAt, time.Second)
	require.WithinDuration(t, trigger.UpdatedAt, got.UpdatedAt, time.Second)
}

func TestUnit_FunctionStore_UpdateEventTrigger(t *testing.T) {
	ctx, store := SetupStore(t)

	fn := &functionstore.Function{Name: "handler_func"}
	err := store.CreateFunction(ctx, fn)
	require.NoError(t, err)

	trigger := &functionstore.EventTrigger{
		Name:        "updatable_trigger",
		Description: "Original",
		ListenFor:   functionstore.Listener{Type: "event.v1"},
		Type:        "function",
		Function:    fn.Name,
	}
	err = store.CreateEventTrigger(ctx, trigger)
	require.NoError(t, err)

	originalUpdatedAt := trigger.UpdatedAt

	// Update
	trigger.Description = "Updated"
	trigger.ListenFor.Type = "event.v2"
	err = store.UpdateEventTrigger(ctx, trigger)
	require.NoError(t, err)
	require.True(t, trigger.UpdatedAt.After(originalUpdatedAt))

	// Verify
	got, err := store.GetEventTrigger(ctx, trigger.Name)
	require.NoError(t, err)
	require.Equal(t, "Updated", got.Description)
	require.Equal(t, "event.v2", got.ListenFor.Type)
	require.True(t, got.UpdatedAt.After(originalUpdatedAt))
}

func TestUnit_FunctionStore_DeleteEventTrigger(t *testing.T) {
	ctx, store := SetupStore(t)

	fn := &functionstore.Function{Name: "handler_func"}
	err := store.CreateFunction(ctx, fn)
	require.NoError(t, err)

	trigger := &functionstore.EventTrigger{Name: "to_delete", Function: fn.Name}
	err = store.CreateEventTrigger(ctx, trigger)
	require.NoError(t, err)

	err = store.DeleteEventTrigger(ctx, trigger.Name)
	require.NoError(t, err)

	_, err = store.GetEventTrigger(ctx, trigger.Name)
	require.ErrorIs(t, err, functionstore.ErrNotFound)
}

func TestUnit_FunctionStore_ListEventTriggers(t *testing.T) {
	ctx, store := SetupStore(t)

	fn := &functionstore.Function{Name: "common_handler"}
	err := store.CreateFunction(ctx, fn)
	require.NoError(t, err)

	now := time.Now().UTC()

	// Create 3 triggers
	for i := 1; i <= 3; i++ {
		trigger := &functionstore.EventTrigger{
			Name:      fmt.Sprintf("trigger_%d", i),
			ListenFor: functionstore.Listener{Type: fmt.Sprintf("event.type.%d", i)},
			Type:      "function",
			Function:  fn.Name,
		}
		err := store.CreateEventTrigger(ctx, trigger)
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
	}

	// List with limit 2
	cursor := now.Add(1 * time.Hour)
	triggers, err := store.ListEventTriggers(ctx, &cursor, 2)
	require.NoError(t, err)
	require.Len(t, triggers, 2)
	require.Equal(t, "trigger_3", triggers[0].Name)
	require.Equal(t, "trigger_2", triggers[1].Name)

	// List next page
	cursor = triggers[1].CreatedAt
	triggers, err = store.ListEventTriggers(ctx, &cursor, 2)
	require.NoError(t, err)
	require.Len(t, triggers, 1)
	require.Equal(t, "trigger_1", triggers[0].Name)

	// ListAll
	all, err := store.ListAllEventTriggers(ctx)
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, "trigger_3", all[0].Name)
}

func TestUnit_FunctionStore_ListEventTriggersByEventType(t *testing.T) {
	ctx, store := SetupStore(t)

	fn := &functionstore.Function{Name: "handler"}
	err := store.CreateFunction(ctx, fn)
	require.NoError(t, err)

	trigger1 := &functionstore.EventTrigger{
		Name:      "trigger_user",
		ListenFor: functionstore.Listener{Type: "user.created"},
		Function:  fn.Name,
	}
	trigger2 := &functionstore.EventTrigger{
		Name:      "trigger_order",
		ListenFor: functionstore.Listener{Type: "order.placed"},
		Function:  fn.Name,
	}
	err = store.CreateEventTrigger(ctx, trigger1)
	require.NoError(t, err)
	err = store.CreateEventTrigger(ctx, trigger2)
	require.NoError(t, err)

	// Query by event type
	triggers, err := store.ListEventTriggersByEventType(ctx, "user.created")
	require.NoError(t, err)
	require.Len(t, triggers, 1)
	require.Equal(t, "trigger_user", triggers[0].Name)

	triggers, err = store.ListEventTriggersByEventType(ctx, "non.existent")
	require.NoError(t, err)
	require.Len(t, triggers, 0)
}

func TestUnit_FunctionStore_ListEventTriggersByFunction(t *testing.T) {
	ctx, store := SetupStore(t)

	fn1 := &functionstore.Function{Name: "handler1"}
	fn2 := &functionstore.Function{Name: "handler2"}
	err := store.CreateFunction(ctx, fn1)
	require.NoError(t, err)
	err = store.CreateFunction(ctx, fn2)
	require.NoError(t, err)

	trigger1 := &functionstore.EventTrigger{Name: "t1", Function: fn1.Name}
	trigger2 := &functionstore.EventTrigger{Name: "t2", Function: fn2.Name}
	err = store.CreateEventTrigger(ctx, trigger1)
	require.NoError(t, err)
	err = store.CreateEventTrigger(ctx, trigger2)
	require.NoError(t, err)

	// Query by function
	triggers, err := store.ListEventTriggersByFunction(ctx, fn1.Name)
	require.NoError(t, err)
	require.Len(t, triggers, 1)
	require.Equal(t, "t1", triggers[0].Name)

	triggers, err = store.ListEventTriggersByFunction(ctx, "ghost")
	require.NoError(t, err)
	require.Len(t, triggers, 0)
}

func TestUnit_FunctionStore_GetEventTriggerNotFound(t *testing.T) {
	ctx, store := SetupStore(t)

	_, err := store.GetEventTrigger(ctx, "ghost")
	require.ErrorIs(t, err, functionstore.ErrNotFound)
}

func TestUnit_FunctionStore_UpdateEventTriggerNotFound(t *testing.T) {
	ctx, store := SetupStore(t)

	trigger := &functionstore.EventTrigger{Name: "ghost"}
	err := store.UpdateEventTrigger(ctx, trigger)
	require.ErrorIs(t, err, functionstore.ErrNotFound)
}

func TestUnit_FunctionStore_DeleteEventTriggerNotFound(t *testing.T) {
	ctx, store := SetupStore(t)

	err := store.DeleteEventTrigger(ctx, "ghost")
	require.ErrorIs(t, err, functionstore.ErrNotFound)
}
