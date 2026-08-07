package runtimetypes_test

// message_indices / messages store tests, over the production SQLite backend
// (same no-Docker idiom as event_firings_test.go). The store takes an Exec, so
// these open a DBManager only to hand out WithoutTransaction / WithTransaction.

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

func setupMessageDB(t *testing.T) (context.Context, libdb.DBManager) {
	t.Helper()
	ctx := context.TODO()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "messages.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return ctx, db
}

func TestUnit_Messages_CreateAndListIndices(t *testing.T) {
	ctx, db := setupMessageDB(t)
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "")

	require.NoError(t, store.CreateMessageIndex(ctx, "idx-alice", "alice"))
	require.NoError(t, store.CreateMessageIndex(ctx, "idx-alice-2", "alice"))

	ids, err := store.ListMessageIndices(ctx, "alice")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"idx-alice", "idx-alice-2"}, ids)

	ids, err = store.ListMessageIndices(ctx, "bob")
	require.NoError(t, err)
	require.Empty(t, ids)
}

func TestUnit_Messages_AppendAndList(t *testing.T) {
	ctx, db := setupMessageDB(t)
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "")

	require.NoError(t, store.CreateMessageIndex(ctx, "idx-msgs", "alice"))

	now := time.Now().UTC()
	msgs := []*runtimetypes.Message{
		{ID: "m1", IDX: "idx-msgs", Payload: []byte(`{"role":"user","content":"hello"}`), AddedAt: now},
		{ID: "m2", IDX: "idx-msgs", Payload: []byte(`{"role":"assistant","content":"hi there"}`), AddedAt: now.Add(time.Millisecond)},
	}
	require.NoError(t, store.AppendMessages(ctx, msgs...))

	listed, err := store.ListMessages(ctx, "idx-msgs")
	require.NoError(t, err)
	require.Len(t, listed, 2)
	require.Equal(t, "m1", listed[0].ID)
	require.Equal(t, "m2", listed[1].ID)

	count, err := store.CountMessages(ctx, "idx-msgs")
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

func TestUnit_Messages_LastMessage(t *testing.T) {
	ctx, db := setupMessageDB(t)
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "")

	require.NoError(t, store.CreateMessageIndex(ctx, "idx-last", "alice"))

	now := time.Now().UTC()
	require.NoError(t, store.AppendMessages(ctx,
		&runtimetypes.Message{ID: "first", IDX: "idx-last", Payload: []byte(`"first"`), AddedAt: now},
		&runtimetypes.Message{ID: "last", IDX: "idx-last", Payload: []byte(`"last"`), AddedAt: now.Add(time.Millisecond)},
	))

	msg, err := store.LastMessage(ctx, "idx-last")
	require.NoError(t, err)
	require.Equal(t, "last", msg.ID)

	_, err = store.LastMessage(ctx, "idx-empty")
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

func TestUnit_Messages_DeleteMessages(t *testing.T) {
	ctx, db := setupMessageDB(t)
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "")

	require.NoError(t, store.CreateMessageIndex(ctx, "idx-del", "alice"))
	require.NoError(t, store.AppendMessages(ctx,
		&runtimetypes.Message{ID: "d1", IDX: "idx-del", Payload: []byte(`"x"`), AddedAt: time.Now().UTC()},
	))

	require.NoError(t, store.DeleteMessages(ctx, "idx-del"))

	listed, err := store.ListMessages(ctx, "idx-del")
	require.NoError(t, err)
	require.Empty(t, listed)
}

// TestUnit_Messages_WorkspaceIsolation pins the store's construction-time
// scope: an index created in one workspace is invisible to another's reads,
// with no filter argument anywhere in the call.
func TestUnit_Messages_WorkspaceIsolation(t *testing.T) {
	ctx, db := setupMessageDB(t)
	storeA := runtimetypes.NewMessageStore(db.WithoutTransaction(), "ws-a")
	storeB := runtimetypes.NewMessageStore(db.WithoutTransaction(), "ws-b")

	require.NoError(t, storeA.CreateNamedMessageIndex(ctx, "idx-a", "alice", "shared-name"))
	require.NoError(t, storeB.CreateNamedMessageIndex(ctx, "idx-b", "alice", "shared-name"))

	idsA, err := storeA.ListMessageIndices(ctx, "alice")
	require.NoError(t, err)
	require.Equal(t, []string{"idx-a"}, idsA)

	sessA, err := storeA.GetMessageSessionByName(ctx, "alice", "shared-name")
	require.NoError(t, err)
	require.Equal(t, "idx-a", sessA.ID, "the same name in another workspace is a different session")

	_, err = runtimetypes.NewMessageStore(db.WithoutTransaction(), "ws-c").
		GetMessageSessionByName(ctx, "alice", "shared-name")
	require.ErrorIs(t, err, libdb.ErrNotFound, "a workspace that never created it sees nothing, not another's row")

	require.ErrorIs(t, storeB.DeleteMessageIndex(ctx, "idx-a", "alice"), libdb.ErrNotFound,
		"a delete cannot reach across the workspace boundary")
}

// TestUnit_Messages_SessionsOrderedByActivity pins the listing contract the
// session picker renders: most recently active first, never-used sessions last.
func TestUnit_Messages_SessionsOrderedByActivity(t *testing.T) {
	ctx, db := setupMessageDB(t)
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "ws")

	require.NoError(t, store.CreateNamedMessageIndex(ctx, "idx-old", "alice", "old"))
	require.NoError(t, store.CreateNamedMessageIndex(ctx, "idx-new", "alice", "new"))
	require.NoError(t, store.CreateNamedMessageIndex(ctx, "idx-idle", "alice", "idle"))

	base := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	require.NoError(t, store.AppendMessages(ctx,
		&runtimetypes.Message{ID: "o1", IDX: "idx-old", Payload: []byte(`"x"`), AddedAt: base},
		&runtimetypes.Message{ID: "n1", IDX: "idx-new", Payload: []byte(`"x"`), AddedAt: base.Add(time.Hour)},
		&runtimetypes.Message{ID: "n2", IDX: "idx-new", Payload: []byte(`"x"`), AddedAt: base.Add(2 * time.Hour)},
	))

	sessions, err := store.ListMessageSessions(ctx, "alice")
	require.NoError(t, err)
	require.Len(t, sessions, 3)
	require.Equal(t, "idx-new", sessions[0].ID)
	require.Equal(t, 2, sessions[0].MessageCount)
	require.Equal(t, base.Add(2*time.Hour), sessions[0].UpdatedAt)
	require.Equal(t, "idx-old", sessions[1].ID)
	require.Equal(t, "idx-idle", sessions[2].ID, "a session with no messages sorts last")
	require.True(t, sessions[2].UpdatedAt.IsZero(), "no messages means no activity time")
	require.Equal(t, 0, sessions[2].MessageCount)
}

// TestUnit_Messages_RenameSession pins the rename and its not-found signal.
func TestUnit_Messages_RenameSession(t *testing.T) {
	ctx, db := setupMessageDB(t)
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "ws")

	require.NoError(t, store.CreateNamedMessageIndex(ctx, "idx-ren", "alice", "before"))
	require.NoError(t, store.RenameMessageSession(ctx, "idx-ren", "after"))

	si, err := store.GetMessageSessionByName(ctx, "alice", "after")
	require.NoError(t, err)
	require.Equal(t, "idx-ren", si.ID)

	_, err = store.GetMessageSessionByName(ctx, "alice", "before")
	require.ErrorIs(t, err, libdb.ErrNotFound)

	require.ErrorIs(t, store.RenameMessageSession(ctx, "no-such-idx", "x"), libdb.ErrNotFound)
}

func TestUnit_Messages_WithTransaction(t *testing.T) {
	ctx, db := setupMessageDB(t)
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "")

	require.NoError(t, store.CreateMessageIndex(ctx, "idx-tx", "eve"))

	t.Run("rollback discards messages", func(t *testing.T) {
		exec, _, release, err := db.WithTransaction(ctx)
		require.NoError(t, err)

		txStore := runtimetypes.NewMessageStore(exec, "")
		require.NoError(t, txStore.AppendMessages(ctx, &runtimetypes.Message{
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

		txStore := runtimetypes.NewMessageStore(exec, "")
		require.NoError(t, txStore.AppendMessages(ctx, &runtimetypes.Message{
			ID: "committed-msg", IDX: "idx-tx", Payload: []byte(`"committed"`), AddedAt: time.Now().UTC(),
		}))
		require.NoError(t, commit(ctx))

		msgs, err := store.ListMessages(ctx, "idx-tx")
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		require.Equal(t, "committed-msg", msgs[0].ID)
	})
}

// TestUnit_Messages_GetIndexName pins the id -> name lookup both the ACP serve
// path and the CLI session resolver now share, including the unnamed case
// (a row with no name is found, not missing) and the not-found signal.
func TestUnit_Messages_GetIndexName(t *testing.T) {
	ctx, db := setupMessageDB(t)
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "ws")

	require.NoError(t, store.CreateNamedMessageIndex(ctx, "idx-named", "acp-client", "zed-42"))
	require.NoError(t, store.CreateMessageIndex(ctx, "idx-unnamed", "acp-client"))

	name, err := store.GetMessageIndexName(ctx, "idx-named")
	require.NoError(t, err)
	require.Equal(t, "zed-42", name)

	name, err = store.GetMessageIndexName(ctx, "idx-unnamed")
	require.NoError(t, err)
	require.Equal(t, "", name, "an unnamed index exists; it is not a missing row")

	_, err = store.GetMessageIndexName(ctx, "no-such-idx")
	require.ErrorIs(t, err, libdb.ErrNotFound)

	// Documented as key-on-primary-key: session ids are UUIDs, unique across
	// workspaces, so another workspace's store resolves the same id.
	other, err := runtimetypes.NewMessageStore(db.WithoutTransaction(), "other-ws").
		GetMessageIndexName(ctx, "idx-named")
	require.NoError(t, err)
	require.Equal(t, "zed-42", other)
}

// TestUnit_Messages_ListAllIndices pins the deliberately unscoped inventory the
// CLI renders: every workspace, every identity, with message counts.
func TestUnit_Messages_ListAllIndices(t *testing.T) {
	ctx, db := setupMessageDB(t)
	storeA := runtimetypes.NewMessageStore(db.WithoutTransaction(), "ws-a")
	storeB := runtimetypes.NewMessageStore(db.WithoutTransaction(), "ws-b")

	require.NoError(t, storeA.CreateNamedMessageIndex(ctx, "idx-a", "acp-client", "zed-1"))
	require.NoError(t, storeB.CreateMessageIndex(ctx, "idx-b", "cli"))

	now := time.Now().UTC()
	require.NoError(t, storeA.AppendMessages(ctx,
		&runtimetypes.Message{ID: "a1", IDX: "idx-a", Payload: []byte(`"x"`), AddedAt: now},
		&runtimetypes.Message{ID: "a2", IDX: "idx-a", Payload: []byte(`"x"`), AddedAt: now},
	))

	rows, err := runtimetypes.ListAllMessageIndices(ctx, db.WithoutTransaction())
	require.NoError(t, err)
	byID := map[string]runtimetypes.MessageIndexRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	require.Len(t, byID, 2, "both workspaces' rows, from one unscoped read")
	require.Equal(t, runtimetypes.MessageIndexRow{
		ID: "idx-a", Identity: "acp-client", WorkspaceID: "ws-a", Name: "zed-1", MessageCount: 2,
	}, byID["idx-a"])
	require.Equal(t, runtimetypes.MessageIndexRow{
		ID: "idx-b", Identity: "cli", WorkspaceID: "ws-b", Name: "", MessageCount: 0,
	}, byID["idx-b"])
}

// TestUnit_Messages_ResolveIndexWorkspace pins the lookup that PICKS a
// workspace (so it cannot be workspace-scoped): the same session name in two
// workspaces resolves to the busier one, and identity still narrows.
func TestUnit_Messages_ResolveIndexWorkspace(t *testing.T) {
	ctx, db := setupMessageDB(t)
	exec := db.WithoutTransaction()
	storeA := runtimetypes.NewMessageStore(exec, "ws-a")
	storeB := runtimetypes.NewMessageStore(exec, "ws-b")

	require.NoError(t, storeA.CreateNamedMessageIndex(ctx, "idx-a", "acp-client", "zed-42"))
	require.NoError(t, storeB.CreateNamedMessageIndex(ctx, "idx-b", "acp-client", "zed-42"))

	now := time.Now().UTC()
	require.NoError(t, storeB.AppendMessages(ctx,
		&runtimetypes.Message{ID: "b1", IDX: "idx-b", Payload: []byte(`"x"`), AddedAt: now},
	))

	ws, err := runtimetypes.ResolveMessageIndexWorkspace(ctx, exec, "acp-client", "zed-42")
	require.NoError(t, err)
	require.Equal(t, "ws-b", ws, "the busier index wins when a name repeats across workspaces")

	_, err = runtimetypes.ResolveMessageIndexWorkspace(ctx, exec, "cli", "zed-42")
	require.ErrorIs(t, err, libdb.ErrNotFound, "identity narrows the resolve")

	_, err = runtimetypes.ResolveMessageIndexWorkspace(ctx, exec, "acp-client", "no-such-session")
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

// appendProbeMessages fills stream with n messages whose ids sort in insert
// order, stamped by clock(i) so a test can choose distinct or colliding times.
func appendProbeMessages(t *testing.T, ctx context.Context, store runtimetypes.MessageStore, stream string, n int, clock func(int) time.Time) []string {
	t.Helper()
	ids := make([]string, 0, n)
	batch := make([]*runtimetypes.Message, 0, n)
	for i := range n {
		id := fmt.Sprintf("%s-m%04d", stream, i)
		ids = append(ids, id)
		batch = append(batch, &runtimetypes.Message{
			ID: id, IDX: stream, Payload: fmt.Appendf(nil, `{"n":%d}`, i), AddedAt: clock(i),
		})
	}
	require.NoError(t, store.AppendMessages(ctx, batch...))
	return ids
}

// drainPages walks every page of stream and returns the ids in page order.
// The walk itself is the assertion subject: gaps and duplicates are detected
// by the caller comparing this against the full listing.
func drainPages(t *testing.T, ctx context.Context, store runtimetypes.MessageStore, stream string, f runtimetypes.MessagePageFilter) []string {
	t.Helper()
	var got []string
	for pages := 0; ; pages++ {
		require.Less(t, pages, 100, "page walk did not terminate — the cursor is not advancing")
		page, err := store.ListMessagesPage(ctx, stream, f)
		require.NoError(t, err)
		for _, m := range page {
			got = append(got, m.ID)
		}
		if len(page) == 0 {
			return got
		}
		f.After = page[len(page)-1].Cursor()
	}
}

// TestUnit_Messages_KeysetPaginationCoversEveryRowOnce pins the property the
// whole feature exists for: walking pages yields the full stream, in order,
// with no row dropped and none repeated.
func TestUnit_Messages_KeysetPaginationCoversEveryRowOnce(t *testing.T) {
	ctx, db := setupMessageDB(t)
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "ws")
	require.NoError(t, store.CreateMessageIndex(ctx, "idx-page", "alice"))

	base := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	// 25 rows over a page size of 7: the last page is deliberately short.
	want := appendProbeMessages(t, ctx, store, "idx-page", 25, func(i int) time.Time {
		return base.Add(time.Duration(i) * time.Second)
	})

	got := drainPages(t, ctx, store, "idx-page", runtimetypes.MessagePageFilter{Limit: 7})
	require.Equal(t, want, got, "every row exactly once, oldest first")

	// Backwards is the same stream reversed, also exactly once.
	gotBack := drainPages(t, ctx, store, "idx-page", runtimetypes.MessagePageFilter{Limit: 7, Backwards: true})
	reversed := make([]string, 0, len(want))
	for i := len(want) - 1; i >= 0; i-- {
		reversed = append(reversed, want[i])
	}
	require.Equal(t, reversed, gotBack, "every row exactly once, newest first")
}

// TestUnit_Messages_KeysetPaginationSurvivesTimestampCollisions is the reason
// MessageCursor carries an ID. One AppendMessages batch stamps every
// zero-timestamped message the same instant, so a page boundary landing inside
// a tie is the NORMAL case, not an edge case: with a timestamp-only cursor the
// tied rows are either re-read forever or skipped wholesale.
func TestUnit_Messages_KeysetPaginationSurvivesTimestampCollisions(t *testing.T) {
	ctx, db := setupMessageDB(t)
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "ws")
	require.NoError(t, store.CreateMessageIndex(ctx, "idx-tie", "alice"))

	base := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	// 12 rows in 3 timestamp groups of 4 — every page boundary at limit 3, 5
	// and 7 lands inside a tie.
	appendProbeMessages(t, ctx, store, "idx-tie", 12, func(i int) time.Time {
		return base.Add(time.Duration(i/4) * time.Second)
	})

	full, err := store.ListMessagesPage(ctx, "idx-tie", runtimetypes.MessagePageFilter{Limit: 100})
	require.NoError(t, err)
	require.Len(t, full, 12)
	want := make([]string, 0, 12)
	for _, m := range full {
		want = append(want, m.ID)
	}

	for _, limit := range []int{1, 2, 3, 5, 7, 11} {
		got := drainPages(t, ctx, store, "idx-tie", runtimetypes.MessagePageFilter{Limit: limit})
		require.Equal(t, want, got, "limit %d: tied rows split across a boundary must not drop or repeat", limit)
		require.Len(t, got, len(unique(got)), "limit %d: no duplicates", limit)
	}

	// A batch appended with no timestamps at all: every row shares one instant,
	// so the whole stream is one tie group.
	require.NoError(t, store.CreateMessageIndex(ctx, "idx-same", "alice"))
	same := make([]*runtimetypes.Message, 0, 9)
	for i := range 9 {
		same = append(same, &runtimetypes.Message{
			ID: fmt.Sprintf("s%02d", i), IDX: "idx-same", Payload: []byte(`"x"`),
		})
	}
	require.NoError(t, store.AppendMessages(ctx, same...))
	for _, m := range same {
		require.Equal(t, same[0].AddedAt, m.AddedAt, "one batch, one stamp — the collision is by construction")
	}
	got := drainPages(t, ctx, store, "idx-same", runtimetypes.MessagePageFilter{Limit: 2})
	require.Len(t, got, 9)
	require.Len(t, unique(got), 9, "a single-timestamp stream still pages exactly once per row")
}

func unique(ids []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// TestUnit_Messages_PageLimitDefaultAndCeiling pins the two bounds: an unnamed
// limit is DefaultMessagePageLimit, and an oversized one is clamped to
// MaxMessagePageLimit rather than degrading into a whole-session read.
func TestUnit_Messages_PageLimitDefaultAndCeiling(t *testing.T) {
	ctx, db := setupMessageDB(t)
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "ws")
	require.NoError(t, store.CreateMessageIndex(ctx, "idx-lim", "alice"))

	base := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	total := runtimetypes.DefaultMessagePageLimit + 5
	appendProbeMessages(t, ctx, store, "idx-lim", total, func(i int) time.Time {
		return base.Add(time.Duration(i) * time.Millisecond)
	})

	for _, tc := range []struct {
		name  string
		limit int
	}{
		{"zero means default", 0},
		{"negative means default", -3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := store.ListMessagesPage(ctx, "idx-lim", runtimetypes.MessagePageFilter{Limit: tc.limit})
			require.NoError(t, err)
			require.Len(t, page, runtimetypes.DefaultMessagePageLimit)
		})
	}

	// The ceiling: ask for more than the max and the page is still capped.
	require.NoError(t, store.CreateMessageIndex(ctx, "idx-big", "alice"))
	appendProbeMessages(t, ctx, store, "idx-big", runtimetypes.MaxMessagePageLimit+10, func(i int) time.Time {
		return base.Add(time.Duration(i) * time.Millisecond)
	})
	page, err := store.ListMessagesPage(ctx, "idx-big", runtimetypes.MessagePageFilter{Limit: runtimetypes.MaxMessagePageLimit * 10})
	require.NoError(t, err)
	require.Len(t, page, runtimetypes.MaxMessagePageLimit, "an oversized limit is clamped, not honoured")
}

// TestUnit_Messages_PageStreamIsolation pins that a page never leaks another
// stream's rows — the messages-level scoping is the idx_id, since message rows
// carry no workspace of their own.
func TestUnit_Messages_PageStreamIsolation(t *testing.T) {
	ctx, db := setupMessageDB(t)
	execA := db.WithoutTransaction()
	storeA := runtimetypes.NewMessageStore(execA, "ws-a")
	storeB := runtimetypes.NewMessageStore(execA, "ws-b")

	require.NoError(t, storeA.CreateMessageIndex(ctx, "idx-a", "alice"))
	require.NoError(t, storeB.CreateMessageIndex(ctx, "idx-b", "alice"))

	base := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	// Interleaved in time so a boundary in one stream sits between the other's
	// rows: a query that forgot idx_id would splice them together.
	clock := func(i int) time.Time { return base.Add(time.Duration(2*i) * time.Second) }
	wantA := appendProbeMessages(t, ctx, storeA, "idx-a", 10, clock)
	appendProbeMessages(t, ctx, storeB, "idx-b", 10, func(i int) time.Time { return clock(i).Add(time.Second) })

	got := drainPages(t, ctx, storeA, "idx-a", runtimetypes.MessagePageFilter{Limit: 3})
	require.Equal(t, wantA, got, "only this stream's rows, at every page boundary")

	// The store's workspace is not what scopes a message read — the stream is —
	// so the assertion above must hold from either workspace's store.
	gotFromB := drainPages(t, ctx, storeB, "idx-a", runtimetypes.MessagePageFilter{Limit: 3})
	require.Equal(t, wantA, gotFromB)
}

// TestUnit_Messages_AppendNormalizesToUTC pins that added_at is stored as UTC
// regardless of the caller's zone. On SQLite added_at is the TEXT of the bound
// time's String(), so a non-UTC zone would sort by local wall clock and every
// keyset boundary comparison would be wrong.
func TestUnit_Messages_AppendNormalizesToUTC(t *testing.T) {
	ctx, db := setupMessageDB(t)
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "ws")
	require.NoError(t, store.CreateMessageIndex(ctx, "idx-tz", "alice"))

	base := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	ahead := time.FixedZone("AHEAD", 9*60*60)
	behind := time.FixedZone("BEHIND", -9*60*60)

	// Written out of order, in three zones, with wall clocks that disagree with
	// the instants: only a UTC-normalized column sorts these correctly.
	require.NoError(t, store.AppendMessages(ctx,
		&runtimetypes.Message{ID: "second", IDX: "idx-tz", Payload: []byte(`"2"`), AddedAt: base.Add(time.Hour).In(behind)},
		&runtimetypes.Message{ID: "third", IDX: "idx-tz", Payload: []byte(`"3"`), AddedAt: base.Add(2 * time.Hour)},
		&runtimetypes.Message{ID: "first", IDX: "idx-tz", Payload: []byte(`"1"`), AddedAt: base.In(ahead)},
	))

	got := drainPages(t, ctx, store, "idx-tz", runtimetypes.MessagePageFilter{Limit: 1})
	require.Equal(t, []string{"first", "second", "third"}, got, "instant order, not wall-clock order")

	listed, err := store.ListMessages(ctx, "idx-tz")
	require.NoError(t, err)
	require.Equal(t, []string{"first", "second", "third"},
		[]string{listed[0].ID, listed[1].ID, listed[2].ID},
		"the whole-session read agrees with the paged one")
	require.True(t, listed[0].AddedAt.Equal(base), "the instant survives the zone normalization")
}

// TestUnit_Messages_PageResumeFromExplicitCursor pins that a cursor is
// exclusive in both directions — the boundary row itself never reappears.
func TestUnit_Messages_PageResumeFromExplicitCursor(t *testing.T) {
	ctx, db := setupMessageDB(t)
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "ws")
	require.NoError(t, store.CreateMessageIndex(ctx, "idx-cur", "alice"))

	base := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	ids := appendProbeMessages(t, ctx, store, "idx-cur", 6, func(i int) time.Time {
		return base.Add(time.Duration(i) * time.Second)
	})

	first, err := store.ListMessagesPage(ctx, "idx-cur", runtimetypes.MessagePageFilter{Limit: 3})
	require.NoError(t, err)
	require.Equal(t, ids[:3], []string{first[0].ID, first[1].ID, first[2].ID})

	next, err := store.ListMessagesPage(ctx, "idx-cur", runtimetypes.MessagePageFilter{
		Limit: 3, After: first[2].Cursor(),
	})
	require.NoError(t, err)
	require.Equal(t, ids[3:], []string{next[0].ID, next[1].ID, next[2].ID}, "exclusive: the boundary row is not repeated")

	back, err := store.ListMessagesPage(ctx, "idx-cur", runtimetypes.MessagePageFilter{
		Limit: 10, After: first[2].Cursor(), Backwards: true,
	})
	require.NoError(t, err)
	require.Equal(t, []string{ids[1], ids[0]}, []string{back[0].ID, back[1].ID}, "backwards is exclusive too")

	// An appended row lands past the cursor, never behind it: keyset paging
	// cannot be shifted by a concurrent write the way OFFSET can.
	require.NoError(t, store.AppendMessages(ctx, &runtimetypes.Message{
		ID: "late", IDX: "idx-cur", Payload: []byte(`"late"`), AddedAt: base.Add(10 * time.Second),
	}))
	tail, err := store.ListMessagesPage(ctx, "idx-cur", runtimetypes.MessagePageFilter{
		Limit: 10, After: next[2].Cursor(),
	})
	require.NoError(t, err)
	require.Len(t, tail, 1)
	require.Equal(t, "late", tail[0].ID)
}

// TestUnit_Messages_PageEmptyStream pins the terminating condition callers
// loop on: an exhausted or never-written stream is an empty page, not an error.
func TestUnit_Messages_PageEmptyStream(t *testing.T) {
	ctx, db := setupMessageDB(t)
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "ws")
	require.NoError(t, store.CreateMessageIndex(ctx, "idx-none", "alice"))

	page, err := store.ListMessagesPage(ctx, "idx-none", runtimetypes.MessagePageFilter{Limit: 5})
	require.NoError(t, err)
	require.Empty(t, page)

	page, err = store.ListMessagesPage(ctx, "no-such-stream", runtimetypes.MessagePageFilter{Limit: 5})
	require.NoError(t, err)
	require.Empty(t, page)
}
