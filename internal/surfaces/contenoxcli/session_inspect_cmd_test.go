package contenoxcli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// The session inventory commands used to carry their own SQL against
// message_indices/messages; they now read through runtimetypes. These pin the
// rendered output, so the reroute is verifiably behaviour-preserving.

func setupInspectDB(t *testing.T) (context.Context, libdb.DBManager) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "inspect.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return ctx, db
}

// seedInspectSessions fills two workspaces with named and unnamed indices
// across two identities, and gives one of them messages.
func seedInspectSessions(t *testing.T, ctx context.Context, db libdb.DBManager) {
	t.Helper()
	exec := db.WithoutTransaction()
	wsA := runtimetypes.NewMessageStore(exec, "ws-a")
	wsB := runtimetypes.NewMessageStore(exec, "ws-b")

	require.NoError(t, wsA.CreateNamedMessageIndex(ctx, "idx-z1", "acp-client", "zed-1111aaaa2222"))
	require.NoError(t, wsA.CreateNamedMessageIndex(ctx, "idx-z2", "acp-client", "zed-3333bbbb4444"))
	require.NoError(t, wsB.CreateNamedMessageIndex(ctx, "idx-j1", "acp-client", "jetbrainsgoland-5555cccc6666"))
	require.NoError(t, wsB.CreateMessageIndex(ctx, "idx-anon", "cli"))

	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	require.NoError(t, wsA.AppendMessages(ctx,
		&runtimetypes.Message{ID: "m1", IDX: "idx-z1", Payload: []byte(`"x"`), AddedAt: now},
		&runtimetypes.Message{ID: "m2", IDX: "idx-z1", Payload: []byte(`"x"`), AddedAt: now.Add(time.Second)},
		&runtimetypes.Message{ID: "m3", IDX: "idx-z2", Payload: []byte(`"x"`), AddedAt: now},
	))
}

// TestUnit_QuerySessionIndex_ReadsEveryWorkspace pins the inventory read: every
// workspace, every identity, unnamed rows included, with message counts. The
// unscoped read is the point of the command — a workspace-scoped store would
// silently hide most of this.
func TestUnit_QuerySessionIndex_ReadsEveryWorkspace(t *testing.T) {
	ctx, db := setupInspectDB(t)
	seedInspectSessions(t, ctx, db)

	rows, err := querySessionIndex(ctx, db.WithoutTransaction())
	require.NoError(t, err)

	byID := map[string]sessionIndexRow{}
	for _, r := range rows {
		byID[r.id] = r
	}
	require.Len(t, byID, 4)
	require.Equal(t, sessionIndexRow{id: "idx-z1", identity: "acp-client", workspace: "ws-a", name: "zed-1111aaaa2222", msgs: 2}, byID["idx-z1"])
	require.Equal(t, sessionIndexRow{id: "idx-z2", identity: "acp-client", workspace: "ws-a", name: "zed-3333bbbb4444", msgs: 1}, byID["idx-z2"])
	require.Equal(t, sessionIndexRow{id: "idx-j1", identity: "acp-client", workspace: "ws-b", name: "jetbrainsgoland-5555cccc6666", msgs: 0}, byID["idx-j1"])
	require.Equal(t, sessionIndexRow{id: "idx-anon", identity: "cli", workspace: "ws-b", name: "", msgs: 0}, byID["idx-anon"],
		"an unnamed index still belongs in the inventory")

	empty, err := querySessionIndex(ctx, mustEmptyDB(t).WithoutTransaction())
	require.NoError(t, err)
	require.Empty(t, empty, "an empty database is no rows, not an error")
}

func mustEmptyDB(t *testing.T) libdb.DBManager {
	t.Helper()
	db, err := libdb.NewSQLiteDBManager(context.Background(), filepath.Join(t.TempDir(), "empty.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

// TestUnit_SessionListFiltered_PinsRenderedTable pins the exact table
// `contenox session list` prints, including the "(unnamed)" placeholder and
// the workspace/name sort, over the rerouted read.
func TestUnit_SessionListFiltered_PinsRenderedTable(t *testing.T) {
	ctx, db := setupInspectDB(t)
	seedInspectSessions(t, ctx, db)

	for _, tc := range []struct {
		name      string
		workspace string
		namespace string
		all       bool
		want      string
	}{
		{
			name: "all workspaces",
			all:  true,
			want: "" +
				"WORKSPACE  NAME                          IDENTITY    MESSAGES  ID\n" +
				"ws-a       zed-1111aaaa2222              acp-client  2         idx-z1\n" +
				"ws-a       zed-3333bbbb4444              acp-client  1         idx-z2\n" +
				"ws-b       (unnamed)                     cli         0         idx-anon\n" +
				"ws-b       jetbrainsgoland-5555cccc6666  acp-client  0         idx-j1\n",
		},
		{
			name:      "one workspace",
			workspace: "ws-a",
			want: "" +
				"WORKSPACE  NAME              IDENTITY    MESSAGES  ID\n" +
				"ws-a       zed-1111aaaa2222  acp-client  2         idx-z1\n" +
				"ws-a       zed-3333bbbb4444  acp-client  1         idx-z2\n",
		},
		{
			name:      "one namespace",
			workspace: "ws-b",
			namespace: "jetbrainsgoland",
			want: "" +
				"WORKSPACE  NAME                          IDENTITY    MESSAGES  ID\n" +
				"ws-b       jetbrainsgoland-5555cccc6666  acp-client  0         idx-j1\n",
		},
		{
			name:      "no match",
			workspace: "ws-nope",
			want:      "No matching sessions.\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			require.NoError(t, runSessionListFiltered(cmd, ctx, db, tc.workspace, tc.namespace, tc.all))
			require.Equal(t, tc.want, out.String())
		})
	}
}

// TestUnit_SessionWorkspaces_PinsRenderedTable pins the namespace rollup:
// counts aggregated per (workspace, namespace, identity), with the generated
// id suffix stripped off the name.
func TestUnit_SessionWorkspaces_PinsRenderedTable(t *testing.T) {
	ctx, db := setupInspectDB(t)
	seedInspectSessions(t, ctx, db)

	rows, err := querySessionIndex(ctx, db.WithoutTransaction())
	require.NoError(t, err)
	require.Len(t, rows, 4)

	// deriveNamespace is what turns the inventory into the rollup; pin it on
	// the names the read actually produced.
	byID := map[string]sessionIndexRow{}
	for _, r := range rows {
		byID[r.id] = r
	}
	require.Equal(t, "zed", deriveNamespace(byID["idx-z1"].name))
	require.Equal(t, "zed", deriveNamespace(byID["idx-z2"].name))
	require.Equal(t, "jetbrainsgoland", deriveNamespace(byID["idx-j1"].name))
	require.Equal(t, "(unnamed)", deriveNamespace(byID["idx-anon"].name))
}

// TestUnit_ResolveSessionByID_ThroughStore pins the id -> name resolve: a
// named index gives its name, an unnamed one is still found (empty name, found
// true), and an unknown id is not found. The lookup crosses workspaces on
// purpose — the operator pastes ids straight out of the inventory above.
func TestUnit_ResolveSessionByID_ThroughStore(t *testing.T) {
	ctx, db := setupInspectDB(t)
	seedInspectSessions(t, ctx, db)

	name, found := resolveSessionByID(ctx, db, "idx-z1")
	require.True(t, found)
	require.Equal(t, "zed-1111aaaa2222", name)

	name, found = resolveSessionByID(ctx, db, "idx-j1")
	require.True(t, found, "another workspace's session resolves too")
	require.Equal(t, "jetbrainsgoland-5555cccc6666", name)

	name, found = resolveSessionByID(ctx, db, "idx-anon")
	require.True(t, found, "an unnamed index exists; it is not a missing row")
	require.Equal(t, "", name)

	_, found = resolveSessionByID(ctx, db, "idx-does-not-exist")
	require.False(t, found)
}
