package missionchanges

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/libacp"
)

// --- notification builders: they construct exactly the shapes acpsvc/events.go
// emits, so the fold is tested against realistic journal contents. Each builder
// mints a UNIQUE tool-call id per call, exactly as acpsvc's toolCallWireID does
// for distinct invocations — two writes of one file are two invocations, so they
// score twice. The *WithID variants let a test model the create+update pair of a
// single invocation, which SHARE an id. ---

var toolCallSeq int

func nextCallID(kind string) string {
	toolCallSeq++
	return fmt.Sprintf("%s#%d", kind, toolCallSeq)
}

// edit builds the tool-call notification a file write produces: a diff content
// carrying old/new text AND a location for the same path (acpsvc sets both). Note
// acpsvc promotes a first-seen update to SessionUpdateToolCall — either kind must
// score identically, which these tests exercise by using SessionUpdateToolCall
// here (the deterministic-write shape) and SessionUpdateToolCallUpdate in touch.
func edit(path, oldText, newText string) libacp.SessionNotification {
	return libacp.SessionNotification{
		Update: libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateToolCall,
			ToolCallID:    nextCallID("edit"),
			Kind:          libacp.ToolKindEdit,
			Status:        libacp.ToolCallStatusCompleted,
			ToolContent:   []libacp.ToolCallContent{{Type: libacp.ToolCallContentDiff, Path: path, OldText: oldText, NewText: newText}},
			Locations:     []libacp.ToolCallLocation{{Path: path}},
		},
	}
}

// touch builds a non-edit tool-call update (read/stat/list/exec) carrying only a
// location for the path — the shape a read_file event has.
func touch(path string, kind libacp.ToolKind) libacp.SessionNotification {
	return touchWithID(nextCallID(string(kind)), path, kind)
}

func touchWithID(id, path string, kind libacp.ToolKind) libacp.SessionNotification {
	return libacp.SessionNotification{
		Update: libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateToolCallUpdate,
			ToolCallID:    id,
			Kind:          kind,
			Status:        libacp.ToolCallStatusCompleted,
			Locations:     []libacp.ToolCallLocation{{Path: path}},
		},
	}
}

// pendingWithID builds the create/pending notification (SessionUpdateToolCall)
// that precedes an interactive tool call — used with a shared id to prove the
// create+update pair of ONE invocation is not double-counted.
func pendingWithID(id, path string, kind libacp.ToolKind) libacp.SessionNotification {
	return libacp.SessionNotification{
		Update: libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateToolCall,
			ToolCallID:    id,
			Kind:          kind,
			Status:        libacp.ToolCallStatusPending,
			Locations:     []libacp.ToolCallLocation{{Path: path}},
		},
	}
}

func TestUnit_Fold_StatusDerivation(t *testing.T) {
	root := "/ws"
	cases := []struct {
		name    string
		updates []libacp.SessionNotification
		path    string
		want    string
	}{
		{
			name:    "added when first old is empty",
			updates: []libacp.SessionNotification{edit("/ws/a.txt", "", "hello")},
			path:    "/ws/a.txt",
			want:    StatusAdded,
		},
		{
			name:    "modified when both non-empty and changed",
			updates: []libacp.SessionNotification{edit("/ws/b.txt", "old", "new")},
			path:    "/ws/b.txt",
			want:    StatusModified,
		},
		{
			name:    "deleted when last new is empty",
			updates: []libacp.SessionNotification{edit("/ws/c.txt", "gone", "")},
			path:    "/ws/c.txt",
			want:    StatusDeleted,
		},
		{
			// added precedence: created then emptied is still "added" (new to the
			// mission), because status checks first-old-empty before last-new-empty.
			name:    "added precedence over deleted",
			updates: []libacp.SessionNotification{edit("/ws/d.txt", "", "v1"), edit("/ws/d.txt", "v1", "")},
			path:    "/ws/d.txt",
			want:    StatusAdded,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fold(tc.updates).changes(root)
			file := findFile(t, got.Files, tc.path)
			if file.Status != tc.want {
				t.Fatalf("status = %q, want %q", file.Status, tc.want)
			}
		})
	}
}

func TestUnit_Fold_FirstOldLastNew(t *testing.T) {
	updates := []libacp.SessionNotification{
		edit("/ws/f.go", "v0", "v1"),
		edit("/ws/f.go", "v1", "v2"),
		edit("/ws/f.go", "v2", "v3"),
	}
	f := fold(updates)
	ff := f.files["/ws/f.go"]
	if ff.firstOld != "v0" {
		t.Fatalf("firstOld = %q, want v0 (the content as the mission first found it)", ff.firstOld)
	}
	if ff.lastNew != "v3" {
		t.Fatalf("lastNew = %q, want v3 (the content as the mission left it)", ff.lastNew)
	}
}

func TestUnit_Fold_OrderingByEditWeightedScore(t *testing.T) {
	// a.txt written twice (2 edits), b.txt written once (1 edit): a must rank
	// first — the changed-files list is ordered by where attention concentrated.
	updates := []libacp.SessionNotification{
		edit("/ws/b.txt", "", "b"),
		edit("/ws/a.txt", "", "a1"),
		edit("/ws/a.txt", "a1", "a2"),
	}
	got := fold(updates).changes("/ws")
	if len(got.Files) != 2 {
		t.Fatalf("want 2 changed files, got %d", len(got.Files))
	}
	if got.Files[0].Path != "/ws/a.txt" {
		t.Fatalf("order[0] = %q, want /ws/a.txt (higher DOI: two edits)", got.Files[0].Path)
	}
	if got.Files[0].Score <= got.Files[1].Score {
		t.Fatalf("a.txt score %d must exceed b.txt score %d", got.Files[0].Score, got.Files[1].Score)
	}
	if got.Files[0].Score != 2*weightEdit {
		t.Fatalf("a.txt score = %d, want %d (two edits)", got.Files[0].Score, 2*weightEdit)
	}
}

func TestUnit_Fold_ReadRaisesScoreOfEditedFile(t *testing.T) {
	// Two files each edited once; one is ALSO read. The read adds weight, so the
	// read-and-edited file ranks above the edited-only one — edit > read, additive.
	updates := []libacp.SessionNotification{
		edit("/ws/hot.go", "", "x"),
		touch("/ws/hot.go", libacp.ToolKindRead),
		edit("/ws/cold.go", "", "y"),
	}
	got := fold(updates).changes("/ws")
	if got.Files[0].Path != "/ws/hot.go" {
		t.Fatalf("order[0] = %q, want /ws/hot.go (edit+read outranks edit)", got.Files[0].Path)
	}
	if got.Files[0].Score != weightEdit+weightRead {
		t.Fatalf("hot.go score = %d, want %d (edit+read)", got.Files[0].Score, weightEdit+weightRead)
	}
	if got.Files[1].Score != weightEdit {
		t.Fatalf("cold.go score = %d, want %d (edit only)", got.Files[1].Score, weightEdit)
	}
}

func TestUnit_Fold_PendingNotDoubleCounted(t *testing.T) {
	// An interactive call journals a pending create THEN a terminal update, sharing
	// a ToolCallID. Deduping by that id counts the invocation once.
	updates := []libacp.SessionNotification{
		pendingWithID("call-1", "/ws/a.go", libacp.ToolKindRead),
		touchWithID("call-1", "/ws/a.go", libacp.ToolKindRead),
	}
	f := fold(updates)
	if f.scores["/ws/a.go"] != weightRead {
		t.Fatalf("score = %d, want %d (pending+update counts once)", f.scores["/ws/a.go"], weightRead)
	}
	// The pending create still contributes the path to the touched set for scope.
	if _, ok := f.touched["/ws/a.go"]; !ok {
		t.Fatal("pending create should still register the path as touched for scope")
	}
}

func TestUnit_Fold_WithinUpdateNoDoubleCount(t *testing.T) {
	// A write carries both a diff and a location for the same path; it must weigh
	// as one edit, not edit + read.
	f := fold([]libacp.SessionNotification{edit("/ws/a.go", "", "x")})
	if f.scores["/ws/a.go"] != weightEdit {
		t.Fatalf("score = %d, want %d (diff+location for one path is one edit)", f.scores["/ws/a.go"], weightEdit)
	}
}

func TestUnit_Fold_ChangedListCap(t *testing.T) {
	// More than maxChangedFiles edited: the list caps and flags incomplete, keeping
	// the HIGHEST-attention files (the cap never hides the hot spot).
	var updates []libacp.SessionNotification
	for i := 0; i < maxChangedFiles+50; i++ {
		updates = append(updates, edit(fmt.Sprintf("/ws/f%03d.txt", i), "", "x"))
	}
	// Give one late file extra attention so it must survive the cap despite a high
	// index (it would be dropped by any insertion-order cap).
	hot := fmt.Sprintf("/ws/f%03d.txt", maxChangedFiles+40)
	for i := 0; i < 5; i++ {
		updates = append(updates, edit(hot, "x", fmt.Sprintf("x%d", i)))
	}
	got := fold(updates).changes("/ws")
	if len(got.Files) != maxChangedFiles {
		t.Fatalf("len = %d, want cap %d", len(got.Files), maxChangedFiles)
	}
	if !got.Incomplete {
		t.Fatal("Incomplete must be set when the list is capped")
	}
	if got.Files[0].Path != hot {
		t.Fatalf("highest-attention file %q must survive the cap at position 0, got %q", hot, got.Files[0].Path)
	}
}

func TestUnit_Scope_DistinctFilesAndDirs(t *testing.T) {
	updates := []libacp.SessionNotification{
		edit("/ws/a.txt", "", "1"),                 // top-level "."
		edit("/ws/sub/b.txt", "", "2"),             // top-level "sub"
		edit("/ws/sub/c.txt", "", "3"),             // top-level "sub" (same bucket)
		touch("/ws/pkg/d.go", libacp.ToolKindRead), // top-level "pkg"
	}
	scope := fold(updates).changes("/ws").Scope
	if scope.Files != 4 {
		t.Fatalf("files = %d, want 4 distinct", scope.Files)
	}
	// top-level dirs: ".", "sub", "pkg" = 3
	if scope.Dirs != 3 {
		t.Fatalf("dirs = %d, want 3 distinct top-level dirs", scope.Dirs)
	}
	if scope.Anomaly {
		t.Fatal("no anomaly: every path is under the workspace root")
	}
}

func TestUnit_Scope_AnomalyOnOutsidePath(t *testing.T) {
	// The $HOME-wanderer: edits in the repo, but reads an absolute path in another
	// tree. Scope arithmetic trips the anomaly and samples the offending path.
	updates := []libacp.SessionNotification{
		edit("/ws/repo/a.go", "", "x"),
		touch("/etc/passwd", libacp.ToolKindRead),
	}
	scope := fold(updates).changes("/ws/repo").Scope
	if !scope.Anomaly {
		t.Fatal("anomaly must trip when a touched path falls outside cwd")
	}
	if len(scope.OutsidePaths) != 1 || scope.OutsidePaths[0] != "/etc/passwd" {
		t.Fatalf("outsidePaths = %v, want [/etc/passwd]", scope.OutsidePaths)
	}
}

func TestUnit_Scope_RelativePathsAreInside(t *testing.T) {
	// A relative path is named relative to the tool's cwd and cannot escape it, so
	// it never trips the anomaly.
	updates := []libacp.SessionNotification{touch("a/b.txt", libacp.ToolKindRead)}
	scope := fold(updates).changes("/ws").Scope
	if scope.Anomaly {
		t.Fatal("a relative path is inside by construction; no anomaly")
	}
}

func TestUnit_Scope_EmptyRootDisablesAnomaly(t *testing.T) {
	// With no recorded cwd there is no lane to have left: the anomaly check is
	// disabled rather than guessing a root (a false alarm is worse than none).
	updates := []libacp.SessionNotification{touch("/anywhere/x", libacp.ToolKindRead)}
	scope := fold(updates).changes("").Scope
	if scope.Anomaly {
		t.Fatal("empty root must disable anomaly detection")
	}
}

func TestUnit_TopLevelDir(t *testing.T) {
	cases := []struct{ root, path, want string }{
		{"/ws", "/ws/a.txt", "."},
		{"/ws", "/ws/src/a.txt", "src"},
		{"/ws", "/ws/src/deep/a.txt", "src"},
		{"/ws", "/etc/hostname", "/etc"},
		{"/ws", "/home/u/x", "/home"},
		{"", "rel/x.txt", "rel"},
		{"", "bare.txt", "."},
	}
	for _, tc := range cases {
		if got := topLevelDir(tc.root, tc.path); got != tc.want {
			t.Errorf("topLevelDir(%q, %q) = %q, want %q", tc.root, tc.path, got, tc.want)
		}
	}
}

func TestUnit_CapDiff(t *testing.T) {
	small := "abc"
	if o, m, tr := capDiff(small, small); tr || o != small || m != small {
		t.Fatalf("small diff must pass through untouched, got trunc=%v", tr)
	}
	big := strings.Repeat("z", diffDisplayCap+10)
	o, m, tr := capDiff(big, "ok")
	if !tr {
		t.Fatal("oversize original must set truncated")
	}
	if len(o) != diffDisplayCap {
		t.Fatalf("original len = %d, want clipped to %d", len(o), diffDisplayCap)
	}
	if m != "ok" {
		t.Fatalf("modified must be untouched, got %q", m)
	}
}

// --- Service-level tests over stubs ---

type stubMissions struct {
	m   *missionservice.Mission
	err error
}

func (s stubMissions) Get(_ context.Context, id string) (*missionservice.Mission, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.m, nil
}

type stubJournal struct {
	updates []libacp.SessionNotification
	cwd     string
	ok      bool
}

func (s stubJournal) SessionJournal(_ string, _ libacp.SessionID) ([]libacp.SessionNotification, string, bool) {
	return s.updates, s.cwd, s.ok
}

func boundMission() *missionservice.Mission {
	return &missionservice.Mission{ID: "m1", SessionID: "s1", InstanceID: "i1"}
}

func TestUnit_Service_Changes_EndToEnd(t *testing.T) {
	svc := New(
		stubMissions{m: boundMission()},
		stubJournal{ok: true, cwd: "/ws", updates: []libacp.SessionNotification{
			edit("/ws/a.txt", "", "hello"),
			edit("/ws/a.txt", "hello", "hello world"),
			edit("/ws/b.txt", "old", "new"),
			touch("/tmp/wander", libacp.ToolKindRead),
		}},
	)
	got, err := svc.Changes(context.Background(), "m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 2 {
		t.Fatalf("want 2 changed files, got %d", len(got.Files))
	}
	if got.Files[0].Path != "/ws/a.txt" || got.Files[0].Status != StatusAdded {
		t.Fatalf("files[0] = %+v, want a.txt/added first", got.Files[0])
	}
	if got.Files[1].Status != StatusModified {
		t.Fatalf("files[1] status = %q, want modified", got.Files[1].Status)
	}
	if !got.Scope.Anomaly {
		t.Fatal("the /tmp/wander read is outside /ws → anomaly")
	}
	if got.Scope.Files != 3 {
		t.Fatalf("scope files = %d, want 3 (a.txt, b.txt, /tmp/wander)", got.Scope.Files)
	}
}

func TestUnit_Service_Diff_FirstOldLastNew(t *testing.T) {
	svc := New(
		stubMissions{m: boundMission()},
		stubJournal{ok: true, cwd: "/ws", updates: []libacp.SessionNotification{
			edit("/ws/a.txt", "one", "two"),
			edit("/ws/a.txt", "two", "three"),
		}},
	)
	d, err := svc.Diff(context.Background(), "m1", "/ws/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if d.Original != "one" || d.Modified != "three" {
		t.Fatalf("diff = {%q,%q}, want {one,three}", d.Original, d.Modified)
	}
}

func TestUnit_Service_Diff_UnknownPathNotFound(t *testing.T) {
	svc := New(stubMissions{m: boundMission()}, stubJournal{ok: true, cwd: "/ws"})
	_, err := svc.Diff(context.Background(), "m1", "/ws/nope.txt")
	if err == nil {
		t.Fatal("a path the mission never wrote must be a not-found error")
	}
}

func TestUnit_Service_MissionNotFoundPropagates(t *testing.T) {
	svc := New(stubMissions{err: libdb.ErrNotFound}, stubJournal{ok: true})
	_, err := svc.Changes(context.Background(), "missing")
	if !errors.Is(err, libdb.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound propagated", err)
	}
}

func TestUnit_Service_UnboundMissionYieldsEmpty(t *testing.T) {
	// A mission with no session/instance bound (never dispatched) reads as empty
	// changes, not an error.
	svc := New(stubMissions{m: &missionservice.Mission{ID: "m1"}}, stubJournal{ok: true})
	got, err := svc.Changes(context.Background(), "m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 0 || got.Scope.Files != 0 {
		t.Fatalf("unbound mission must yield empty changes, got %+v", got)
	}
}

func TestUnit_Service_GoneInstanceYieldsEmpty(t *testing.T) {
	// Bound mission, but the kernel no longer owns the session (ok=false): empty,
	// non-error — "nothing recorded" is honest for a stopped unit.
	svc := New(stubMissions{m: boundMission()}, stubJournal{ok: false})
	got, err := svc.Changes(context.Background(), "m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 0 {
		t.Fatalf("gone instance must yield empty changes, got %+v", got)
	}
}

func findFile(t *testing.T, files []ChangedFile, path string) ChangedFile {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("path %q not in changed files %+v", path, files)
	return ChangedFile{}
}
