package runtimetypes_test

// chain_checkpoints store tests, over the production SQLite backend (same
// no-Docker idiom as hitl_approvals_test.go).

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func setupCheckpointStore(t *testing.T) (context.Context, runtimetypes.Store) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "checkpoints.db")
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, runtimetypes.New(db.WithoutTransaction())
}

func newCheckpointRow(id string) *runtimetypes.ChainCheckpoint {
	return &runtimetypes.ChainCheckpoint{
		ID:            id,
		SchemaVersion: 1,
		Payload:       json.RawMessage(`{"schema_version":1,"approval_id":"` + id + `"}`),
		SessionID:     "sess-1",
		RequestID:     "req-1",
		CreatedAt:     time.Now().UTC(),
	}
}

func TestUnit_ChainCheckpoints_CreateGetDelete(t *testing.T) {
	ctx, store := setupCheckpointStore(t)

	row := newCheckpointRow("cp-1")
	require.NoError(t, store.CreateChainCheckpoint(ctx, row))

	got, err := store.GetChainCheckpoint(ctx, "cp-1")
	require.NoError(t, err)
	require.Equal(t, "cp-1", got.ID)
	require.Equal(t, 1, got.SchemaVersion)
	require.JSONEq(t, string(row.Payload), string(got.Payload), "the payload is opaque and must survive byte-equivalent")
	require.Equal(t, "sess-1", got.SessionID)
	require.Equal(t, "req-1", got.RequestID)
	require.Nil(t, got.Failure)
	require.Nil(t, got.ClaimedAt)

	require.NoError(t, store.DeleteChainCheckpoint(ctx, "cp-1"))
	_, err = store.GetChainCheckpoint(ctx, "cp-1")
	require.ErrorIs(t, err, libdb.ErrNotFound)
	require.ErrorIs(t, store.DeleteChainCheckpoint(ctx, "cp-1"), libdb.ErrNotFound)
}

func TestUnit_ChainCheckpoints_ClaimIsExclusiveUntilStale(t *testing.T) {
	ctx, store := setupCheckpointStore(t)
	require.NoError(t, store.CreateChainCheckpoint(ctx, newCheckpointRow("cp-claim")))

	now := time.Now().UTC()
	stale := now.Add(-10 * time.Minute)

	// First claimant wins.
	require.NoError(t, store.ClaimChainCheckpoint(ctx, "cp-claim", now, stale))
	// A second, concurrent claimant loses (claim is fresh).
	require.ErrorIs(t, store.ClaimChainCheckpoint(ctx, "cp-claim", now.Add(time.Second), stale), libdb.ErrNotFound)
	// Once the claim is STALE (resumer died mid-run), it is reclaimable.
	later := now.Add(20 * time.Minute)
	require.NoError(t, store.ClaimChainCheckpoint(ctx, "cp-claim", later, later.Add(-10*time.Minute)))
	// A missing row claims as not-found, indistinguishable by design.
	require.ErrorIs(t, store.ClaimChainCheckpoint(ctx, "no-such", now, stale), libdb.ErrNotFound)
}

func TestUnit_ChainCheckpoints_FailureAnnotationKeepsRow(t *testing.T) {
	ctx, store := setupCheckpointStore(t)
	require.NoError(t, store.CreateChainCheckpoint(ctx, newCheckpointRow("cp-fail")))

	require.NoError(t, store.SetChainCheckpointFailure(ctx, "cp-fail", "resume blew up"))
	got, err := store.GetChainCheckpoint(ctx, "cp-fail")
	require.NoError(t, err)
	require.NotNil(t, got.Failure)
	require.Equal(t, "resume blew up", *got.Failure)
	require.ErrorIs(t, store.SetChainCheckpointFailure(ctx, "no-such", "x"), libdb.ErrNotFound)
}

func TestUnit_ChainCheckpoints_ListNewestFirst(t *testing.T) {
	ctx, store := setupCheckpointStore(t)
	older := newCheckpointRow("cp-old")
	older.CreatedAt = time.Now().UTC().Add(-time.Hour)
	require.NoError(t, store.CreateChainCheckpoint(ctx, older))
	require.NoError(t, store.CreateChainCheckpoint(ctx, newCheckpointRow("cp-new")))

	rows, err := store.ListChainCheckpoints(ctx, nil, 0)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "cp-new", rows[0].ID)
	require.Equal(t, "cp-old", rows[1].ID)

	_, err = store.ListChainCheckpoints(ctx, nil, runtimetypes.MAXLIMIT+1)
	require.ErrorIs(t, err, runtimetypes.ErrLimitParamExceeded)
}
