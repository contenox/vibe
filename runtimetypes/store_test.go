package runtimetypes_test

import (
	"context"
	"testing"

	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/stretchr/testify/require"
)

func TestUnit_Store_QueryingEmptyDB(t *testing.T) {
	ctx := context.TODO()
	connStr, _, cleanup, err := libdb.SetupLocalInstance(ctx, "test", "test", "test")
	require.NoError(t, err)
	dbManager, err := libdb.NewPostgresDBManager(ctx, connStr, runtimetypes.Schema)
	require.NoError(t, err)
	_ = runtimetypes.New(dbManager.WithoutTransaction())
	t.Cleanup(func() {
		err := dbManager.Close()
		require.NoError(t, err)

		cleanup()
	})
}
