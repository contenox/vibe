package chatservice_test

// PersistDiff unit tests, previously colocated with the message store they
// wrote through (internal/services/messagestore). They test the manager's
// diffing, not the store, so they live with the manager now.

import (
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/chatservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func TestUnit_ChatService_PersistDiff(t *testing.T) {
	ctx, db := setupDB(t)
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "")
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
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "")
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
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "")
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
