package acpsvc

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/chatservice"
	"github.com/contenox/beam/internal/services/messagestore"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Session title resolution order: operator /rename override, then the
// first-user-message heuristic, then the raw session name.

const titleTestWorkspace = "ws-title-test"

// newTitleTransport builds a Transport over a real SQLite DB with one named,
// empty ACP session, returning the entry plus the minted `beam-<uuid>` name.
func newTitleTransport(t *testing.T) (context.Context, *Transport, *sessionEntry, string) {
	t.Helper()
	ctx, db := setupResolverDB(t)
	internalID := "idx-" + uuid.NewString()
	sessionName := "beam-" + uuid.NewString()

	store := messagestore.New(db.WithoutTransaction(), titleTestWorkspace)
	require.NoError(t, store.CreateNamedMessageIndex(ctx, internalID, "acp-client", sessionName))

	tr := &Transport{deps: Deps{DB: db, WorkspaceID: titleTestWorkspace}}
	sess := &sessionEntry{WorkspaceID: titleTestWorkspace, InternalSessionID: internalID}
	return ctx, tr, sess, sessionName
}

// TestUnit_SessionTitleOverride_RoundTripsAndClears pins the storage: write
// round-trips, an empty title clears rather than storing a blank, and an
// unrenamed session reads as "".
func TestUnit_SessionTitleOverride_RoundTripsAndClears(t *testing.T) {
	ctx, db := setupResolverDB(t)
	store := runtimetypes.New(db.WithoutTransaction())

	require.Equal(t, "", sessionTitleOverride(ctx, store, "idx-1"), "an unrenamed session has no override")
	require.Equal(t, "", sessionTitleOverride(ctx, store, ""), "no session id, no title")

	require.NoError(t, setSessionTitleOverride(ctx, store, "idx-1", "rewrite the ingest retry"))
	require.Equal(t, "rewrite the ingest retry", sessionTitleOverride(ctx, store, "idx-1"))

	// Clearing twice is not an error: a missing key is the state asked for.
	require.NoError(t, setSessionTitleOverride(ctx, store, "idx-1", ""))
	require.Equal(t, "", sessionTitleOverride(ctx, store, "idx-1"))
	require.NoError(t, setSessionTitleOverride(ctx, store, "idx-1", ""))

	require.Error(t, setSessionTitleOverride(ctx, store, "", "orphan"), "a title needs a session to belong to")

	// Overlong titles are clipped on the way out.
	long := ""
	for range 200 {
		long += "x"
	}
	require.NoError(t, setSessionTitleOverride(ctx, store, "idx-2", long))
	require.LessOrEqual(t, len([]rune(sessionTitleOverride(ctx, store, "idx-2"))), sessionListTitleMaxLen+3)
}

// TestUnit_HandleRename_SetsShowsAndResets walks /rename's three shapes.
func TestUnit_HandleRename_SetsShowsAndResets(t *testing.T) {
	ctx, tr, sess, _ := newTitleTransport(t)

	out, err := tr.handleRename(ctx, sess, "")
	require.NoError(t, err)
	require.Contains(t, out, "no title yet", "an untitled session says so rather than showing a blank")

	out, err = tr.handleRename(ctx, sess, "  rewrite the ingest retry  ")
	require.NoError(t, err)
	require.Contains(t, out, "rewrite the ingest retry")
	require.Equal(t, "rewrite the ingest retry", tr.sessionInfoTitle(ctx, sess.InternalSessionID),
		"the title the push and the roster read must be the one just set")

	out, err = tr.handleRename(ctx, sess, "")
	require.NoError(t, err)
	require.Contains(t, out, "rewrite the ingest retry", "no argument reports the current title")

	out, err = tr.handleRename(ctx, sess, "-")
	require.NoError(t, err)
	require.Contains(t, out, "reset")
	require.Equal(t, "", tr.sessionInfoTitle(ctx, sess.InternalSessionID))

	// A session with no durable record cannot carry a title.
	_, err = tr.handleRename(ctx, &sessionEntry{WorkspaceID: titleTestWorkspace}, "orphan")
	require.Error(t, err)
}

// TestUnit_SessionTitle_OverrideBeatsTheDerivedTitle pins the precedence on
// both readers, session/list's Title and the live session_info_update's.
func TestUnit_SessionTitle_OverrideBeatsTheDerivedTitle(t *testing.T) {
	ctx, tr, sess, sessionName := newTitleTransport(t)

	exec := tr.deps.DB.WithoutTransaction()
	mgr := chatservice.NewManager(titleTestWorkspace)

	// No messages: the roster falls back to the raw `beam-<uuid>` name.
	require.Equal(t, sessionName, tr.sessionListTitle(ctx, mgr, exec, sess.InternalSessionID, sessionName))
	require.Equal(t, "", tr.sessionInfoTitle(ctx, sess.InternalSessionID), "no message, no live title")

	// One user message: both readers derive the same subject.
	require.NoError(t, mgr.PersistDiff(ctx, exec, sess.InternalSessionID, []taskengine.Message{
		{ID: uuid.NewString(), Role: "user", Content: "why does the ingest retry loop forever?", Timestamp: time.Now().UTC()},
	}))
	const derived = "why does the ingest retry loop forever?"
	require.Equal(t, derived, tr.sessionListTitle(ctx, mgr, exec, sess.InternalSessionID, sessionName))
	require.Equal(t, derived, tr.sessionInfoTitle(ctx, sess.InternalSessionID))

	// The operator's own name wins over the derived one, on both readers.
	_, err := tr.handleRename(ctx, sess, "ingest retry bug")
	require.NoError(t, err)
	require.Equal(t, "ingest retry bug", tr.sessionListTitle(ctx, mgr, exec, sess.InternalSessionID, sessionName))
	require.Equal(t, "ingest retry bug", tr.sessionInfoTitle(ctx, sess.InternalSessionID))

	// Resetting falls back to the derived title, not the raw name.
	_, err = tr.handleRename(ctx, sess, "-")
	require.NoError(t, err)
	require.Equal(t, derived, tr.sessionListTitle(ctx, mgr, exec, sess.InternalSessionID, sessionName))
}

// TestUnit_FirstUserMessageTitle_SkipsCommandShapedMessages pins that a
// command-shaped user message (known or not) is skipped when deriving a
// title, falling back to "" when every user message so far is a command.
func TestUnit_FirstUserMessageTitle_SkipsCommandShapedMessages(t *testing.T) {
	mgr := chatservice.NewManager(titleTestWorkspace)

	tests := []struct {
		name     string
		messages []string // user-role message bodies, in order
		want     string
	}{
		{
			name:     "command first, then prose: prose wins",
			messages: []string{"/doctor", "why does the ingest retry loop forever?"},
			want:     "why does the ingest retry loop forever?",
		},
		{
			name:     "prose first: unchanged by a later command",
			messages: []string{"why does the ingest retry loop forever?", "/doctor"},
			want:     "why does the ingest retry loop forever?",
		},
		{
			name:     "only commands so far: falls back to empty",
			messages: []string{"/doctor", "/model", "/clear"},
			want:     "",
		},
		{
			name:     "unrecognized but command-shaped: also skipped",
			messages: []string{"/frobnicate", "actually just fix the retry loop"},
			want:     "actually just fix the retry loop",
		},
		{
			name:     "a path is not a command: legitimate title",
			messages: []string{"/etc/passwd contains x"},
			want:     "/etc/passwd contains x",
		},
		{
			name:     "multiline prose: whitespace-collapsing clip is unaffected",
			messages: []string{"/doctor", "line one\nline two\n   line three"},
			want:     "line one line two line three",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, tr, sess, _ := newTitleTransport(t)
			exec := tr.deps.DB.WithoutTransaction()

			now := time.Now().UTC()
			msgs := make([]taskengine.Message, len(tc.messages))
			for i, body := range tc.messages {
				msgs[i] = taskengine.Message{
					ID:        uuid.NewString(),
					Role:      "user",
					Content:   body,
					Timestamp: now.Add(time.Duration(i) * time.Millisecond),
				}
			}
			require.NoError(t, mgr.PersistDiff(ctx, exec, sess.InternalSessionID, msgs))

			require.Equal(t, tc.want, firstUserMessageTitle(ctx, mgr, exec, sess.InternalSessionID))
			// sessionInfoTitle shares the same derivation; must agree.
			require.Equal(t, tc.want, tr.sessionInfoTitle(ctx, sess.InternalSessionID))
		})
	}
}

// TestUnit_SessionListTitle_RederivesOnceRealProseArrives pins that a roster
// title upgrades from the fallback name to real prose without a rename.
func TestUnit_SessionListTitle_RederivesOnceRealProseArrives(t *testing.T) {
	ctx, tr, sess, sessionName := newTitleTransport(t)
	exec := tr.deps.DB.WithoutTransaction()
	mgr := chatservice.NewManager(titleTestWorkspace)

	require.NoError(t, mgr.PersistDiff(ctx, exec, sess.InternalSessionID, []taskengine.Message{
		{ID: uuid.NewString(), Role: "user", Content: "/doctor", Timestamp: time.Now().UTC()},
		{ID: uuid.NewString(), Role: "assistant", Content: "all providers healthy", Timestamp: time.Now().UTC()},
	}))
	require.Equal(t, sessionName, tr.sessionListTitle(ctx, mgr, exec, sess.InternalSessionID, sessionName),
		"a command-only session falls back to the raw name, not the command text")
	require.Equal(t, "", tr.sessionInfoTitle(ctx, sess.InternalSessionID))

	require.NoError(t, mgr.PersistDiff(ctx, exec, sess.InternalSessionID, []taskengine.Message{
		{ID: uuid.NewString(), Role: "user", Content: "the retry loop never backs off", Timestamp: time.Now().UTC().Add(time.Millisecond)},
	}))
	const derived = "the retry loop never backs off"
	require.Equal(t, derived, tr.sessionListTitle(ctx, mgr, exec, sess.InternalSessionID, sessionName))
	require.Equal(t, derived, tr.sessionInfoTitle(ctx, sess.InternalSessionID))
}

// TestUnit_RenameCommand_IsAdvertisedRecognizedAndPushesSessionInfo pins that
// /rename is advertised, parses, and is the only command pushing session_info_update.
func TestUnit_RenameCommand_IsAdvertisedRecognizedAndPushesSessionInfo(t *testing.T) {
	var advertised bool
	for _, c := range allACPCommands() {
		if c.Name == "rename" {
			advertised = true
			require.NotEmpty(t, c.Description)
			require.NotNil(t, c.Input, "rename takes an argument, so it must hint at one")
		}
	}
	require.True(t, advertised, "/rename is not in the advertised command set")

	name, args, ok := parseCommand("/rename the ingest rewrite")
	require.True(t, ok)
	require.Equal(t, "rename", name)
	require.Equal(t, "the ingest rewrite", args)

	require.True(t, commandUpdatesSessionInfo("rename"), "a rename must push the new label to attached clients")
	for _, other := range []string{"help", "clear", "compact", "model", "mission"} {
		require.False(t, commandUpdatesSessionInfo(other), "%s does not change the session's label", other)
	}
	require.False(t, commandRewritesHistory("rename"), "a rename is an ordinary durable exchange")
}
