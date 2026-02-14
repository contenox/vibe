package runtimestate_test

// import (
// 	"context"
// 	"encoding/json"
// 	"strings"
// 	"testing"
// 	"time"

// 	libbus "github.com/contenox/vibe/bus"
// 	libdb "github.com/contenox/vibe/dbexec"
// 	"github.com/contenox/vibe-mvp/core/runtimestate"
// 	"github.com/contenox/vibe/store"
// 	"github.com/contenox/vibe-mvp/libs/libroutine"
// 	"github.com/contenox/vibe-mvp/libs/libtestenv"
// 	"github.com/google/uuid"
// 	"github.com/stretchr/testify/require"
// )

// func setupgroupTest(t *testing.T) (context.Context, string, *runtimestate.State, store.Store, func()) {
// 	ctx := context.TODO()

// 	// Setup Ollama instance
// 	ollamaUrl, _, cleanupOllama, err := libtestenv.SetupOllamaLocalInstance(ctx)
// 	require.NoError(t, err)

// 	// Setup database
// 	dbConn, _, cleanupDB, err := libdb.SetupLocalInstance(ctx, "test", "test", "test")
// 	require.NoError(t, err)

// 	dbInstance, err := libdb.NewPostgresDBManager(ctx, dbConn, store.Schema)
// 	require.NoError(t, err)

// 	// Create pubsub
// 	ps, cleanupPS, err := libbus.NewTestPubSub()
// 	require.NoError(t, err)

// 	// Create state with group feature enabled
// 	backendState, err := runtimestate.New(ctx, dbInstance, ps, runtimestate.Withgroups())
// 	require.NoError(t, err)

// 	return ctx, ollamaUrl, backendState, store.New(dbInstance.WithoutTransaction()), func() {
// 		cleanupOllama()
// 		cleanupDB()
// 		cleanupPS()
// 	}
// }

// func TestSystem_groupAwareState_ShouldSyncAndDownloadModels(t *testing.T) {
// 	ctx, ollamaUrl, backendState, dbStore, cleanup := setupgroupTest(t)
// 	defer cleanup()

// 	triggerChan := make(chan struct{}, 10)
// 	breaker := libroutine.NewRoutine(3, 10*time.Second)
// 	go breaker.Loop(ctx, time.Second, triggerChan, backendState.RunBackendCycle, func(err error) {})
// 	breaker2 := libroutine.NewRoutine(3, 10*time.Second)
// 	go breaker2.Loop(ctx, time.Second, triggerChan, backendState.RunDownloadCycle, func(err error) {})

// 	// Create group
// 	groupID := uuid.NewString()
// 	require.NoError(t, dbStore.Creategroup(ctx, &store.group{
// 		ID:          groupID,
// 		Name:        "test-group",
// 		PurposeType: "inference",
// 	}))

// 	// Create backend and assign to group
// 	backendID := uuid.NewString()
// 	require.NoError(t, dbStore.CreateBackend(ctx, &store.Backend{
// 		ID:      backendID,
// 		Name:    "group-backend",
// 		BaseURL: ollamaUrl,
// 		Type:    "ollama",
// 	}))
// 	require.NoError(t, dbStore.AssignBackendTogroup(ctx, groupID, backendID))

// 	// Create model and assign to group
// 	modelID := uuid.NewString()
// 	require.NoError(t, dbStore.AppendModel(ctx, &store.Model{
// 		ID:    modelID,
// 		Model: "granite-embedding:30m",
// 	}))
// 	require.NoError(t, dbStore.AssignModelTogroup(ctx, groupID, modelID))

// 	// Trigger sync and verify state
// 	triggerChan <- struct{}{}
// 	require.Eventually(t, func() bool {
// 		state := backendState.Get(ctx)
// 		if len(state) != 1 {
// 			return false
// 		}
// 		backendState := state[backendID]
// 		return strings.Contains(backendState.Backend.ID, backendID) &&
// 			len(backendState.Models) == 1
// 	}, 5*time.Second, 100*time.Millisecond)

// 	// Verify model download
// 	triggerChan <- struct{}{}
// 	require.Eventually(t, func() bool {
// 		state := backendState.Get(ctx)
// 		if len(state) != 1 {
// 			return false
// 		}
// 		backendState := state[backendID]
// 		if len(backendState.PulledModels) == 0 {
// 			return false
// 		}
// 		r, _ := json.Marshal(backendState.PulledModels[0])
// 		return strings.Contains(string(r), "granite-embedding:30m")
// 	}, 30*time.Second, 1*time.Second)
// }

// func TestSystem_groupIsolation_ShouldNotLeakModelsAcrossgroups(t *testing.T) {
// 	ctx, ollamaUrl, backendState, dbStore, cleanup := setupgroupTest(t)
// 	defer cleanup()

// 	// Create two groups
// 	group1ID := uuid.NewString()
// 	require.NoError(t, dbStore.Creategroup(ctx, &store.group{
// 		ID:          group1ID,
// 		Name:        "group-1",
// 		PurposeType: "inference",
// 	}))
// 	group2ID := uuid.NewString()
// 	require.NoError(t, dbStore.Creategroup(ctx, &store.group{
// 		ID:          group2ID,
// 		Name:        "group-2",
// 		PurposeType: "training",
// 	}))

// 	// Create backends for each group
// 	backend1ID := uuid.NewString()
// 	require.NoError(t, dbStore.CreateBackend(ctx, &store.Backend{
// 		ID:      backend1ID,
// 		Name:    "group-1-backend",
// 		BaseURL: ollamaUrl,
// 		Type:    "ollama",
// 	}))
// 	require.NoError(t, dbStore.AssignBackendTogroup(ctx, group1ID, backend1ID))

// 	backend2ID := uuid.NewString()
// 	require.NoError(t, dbStore.CreateBackend(ctx, &store.Backend{
// 		ID:      backend2ID,
// 		Name:    "group-2-backend",
// 		BaseURL: "http://localhost:11435",
// 		Type:    "ollama",
// 	}))
// 	require.NoError(t, dbStore.AssignBackendTogroup(ctx, group2ID, backend2ID))

// 	// Create model for group1
// 	modelID := uuid.NewString()
// 	require.NoError(t, dbStore.AppendModel(ctx, &store.Model{
// 		ID:    modelID,
// 		Model: "granite-embedding:30m",
// 	}))
// 	require.NoError(t, dbStore.AssignModelTogroup(ctx, group1ID, modelID))

// 	// Trigger sync
// 	triggerChan := make(chan struct{}, 10)
// 	breaker := libroutine.NewRoutine(3, 10*time.Second)
// 	go breaker.Loop(ctx, time.Second, triggerChan, backendState.RunBackendCycle, func(err error) {})
// 	triggerChan <- struct{}{}

// 	// Verify only group1 backend has the model
// 	require.Eventually(t, func() bool {
// 		state := backendState.Get(ctx)
// 		if len(state) != 2 {
// 			return false
// 		}
// 		return len(state[backend1ID].Models) == 1 &&
// 			len(state[backend2ID].Models) == 0
// 	}, 5*time.Second, 100*time.Millisecond)
// }

// func TestUnit_GroupAffinityBackendRemoval(t *testing.T) {
// 	ctx, ollamaUrl, backendState, dbStore, cleanup := setupgroupTest(t)
// 	defer cleanup()

// 	// Create group and backend
// 	groupID := uuid.NewString()
// 	require.NoError(t, dbStore.Creategroup(ctx, &store.group{
// 		ID:          groupID,
// 		Name:        "test-group",
// 		PurposeType: "inference",
// 	}))

// 	backendID := uuid.NewString()
// 	require.NoError(t, dbStore.CreateBackend(ctx, &store.Backend{
// 		ID:      backendID,
// 		Name:    "group-backend",
// 		BaseURL: ollamaUrl,
// 		Type:    "ollama",
// 	}))
// 	require.NoError(t, dbStore.AssignBackendTogroup(ctx, groupID, backendID))

// 	// Initial sync
// 	triggerChan := make(chan struct{}, 10)
// 	breaker := libroutine.NewRoutine(3, 10*time.Second)
// 	go breaker.Loop(ctx, time.Second, triggerChan, backendState.RunBackendCycle, func(err error) {})
// 	triggerChan <- struct{}{}
// 	require.Eventually(t, func() bool {
// 		return len(backendState.Get(ctx)) == 1
// 	}, 5*time.Second, 100*time.Millisecond)

// 	// Remove backend from group
// 	require.NoError(t, dbStore.RemoveBackendFromgroup(ctx, groupID, backendID))
// 	triggerChan <- struct{}{}

// 	// Verify backend is removed from state
// 	require.Eventually(t, func() bool {
// 		return len(backendState.Get(ctx)) == 0
// 	}, 5*time.Second, 100*time.Millisecond)
// }
