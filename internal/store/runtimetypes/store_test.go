package runtimetypes_test

import (
	"context"
	"path/filepath"
	"testing"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func TestUnit_Store_QueryingEmptyDB(t *testing.T) {
	ctx := context.TODO()
	dbManager, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "test.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	_ = runtimetypes.New(dbManager.WithoutTransaction())
	t.Cleanup(func() {
		require.NoError(t, dbManager.Close())
	})
}
