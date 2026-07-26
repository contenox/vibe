package taskchainservice_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/localfileservice"
	"github.com/contenox/beam/internal/services/taskchainservice"
	"github.com/stretchr/testify/require"
)

func testChain(id string) *taskengine.TaskChainDefinition {
	return &taskengine.TaskChainDefinition{
		ID: id,
		Tasks: []taskengine.TaskDefinition{
			{ID: "one", Handler: taskengine.HandleNoop},
		},
	}
}

func TestUnit_TaskChainService_CRUD(t *testing.T) {
	ctx := context.Background()
	files, err := localfileservice.New(t.TempDir())
	require.NoError(t, err)
	svc := taskchainservice.NewLocal(files)

	require.NoError(t, svc.CreateAtPath(ctx, "default-chain.json", testChain("default")))
	paths, err := svc.List(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"default-chain.json"}, paths)

	byPath, err := svc.Get(ctx, "default-chain.json")
	require.NoError(t, err)
	require.Equal(t, "default", byPath.ID)

	byID, err := svc.Get(ctx, "default")
	require.NoError(t, err)
	require.Equal(t, "default", byID.ID)

	require.NoError(t, svc.UpdateAtPath(ctx, "default-chain.json", testChain("updated")))
	got, err := svc.Get(ctx, "default-chain.json")
	require.NoError(t, err)
	require.Equal(t, "updated", got.ID)

	require.NoError(t, svc.DeleteByPath(ctx, "default-chain.json"))
	paths, err = svc.List(ctx)
	require.NoError(t, err)
	require.Empty(t, paths)
}

func TestUnit_TaskChainService_RejectsInvalidPath(t *testing.T) {
	files, err := localfileservice.New(t.TempDir())
	require.NoError(t, err)
	svc := taskchainservice.NewLocal(files)

	err = svc.CreateAtPath(context.Background(), "../bad.json", testChain("bad"))
	require.Error(t, err)

	err = svc.CreateAtPath(context.Background(), "bad.txt", testChain("bad"))
	require.Error(t, err)
}

// invalidChain fails the load-time linter (unknown handler) while still
// parsing and carrying an id and tasks — the kind of file that used to slip
// through and explode mid-run.
func invalidChain(id string) *taskengine.TaskChainDefinition {
	return &taskengine.TaskChainDefinition{
		ID: id,
		Tasks: []taskengine.TaskDefinition{
			{ID: "one", Handler: "prompt"},
		},
	}
}

// TestUnit_TaskChainService_WriteRefusesLintFailingChain: validation gates
// the write side, so a broken chain never reaches disk through the service.
func TestUnit_TaskChainService_WriteRefusesLintFailingChain(t *testing.T) {
	ctx := context.Background()
	files, err := localfileservice.New(t.TempDir())
	require.NoError(t, err)
	svc := taskchainservice.NewLocal(files)

	err = svc.CreateAtPath(ctx, "broken.json", invalidChain("broken"))
	require.Error(t, err)
	require.ErrorIs(t, err, taskengine.ErrChainLint)

	// Same gate on update: first write a good chain, then try to break it.
	require.NoError(t, svc.CreateAtPath(ctx, "good.json", testChain("good")))
	err = svc.UpdateAtPath(ctx, "good.json", invalidChain("good"))
	require.Error(t, err)
	require.ErrorIs(t, err, taskengine.ErrChainLint)
}

// TestUnit_TaskChainService_ReadStickyDisablesLintFailingChain: a broken chain
// written to disk BEHIND the service (hand edit, old version) is refused on
// every read — by path and by id — with the linter's reason, until fixed.
// Fixing the file re-enables it with no further ceremony.
func TestUnit_TaskChainService_ReadStickyDisablesLintFailingChain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	files, err := localfileservice.New(dir)
	require.NoError(t, err)
	svc := taskchainservice.NewLocal(files)

	broken, err := json.Marshal(invalidChain("sneaky"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sneaky.json"), broken, 0o600))

	_, err = svc.Get(ctx, "sneaky.json")
	require.Error(t, err)
	require.ErrorIs(t, err, taskengine.ErrChainLint)
	require.Contains(t, err.Error(), "disabled until it is fixed")

	_, err = svc.Get(ctx, "sneaky")
	require.Error(t, err)
	require.ErrorIs(t, err, taskengine.ErrChainLint,
		"lookup by id must refuse with the reason, not report NotFound")

	// Repairing the file lifts the refusal: the disable is exactly as sticky
	// as the defect.
	fixed, err := json.Marshal(testChain("sneaky"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sneaky.json"), fixed, 0o600))
	got, err := svc.Get(ctx, "sneaky")
	require.NoError(t, err)
	require.Equal(t, "sneaky", got.ID)
}
