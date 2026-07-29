package messagestore_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/internal/services/chatservice"
	"github.com/contenox/contenox/internal/services/messagestore"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func setupDB(t *testing.T) (context.Context, libdb.DBManager) {
	t.Helper()
	ctx := context.TODO()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "test.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	return ctx, db
}

func TestUnit_MessageStore_CreateAndListIndices(t *testing.T) {
	ctx, db := setupDB(t)
	store := messagestore.New(db.WithoutTransaction(), "")

	err := store.CreateMessageIndex(ctx, "idx-alice", "alice")
	require.NoError(t, err)

	err = store.CreateMessageIndex(ctx, "idx-alice-2", "alice")
	require.NoError(t, err)

	ids, err := store.ListMessageIndices(ctx, "alice")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"idx-alice", "idx-alice-2"}, ids)

	ids, err = store.ListMessageIndices(ctx, "bob")
	require.NoError(t, err)
	require.Empty(t, ids)
}

func TestUnit_MessageStore_AppendAndList(t *testing.T) {
	ctx, db := setupDB(t)
	store := messagestore.New(db.WithoutTransaction(), "")

	require.NoError(t, store.CreateMessageIndex(ctx, "idx-msgs", "alice"))

	now := time.Now().UTC()
	msgs := []*messagestore.Message{
		{ID: "m1", IDX: "idx-msgs", Payload: marshal(t, taskengine.Message{Role: "user", Content: "hello"}), AddedAt: now},
		{ID: "m2", IDX: "idx-msgs", Payload: marshal(t, taskengine.Message{Role: "assistant", Content: "hi there"}), AddedAt: now.Add(time.Millisecond)},
	}
	require.NoError(t, store.AppendMessages(ctx, msgs...))

	listed, err := store.ListMessages(ctx, "idx-msgs")
	require.NoError(t, err)
	require.Len(t, listed, 2)
	require.Equal(t, "m1", listed[0].ID)
	require.Equal(t, "m2", listed[1].ID)
}

func TestUnit_MessageStore_LastMessage(t *testing.T) {
	ctx, db := setupDB(t)
	store := messagestore.New(db.WithoutTransaction(), "")

	require.NoError(t, store.CreateMessageIndex(ctx, "idx-last", "alice"))

	now := time.Now().UTC()
	require.NoError(t, store.AppendMessages(ctx,
		&messagestore.Message{ID: "first", IDX: "idx-last", Payload: []byte(`"first"`), AddedAt: now},
		&messagestore.Message{ID: "last", IDX: "idx-last", Payload: []byte(`"last"`), AddedAt: now.Add(time.Millisecond)},
	))

	msg, err := store.LastMessage(ctx, "idx-last")
	require.NoError(t, err)
	require.Equal(t, "last", msg.ID)
}

func TestUnit_MessageStore_DeleteMessages(t *testing.T) {
	ctx, db := setupDB(t)
	store := messagestore.New(db.WithoutTransaction(), "")

	require.NoError(t, store.CreateMessageIndex(ctx, "idx-del", "alice"))
	require.NoError(t, store.AppendMessages(ctx,
		&messagestore.Message{ID: "d1", IDX: "idx-del", Payload: []byte(`"x"`), AddedAt: time.Now().UTC()},
	))

	require.NoError(t, store.DeleteMessages(ctx, "idx-del"))

	listed, err := store.ListMessages(ctx, "idx-del")
	require.NoError(t, err)
	require.Empty(t, listed)
}

func TestUnit_ChatService_PersistDiff(t *testing.T) {
	ctx, db := setupDB(t)
	store := messagestore.New(db.WithoutTransaction(), "")
	mgr := chatservice.NewManager("")

	require.NoError(t, store.CreateMessageIndex(ctx, "idx-diff", "dave"))

	now := time.Now().UTC()

	t.Run("initial persist", func(t *testing.T) {
		history := []taskengine.Message{
			{ID: "d1", Role: "user", Content: "hi", Timestamp: now},
			{ID: "d2", Role: "assistant", Content: "hello", Timestamp: now.Add(time.Millisecond)},
		}
		require.NoError(t, mgr.PersistDiff(ctx, db.WithoutTransaction(), "idx-diff", history))

		msgs, err := store.ListMessages(ctx, "idx-diff")
		require.NoError(t, err)
		require.Len(t, msgs, 2)
	})

	t.Run("surgical append only inserts new messages", func(t *testing.T) {
		history := []taskengine.Message{
			{ID: "d1", Role: "user", Content: "hi", Timestamp: now},
			{ID: "d2", Role: "assistant", Content: "hello", Timestamp: now.Add(time.Millisecond)},
			{ID: "d3", Role: "user", Content: "how are you?", Timestamp: now.Add(2 * time.Millisecond)},
		}
		require.NoError(t, mgr.PersistDiff(ctx, db.WithoutTransaction(), "idx-diff", history))

		msgs, err := store.ListMessages(ctx, "idx-diff")
		require.NoError(t, err)
		require.Len(t, msgs, 3, "only the new message should be appended")
	})

	t.Run("list messages returns correct order and content", func(t *testing.T) {
		messages, err := mgr.ListMessages(ctx, db.WithoutTransaction(), "idx-diff")
		require.NoError(t, err)
		require.Len(t, messages, 3)
		require.Equal(t, "hi", messages[0].Content)
		require.Equal(t, "hello", messages[1].Content)
		require.Equal(t, "how are you?", messages[2].Content)
	})
}

// TestUnit_ChatService_PersistDiff_WithinBatchDedup pins that PersistDiff dedups a batch containing the same message twice.
func TestUnit_ChatService_PersistDiff_WithinBatchDedup(t *testing.T) {
	ctx, db := setupDB(t)
	store := messagestore.New(db.WithoutTransaction(), "")
	mgr := chatservice.NewManager("")

	require.NoError(t, store.CreateMessageIndex(ctx, "idx-dup", "alice"))

	now := time.Now().UTC()
	dup := taskengine.Message{Role: "user", Content: "same message", Timestamp: now}
	history := []taskengine.Message{
		dup,
		dup, // identical role+ts+content — same generated ID
		{Role: "assistant", Content: "different", Timestamp: now.Add(time.Millisecond)},
	}

	require.NoError(t, mgr.PersistDiff(ctx, db.WithoutTransaction(), "idx-dup", history))

	msgs, err := store.ListMessages(ctx, "idx-dup")
	require.NoError(t, err)
	require.Len(t, msgs, 2, "duplicate user message must be deduped within the batch; only 2 distinct rows expected")
}

// TestUnit_ChatService_PersistDiff_RoundTripsImages pins the base64 []byte image round-trip through the opaque JSON payload.
func TestUnit_ChatService_PersistDiff_RoundTripsImages(t *testing.T) {
	ctx, db := setupDB(t)
	store := messagestore.New(db.WithoutTransaction(), "")
	mgr := chatservice.NewManager("")

	require.NoError(t, store.CreateMessageIndex(ctx, "idx-img", "carol"))

	now := time.Now().UTC()
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	history := []taskengine.Message{
		{
			ID:      "img1",
			Role:    "user",
			Content: "what is in this screenshot?",
			Images: []taskengine.ImagePart{
				{Data: png, MimeType: "image/png"},
			},
			Timestamp: now,
		},
	}
	require.NoError(t, mgr.PersistDiff(ctx, db.WithoutTransaction(), "idx-img", history))

	messages, err := mgr.ListMessages(ctx, db.WithoutTransaction(), "idx-img")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "what is in this screenshot?", messages[0].Content)
	require.Len(t, messages[0].Images, 1)
	require.Equal(t, png, messages[0].Images[0].Data)
	require.Equal(t, "image/png", messages[0].Images[0].MimeType)
}

func TestUnit_MessageStore_WithTransaction(t *testing.T) {
	ctx, db := setupDB(t)
	store := messagestore.New(db.WithoutTransaction(), "")

	require.NoError(t, store.CreateMessageIndex(ctx, "idx-tx", "eve"))

	t.Run("rollback discards messages", func(t *testing.T) {
		exec, _, release, err := db.WithTransaction(ctx)
		require.NoError(t, err)

		txStore := messagestore.New(exec, "")
		require.NoError(t, txStore.AppendMessages(ctx, &messagestore.Message{
			ID: "rollback-msg", IDX: "idx-tx", Payload: []byte(`"test"`), AddedAt: time.Now().UTC(),
		}))

		require.NoError(t, release()) // rolls back

		msgs, err := store.ListMessages(ctx, "idx-tx")
		require.NoError(t, err)
		require.Empty(t, msgs)
	})

	t.Run("commit persists messages", func(t *testing.T) {
		exec, commit, release, err := db.WithTransaction(ctx)
		require.NoError(t, err)
		defer release()

		txStore := messagestore.New(exec, "")
		require.NoError(t, txStore.AppendMessages(ctx, &messagestore.Message{
			ID: "committed-msg", IDX: "idx-tx", Payload: []byte(`"committed"`), AddedAt: time.Now().UTC(),
		}))
		require.NoError(t, commit(ctx))

		msgs, err := store.ListMessages(ctx, "idx-tx")
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		require.Equal(t, "committed-msg", msgs[0].ID)
	})
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
