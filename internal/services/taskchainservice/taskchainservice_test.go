package taskchainservice_test

import (
	"context"
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
			{ID: "one", Handler: "prompt"},
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
