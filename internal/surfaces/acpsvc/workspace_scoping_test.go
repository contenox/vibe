package acpsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// tagSessionRoot writes the stored workspace root for a named session, the same
// per-session KV persistSessionCwd writes and the scoping predicates read.
func tagSessionRoot(t *testing.T, ctx context.Context, store runtimetypes.Store, name, root string) {
	t.Helper()
	raw, err := json.Marshal(sessionCwdRecord{Cwd: root})
	require.NoError(t, err)
	require.NoError(t, store.SetKV(ctx, acpSessionCwdKVPrefix+name, raw))
}

func listedSessionIDs(resp libacp.ListSessionsResponse) []string {
	out := make([]string, 0, len(resp.Sessions))
	for _, s := range resp.Sessions {
		out = append(out, string(s.SessionID))
	}
	return out
}

// TestUnit_ListSessions_ScopesByWorkspaceRoot pins the leak fix: two instances on
// one machine partition, each with its own root, list ONLY their own workspace's
// sessions. A subpath of a root counts as in view; an untagged (legacy) session
// falls into neither fixed-root instance's view.
func TestUnit_ListSessions_ScopesByWorkspaceRoot(t *testing.T) {
	ctx, db := setupResolverDB(t)
	exec := db.WithoutTransaction()
	msgStore := runtimetypes.NewMessageStore(exec, routingWorkspace)
	kv := runtimetypes.New(exec)

	rootA := t.TempDir()
	rootB := t.TempDir()
	nestedA := filepath.Join(rootA, "nested")
	require.NoError(t, os.MkdirAll(nestedA, 0o755))

	seedRoutingSession(t, ctx, msgStore, "sess-a1")
	seedRoutingSession(t, ctx, msgStore, "sess-a2")
	seedRoutingSession(t, ctx, msgStore, "sess-b1")
	seedRoutingSession(t, ctx, msgStore, "sess-legacy") // named but never tagged with a root

	tagSessionRoot(t, ctx, kv, "sess-a1", rootA)
	tagSessionRoot(t, ctx, kv, "sess-a2", nestedA) // a subpath of rootA is still A's
	tagSessionRoot(t, ctx, kv, "sess-b1", rootB)

	factoryA, err := vfs.NewFactory(rootA)
	require.NoError(t, err)
	factoryB, err := vfs.NewFactory(rootB)
	require.NoError(t, err)

	instA := &Transport{deps: Deps{DB: db, WorkspaceID: routingWorkspace, WorkspaceRoots: factoryA}}
	instB := &Transport{deps: Deps{DB: db, WorkspaceID: routingWorkspace, WorkspaceRoots: factoryB}}

	respA, err := instA.ListSessions(ctx, libacp.ListSessionsRequest{})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"sess-a1", "sess-a2"}, listedSessionIDs(respA),
		"instance A lists its root and a subpath of it, never rootB's session nor the untagged one")

	respB, err := instB.ListSessions(ctx, libacp.ListSessionsRequest{})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"sess-b1"}, listedSessionIDs(respB),
		"instance B lists only its own workspace's session")
}

// TestUnit_ListSessions_ExplicitCwdNarrowsWithinTheView pins that req.Cwd is an
// in-view narrowing, never an escape hatch: it filters down to an exact-match cwd
// among the instance's own sessions, but cannot reach a foreign workspace's.
func TestUnit_ListSessions_ExplicitCwdNarrowsWithinTheView(t *testing.T) {
	ctx, db := setupResolverDB(t)
	exec := db.WithoutTransaction()
	msgStore := runtimetypes.NewMessageStore(exec, routingWorkspace)
	kv := runtimetypes.New(exec)

	rootA := t.TempDir()
	rootB := t.TempDir()
	nestedA := filepath.Join(rootA, "nested")
	require.NoError(t, os.MkdirAll(nestedA, 0o755))

	seedRoutingSession(t, ctx, msgStore, "sess-a1")
	seedRoutingSession(t, ctx, msgStore, "sess-a2")
	seedRoutingSession(t, ctx, msgStore, "sess-b1")
	tagSessionRoot(t, ctx, kv, "sess-a1", rootA)
	tagSessionRoot(t, ctx, kv, "sess-a2", nestedA)
	tagSessionRoot(t, ctx, kv, "sess-b1", rootB)

	factoryA, err := vfs.NewFactory(rootA)
	require.NoError(t, err)
	instA := &Transport{deps: Deps{DB: db, WorkspaceID: routingWorkspace, WorkspaceRoots: factoryA}}

	narrowed, err := instA.ListSessions(ctx, libacp.ListSessionsRequest{Cwd: rootA})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"sess-a1"}, listedSessionIDs(narrowed),
		"an explicit cwd narrows to the exact-match session, dropping the subpath one")

	escape, err := instA.ListSessions(ctx, libacp.ListSessionsRequest{Cwd: rootB})
	require.NoError(t, err)
	require.Empty(t, listedSessionIDs(escape),
		"req.Cwd naming a foreign root cannot pull a foreign workspace's session into the view")
}

// TestUnit_ListSessions_EditorShapeStaysUnscoped pins that with no host factory
// (the acp editor shape) listing is client-driven as before: every named session
// in the partition is returned, whatever its stored root.
func TestUnit_ListSessions_EditorShapeStaysUnscoped(t *testing.T) {
	ctx, db := setupResolverDB(t)
	exec := db.WithoutTransaction()
	msgStore := runtimetypes.NewMessageStore(exec, routingWorkspace)
	kv := runtimetypes.New(exec)

	rootA := t.TempDir()
	rootB := t.TempDir()
	seedRoutingSession(t, ctx, msgStore, "sess-a1")
	seedRoutingSession(t, ctx, msgStore, "sess-b1")
	seedRoutingSession(t, ctx, msgStore, "sess-legacy")
	tagSessionRoot(t, ctx, kv, "sess-a1", rootA)
	tagSessionRoot(t, ctx, kv, "sess-b1", rootB)

	editor := &Transport{deps: Deps{DB: db, WorkspaceID: routingWorkspace}} // nil WorkspaceRoots

	resp, err := editor.ListSessions(ctx, libacp.ListSessionsRequest{})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"sess-a1", "sess-b1", "sess-legacy"}, listedSessionIDs(resp),
		"the editor shape has no server root, so no session is scoped out")
}

// TestUnit_ListSessions_ScopedPagingIsFullAndOrdered pins that scoping BEFORE
// pagination keeps pages full and the cursor correct: an instance walks exactly
// its own sessions, in full pages, never seeing a foreign one on any page.
func TestUnit_ListSessions_ScopedPagingIsFullAndOrdered(t *testing.T) {
	ctx, db := setupResolverDB(t)
	exec := db.WithoutTransaction()
	msgStore := runtimetypes.NewMessageStore(exec, routingWorkspace)
	kv := runtimetypes.New(exec)

	rootA := t.TempDir()
	rootB := t.TempDir()

	prev := listSessionsPageSize
	listSessionsPageSize = 2
	t.Cleanup(func() { listSessionsPageSize = prev })

	wantA := map[string]bool{}
	for i := range 5 {
		name := fmt.Sprintf("sess-a%02d", i)
		seedRoutingSession(t, ctx, msgStore, name)
		tagSessionRoot(t, ctx, kv, name, rootA)
		wantA[name] = true
	}
	// Foreign chaff interleaved into the same partition; none may surface.
	for i := range 3 {
		name := fmt.Sprintf("sess-b%02d", i)
		seedRoutingSession(t, ctx, msgStore, name)
		tagSessionRoot(t, ctx, kv, name, rootB)
	}

	factoryA, err := vfs.NewFactory(rootA)
	require.NoError(t, err)
	instA := &Transport{deps: Deps{DB: db, WorkspaceID: routingWorkspace, WorkspaceRoots: factoryA}}

	seen := map[string]int{}
	var pageLens []int
	cursor := ""
	for pages := 0; ; pages++ {
		require.Less(t, pages, 20, "paging did not terminate")
		resp, err := instA.ListSessions(ctx, libacp.ListSessionsRequest{Cursor: cursor})
		require.NoError(t, err)
		pageLens = append(pageLens, len(resp.Sessions))
		for _, s := range resp.Sessions {
			require.NotContains(t, string(s.SessionID), "sess-b", "a foreign session must never appear on any page")
			seen[string(s.SessionID)]++
		}
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}

	require.Equal(t, []int{2, 2, 1}, pageLens, "the filtered view pages in full pages of 2, then the remainder")
	require.Len(t, seen, len(wantA))
	for name := range wantA {
		require.Equal(t, 1, seen[name], "%s must appear exactly once across pages", name)
	}
}

// TestUnit_LoadResume_RefuseForeignWorkspaceSession pins the load/resume scope: a
// session id that exists in the machine partition but is rooted in another
// workspace is refused with the same "not found" shape as an unknown id, so it is
// never replayed or re-tagged to this instance's root.
func TestUnit_LoadResume_RefuseForeignWorkspaceSession(t *testing.T) {
	ctx, db := setupResolverDB(t)
	exec := db.WithoutTransaction()
	msgStore := runtimetypes.NewMessageStore(exec, routingWorkspace)
	kv := runtimetypes.New(exec)

	rootA := t.TempDir()
	rootB := t.TempDir()
	seedRoutingSession(t, ctx, msgStore, "sess-b1")
	tagSessionRoot(t, ctx, kv, "sess-b1", rootB)

	factoryA, err := vfs.NewFactory(rootA)
	require.NoError(t, err)
	instA := &Transport{deps: Deps{
		Engine:         &enginesvc.Engine{},
		DB:             db,
		WorkspaceID:    routingWorkspace,
		WorkspaceRoots: factoryA,
	}}

	_, loadErr := instA.LoadSession(ctx, libacp.LoadSessionRequest{SessionID: "sess-b1", Cwd: "/"})
	require.Error(t, loadErr, "loading a foreign workspace's session by id must be refused before any replay")
	require.ErrorContains(t, loadErr, "not found",
		"the refusal is indistinguishable from an unknown id")
	var loadCoded *libacp.Error
	require.ErrorAs(t, loadErr, &loadCoded)
	require.Equal(t, libacp.ErrInvalidParams, loadCoded.Code)

	_, resumeErr := instA.ResumeSession(ctx, libacp.ResumeSessionRequest{SessionID: "sess-b1", Cwd: "/"})
	require.Error(t, resumeErr, "resuming a foreign workspace's session by id must be refused too")
	require.ErrorContains(t, resumeErr, "not found")

	// The foreign session's stored root is untouched: the refusal never re-tagged it.
	require.Equal(t, rootB, instA.sessionCwd(ctx, kv, "sess-b1"),
		"a refused open must not silently corrupt the session's stored root")
}

// TestUnit_WorkspaceRootView_Predicate proves the scoping predicate takes a root
// SET, so multi-root joins later with no rewrite: a size-2 set matches either
// root and their subpaths. It also pins the "" divergence InView adds over Allows.
func TestUnit_WorkspaceRootView_Predicate(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	outside := t.TempDir()

	single, err := vfs.NewFactory(rootA)
	require.NoError(t, err)
	require.True(t, single.InView(rootA))
	require.False(t, single.InView(rootB))
	require.False(t, single.InView(""), "an untagged session belongs to no fixed-root instance's view")
	// Allows would admit the empty string as the default root; InView must not.
	_, allowsEmpty := single.Allows("")
	require.True(t, allowsEmpty, "Allows resolves \"\" to the default root")

	multi, err := vfs.NewFactory(rootA, rootB)
	require.NoError(t, err)
	require.True(t, multi.InView(rootA), "a size-2 set matches its first root")
	require.True(t, multi.InView(rootB), "and its second root")
	require.True(t, multi.InView(filepath.Join(rootB, "sub")), "and a subpath of the second root")
	require.False(t, multi.InView(outside), "but nothing outside every root")
}

// TestUnit_WorkspaceViewWrappers_ListVsLoadAsymmetry pins the one deliberate
// asymmetry: the editor shape admits everything; a fixed-root instance excludes
// the untagged session from listing yet admits it for load/resume (to claim and
// re-tag it), and refuses a foreign root for both.
func TestUnit_WorkspaceViewWrappers_ListVsLoadAsymmetry(t *testing.T) {
	editor := &Transport{deps: Deps{}}
	require.True(t, editor.sessionInWorkspaceView(""))
	require.True(t, editor.sessionInWorkspaceView("/any/where"))
	require.True(t, editor.sessionLoadableInView(""))
	require.True(t, editor.sessionLoadableInView("/any/where"))

	rootA := t.TempDir()
	foreign := t.TempDir()
	factoryA, err := vfs.NewFactory(rootA)
	require.NoError(t, err)
	hosted := &Transport{deps: Deps{WorkspaceRoots: factoryA}}

	require.True(t, hosted.sessionInWorkspaceView(rootA))
	require.False(t, hosted.sessionInWorkspaceView(""), "listing excludes the untagged session")
	require.True(t, hosted.sessionLoadableInView(""), "load/resume admit and re-tag the untagged session")
	require.False(t, hosted.sessionLoadableInView(foreign), "a foreign root is loadable by neither")
	require.False(t, hosted.sessionInWorkspaceView(foreign))
}
