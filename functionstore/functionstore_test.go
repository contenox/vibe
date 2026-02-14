package functionstore_test

import (
	"context"
	"os"
	"testing"

	"github.com/contenox/vibe/functionstore"
	libdb "github.com/contenox/vibe/libdbexec"
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

// SetupStore initializes a test Postgres instance with functionstore schema.
func SetupStore(t *testing.T) (context.Context, functionstore.Store) {
	t.Helper()

	unquiet := quiet()
	t.Cleanup(unquiet)

	ctx := context.TODO()
	connStr, _, cleanup, err := libdb.SetupLocalInstance(ctx, "test", "test", "test")
	require.NoError(t, err)

	dbManager, err := libdb.NewPostgresDBManager(ctx, connStr, "")
	require.NoError(t, err)

	// Apply schema
	err = functionstore.InitSchema(ctx, dbManager.WithoutTransaction())
	require.NoError(t, err)

	// Cleanup
	t.Cleanup(func() {
		require.NoError(t, dbManager.Close())
		cleanup()
	})

	return ctx, functionstore.New(dbManager.WithoutTransaction())
}
