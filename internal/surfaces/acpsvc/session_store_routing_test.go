package acpsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/chatservice"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The session/list, session-workspace-resolve and serve-cwd paths used to hold
// their own SQL against message_indices/messages; they now go through
// runtimetypes' message store. These pin the observable output of each, so the
// reroute is provably behaviour-preserving rather than merely compiling.

const routingWorkspace = "ws-routing"

// seedRoutingSession creates a named acp-client index in workspaceID and
// returns its internal id.
func seedRoutingSession(t *testing.T, ctx context.Context, store runtimetypes.MessageStore, name string) string {
	t.Helper()
	internalID := "idx-" + uuid.NewString()
	require.NoError(t, store.CreateNamedMessageIndex(ctx, internalID, acpClientIdentity, name))
	return internalID
}

// TestUnit_ListSessions_ThroughStore_PinsRosterOutput pins session/list after
// the store reroute: only named acp-client indices of THIS workspace, freshest
// activity first, never-messaged last, UpdatedAt in RFC3339 UTC.
func TestUnit_ListSessions_ThroughStore_PinsRosterOutput(t *testing.T) {
	ctx, db := setupResolverDB(t)
	exec := db.WithoutTransaction()
	store := runtimetypes.NewMessageStore(exec, routingWorkspace)
	mgr := chatservice.NewManager(routingWorkspace)

	oldID := seedRoutingSession(t, ctx, store, "beam-old")
	newID := seedRoutingSession(t, ctx, store, "beam-new")
	seedRoutingSession(t, ctx, store, "beam-idle") // named, never messaged

	// An unnamed index: predates ACP naming, must not be listed.
	require.NoError(t, store.CreateMessageIndex(ctx, "idx-unnamed", acpClientIdentity))
	// Another workspace's session, and another identity's: neither is ours.
	otherWS := runtimetypes.NewMessageStore(exec, "ws-other")
	require.NoError(t, otherWS.CreateNamedMessageIndex(ctx, "idx-foreign", acpClientIdentity, "beam-foreign"))
	require.NoError(t, store.CreateNamedMessageIndex(ctx, "idx-cli", "cli", "beam-cli"))

	base := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	require.NoError(t, mgr.PersistDiff(ctx, exec, oldID, []taskengine.Message{
		{ID: uuid.NewString(), Role: "user", Content: "older thread", Timestamp: base},
	}))
	require.NoError(t, mgr.PersistDiff(ctx, exec, newID, []taskengine.Message{
		{ID: uuid.NewString(), Role: "user", Content: "newer thread", Timestamp: base.Add(time.Hour)},
	}))

	tr := &Transport{deps: Deps{DB: db, WorkspaceID: routingWorkspace}}
	resp, err := tr.ListSessions(ctx, libacp.ListSessionsRequest{})
	require.NoError(t, err)

	var ids []string
	for _, s := range resp.Sessions {
		ids = append(ids, string(s.SessionID))
	}
	require.Equal(t, []string{"beam-new", "beam-old", "beam-idle"}, ids,
		"freshest first, never-messaged last; no unnamed, no foreign workspace, no foreign identity")
	require.Equal(t, base.Add(time.Hour).Format(time.RFC3339), resp.Sessions[0].UpdatedAt)
	require.Equal(t, base.Format(time.RFC3339), resp.Sessions[1].UpdatedAt)
	require.Equal(t, "", resp.Sessions[2].UpdatedAt, "no messages, no activity time")
	require.Equal(t, "newer thread", resp.Sessions[0].Title, "the title still derives from the first user message")
	require.Equal(t, "beam-idle", resp.Sessions[2].Title, "no message, so the raw name is the fallback")
	require.Equal(t, "", resp.NextCursor, "one page holds them all")
}

// TestUnit_ListSessions_ThroughStore_PagesWithCursor pins that the roster's own
// cursor still walks every session exactly once after the reroute.
func TestUnit_ListSessions_ThroughStore_PagesWithCursor(t *testing.T) {
	ctx, db := setupResolverDB(t)
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), routingWorkspace)

	prev := listSessionsPageSize
	listSessionsPageSize = 2
	t.Cleanup(func() { listSessionsPageSize = prev })

	want := map[string]bool{}
	for i := range 5 {
		name := fmt.Sprintf("beam-%02d", i)
		seedRoutingSession(t, ctx, store, name)
		want[name] = true
	}

	tr := &Transport{deps: Deps{DB: db, WorkspaceID: routingWorkspace}}
	seen := map[string]int{}
	cursor := ""
	for pages := 0; ; pages++ {
		require.Less(t, pages, 20, "roster paging did not terminate")
		resp, err := tr.ListSessions(ctx, libacp.ListSessionsRequest{Cursor: cursor})
		require.NoError(t, err)
		for _, s := range resp.Sessions {
			seen[string(s.SessionID)]++
		}
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}
	require.Len(t, seen, len(want))
	for name := range want {
		require.Equal(t, 1, seen[name], "%s must appear exactly once across pages", name)
	}
}

// TestUnit_FirstUserMessageTitle_PagesPastTheFirstPage pins that the keyset
// walk finds a title that sits beyond one page — the whole-session read it
// replaced would have found it, so the paged read must too.
func TestUnit_FirstUserMessageTitle_PagesPastTheFirstPage(t *testing.T) {
	ctx, tr, sess, _ := newTitleTransport(t)
	exec := tr.deps.DB.WithoutTransaction()
	mgr := chatservice.NewManager(titleTestWorkspace)

	base := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	// Two and a half pages of non-title-worthy turns, then the prose.
	lead := sessionTitleScanPage*2 + 3
	msgs := make([]taskengine.Message, 0, lead+1)
	for i := range lead {
		msgs = append(msgs, taskengine.Message{
			ID: uuid.NewString(), Role: "user", Content: "/doctor",
			Timestamp: base.Add(time.Duration(i) * time.Second),
		})
	}
	msgs = append(msgs, taskengine.Message{
		ID: uuid.NewString(), Role: "user", Content: "the retry loop never backs off",
		Timestamp: base.Add(time.Duration(lead) * time.Second),
	})
	require.NoError(t, mgr.PersistDiff(ctx, exec, sess.InternalSessionID, msgs))

	require.Equal(t, "the retry loop never backs off",
		tr.firstUserMessageTitle(ctx, exec, sess.InternalSessionID),
		"the walk must cross page boundaries, not stop at the first page")
	require.Equal(t, "the retry loop never backs off", tr.sessionInfoTitle(ctx, sess.InternalSessionID))
}

// TestUnit_FirstUserMessageTitle_PagesThroughTimestampCollisions pins the
// tiebreaker end to end: one PersistDiff batch stamps identical timestamps, so
// a cursor without the message id would loop on or skip the boundary page.
func TestUnit_FirstUserMessageTitle_PagesThroughTimestampCollisions(t *testing.T) {
	ctx, tr, sess, _ := newTitleTransport(t)
	exec := tr.deps.DB.WithoutTransaction()
	mgr := chatservice.NewManager(titleTestWorkspace)

	stamp := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	lead := sessionTitleScanPage * 2
	msgs := make([]taskengine.Message, 0, lead+1)
	for range lead {
		msgs = append(msgs, taskengine.Message{
			ID: uuid.NewString(), Role: "user", Content: "/doctor", Timestamp: stamp,
		})
	}
	msgs = append(msgs, taskengine.Message{
		ID: uuid.NewString(), Role: "user", Content: "everything shares one instant", Timestamp: stamp,
	})
	require.NoError(t, mgr.PersistDiff(ctx, exec, sess.InternalSessionID, msgs))

	require.Equal(t, "everything shares one instant",
		tr.firstUserMessageTitle(ctx, exec, sess.InternalSessionID),
		"a stream with a single timestamp must still page to its end")
}

// TestUnit_ServeSessionCwd_ThroughStore pins the fileio serve path: an
// internal session id resolves through the store's name lookup to the cwd
// persisted under that name, and every miss degrades to "" rather than erroring.
func TestUnit_ServeSessionCwd_ThroughStore(t *testing.T) {
	ctx, db := setupResolverDB(t)
	exec := db.WithoutTransaction()
	store := runtimetypes.NewMessageStore(exec, routingWorkspace)

	internalID := seedRoutingSession(t, ctx, store, "beam-cwd")
	root := t.TempDir()
	raw, err := json.Marshal(sessionCwdRecord{Cwd: root})
	require.NoError(t, err)
	require.NoError(t, runtimetypes.New(exec).SetKV(ctx, acpSessionCwdKVPrefix+"beam-cwd", raw))

	require.Equal(t, root, serveSessionCwd(ctx, db, internalID))
	require.Equal(t, "", serveSessionCwd(ctx, db, "idx-does-not-exist"), "an unknown id is not an error, it is no cwd")

	// An index with no name cannot key the cwd record.
	require.NoError(t, store.CreateMessageIndex(ctx, "idx-nameless", acpClientIdentity))
	require.Equal(t, "", serveSessionCwd(ctx, db, "idx-nameless"))

	// A named index with no stored record is also "".
	other := seedRoutingSession(t, ctx, store, "beam-nocwd")
	require.Equal(t, "", serveSessionCwd(ctx, db, other))

	// The resolver wired over it hands back the stored cwd when it is allowed
	// by the roots, which is what the local_fs tool actually consumes.
	roots, err := vfs.NewFactory(root)
	require.NoError(t, err)
	resolve := NewServeCwdResolver(db, roots, nil)
	require.Equal(t, root, resolve(context.WithValue(ctx, runtimetypes.SessionIDContextKey, internalID)))
}
