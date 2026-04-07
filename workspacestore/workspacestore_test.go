package workspacestore_test

import (
	"context"
	"os"
	"testing"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/workspacestore"
	"github.com/stretchr/testify/require"
)

func quiet() func() {
	null, _ := os.Open(os.DevNull)
	sout := os.Stdout
	serr := os.Stderr
	os.Stdout = null
	os.Stderr = null
	return func() {
		defer null.Close()
		os.Stdout = sout
		os.Stderr = serr
	}
}

func SetupStore(t *testing.T) (context.Context, libdb.DBManager) {
	t.Helper()
	unquiet := quiet()
	t.Cleanup(unquiet)

	ctx := context.TODO()
	connStr, _, cleanup, err := libdb.SetupLocalInstance(ctx, "test", "test", "test")
	require.NoError(t, err)

	dbManager, err := libdb.NewPostgresDBManager(ctx, connStr, "")
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, dbManager.Close())
		cleanup()
	})

	err = workspacestore.InitSchema(ctx, dbManager.WithoutTransaction())
	require.NoError(t, err)

	return ctx, dbManager
}
