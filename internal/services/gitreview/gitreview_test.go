package gitreview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func newRepo(t *testing.T) (string, *git.Repository) {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	return dir, repo
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func commit(t *testing.T, repo *git.Repository, paths []string, message string) plumbing.Hash {
	t.Helper()
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	for _, p := range paths {
		if err := wt.AddWithOptions(&git.AddOptions{Path: p}); err != nil {
			t.Fatalf("add %s: %v", p, err)
		}
	}
	hash, err := wt.Commit(message, &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return hash
}

func stageAll(t *testing.T, repo *git.Repository, paths ...string) {
	t.Helper()
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	for _, p := range paths {
		if err := wt.AddWithOptions(&git.AddOptions{Path: p}); err != nil {
			t.Fatalf("add %s: %v", p, err)
		}
	}
}

func fileByPath(t *testing.T, res *DiffResult, path string) FileDiff {
	t.Helper()
	for _, f := range res.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no file %q in diff; got %v", path, pathsOf(res))
	return FileDiff{}
}

func pathsOf(res *DiffResult) []string {
	out := make([]string, 0, len(res.Files))
	for _, f := range res.Files {
		out = append(out, f.Path)
	}
	return out
}

func numbered(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

func TestDiffUnstagedHunkCoordinates(t *testing.T) {
	dir, repo := newRepo(t)
	base := numbered("a", "b", "c", "d", "e", "f", "g", "h", "i", "j")
	write(t, dir, "f.txt", base)
	commit(t, repo, []string{"f.txt"}, "base")

	changed := numbered("a", "b", "c", "d", "E", "f", "g", "h", "i", "j")
	write(t, dir, "f.txt", changed)

	res, err := Diff(context.Background(), dir, RefIndex, RefWorktree)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	fd := fileByPath(t, res, "f.txt")
	if fd.Status != StatusModified {
		t.Fatalf("status = %q, want modified", fd.Status)
	}
	if len(fd.Hunks) != 1 {
		t.Fatalf("hunks = %d, want 1: %+v", len(fd.Hunks), fd.Hunks)
	}
	h := fd.Hunks[0]
	if h.OldStart != 2 || h.OldLines != 7 || h.NewStart != 2 || h.NewLines != 7 {
		t.Fatalf("hunk = -%d,%d +%d,%d, want -2,7 +2,7", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
	}
	if fd.Additions != 1 || fd.Deletions != 1 {
		t.Fatalf("additions/deletions = %d/%d, want 1/1", fd.Additions, fd.Deletions)
	}
	var del, add string
	for _, l := range h.Lines {
		switch l.Kind {
		case LineDel:
			del = l.Text
		case LineAdd:
			add = l.Text
		}
	}
	if del != "e" || add != "E" {
		t.Fatalf("changed lines = %q -> %q, want e -> E", del, add)
	}
}

func TestDiffStagedAndUnstagedAreDifferentProjections(t *testing.T) {
	dir, repo := newRepo(t)
	write(t, dir, "f.txt", numbered("a", "b", "c"))
	commit(t, repo, []string{"f.txt"}, "base")

	write(t, dir, "f.txt", numbered("a", "B", "c"))
	stageAll(t, repo, "f.txt")
	write(t, dir, "f.txt", numbered("a", "B", "C"))

	staged, err := Diff(context.Background(), dir, RefHead, RefIndex)
	if err != nil {
		t.Fatalf("staged diff: %v", err)
	}
	sf := fileByPath(t, staged, "f.txt")
	if sf.Additions != 1 || sf.Deletions != 1 {
		t.Fatalf("staged +%d-%d, want +1-1", sf.Additions, sf.Deletions)
	}

	unstaged, err := Diff(context.Background(), dir, RefIndex, RefWorktree)
	if err != nil {
		t.Fatalf("unstaged diff: %v", err)
	}
	uf := fileByPath(t, unstaged, "f.txt")
	if uf.Additions != 1 || uf.Deletions != 1 {
		t.Fatalf("unstaged +%d-%d, want +1-1", uf.Additions, uf.Deletions)
	}
	if sf.ToHash == uf.ToHash {
		t.Fatal("staged and unstaged sides hash the same; the two projections collapsed")
	}
}

func TestDiffCommitToCommitAndBranchToBranch(t *testing.T) {
	dir, repo := newRepo(t)
	write(t, dir, "f.txt", numbered("one"))
	first := commit(t, repo, []string{"f.txt"}, "first")
	write(t, dir, "f.txt", numbered("two"))
	write(t, dir, "new.txt", numbered("brand new"))
	second := commit(t, repo, []string{"f.txt", "new.txt"}, "second")

	res, err := Diff(context.Background(), dir, first.String(), second.String())
	if err != nil {
		t.Fatalf("commit-to-commit: %v", err)
	}
	if got := pathsOf(res); len(got) != 2 {
		t.Fatalf("files = %v, want f.txt and new.txt", got)
	}
	added := fileByPath(t, res, "new.txt")
	if added.Status != StatusAdded {
		t.Fatalf("new.txt status = %q, want added", added.Status)
	}
	if added.FromHash != "" {
		t.Fatalf("an absent side must hash to \"\", got %q", added.FromHash)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("side"), Hash: first, Create: true}); err != nil {
		t.Fatalf("branch: %v", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Hash: second}); err != nil {
		t.Fatalf("back to second: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	_ = head

	byBranch, err := Diff(context.Background(), dir, "side", second.String())
	if err != nil {
		t.Fatalf("branch-to-branch: %v", err)
	}
	if len(byBranch.Files) != len(res.Files) {
		t.Fatalf("branch diff = %v, commit diff = %v", pathsOf(byBranch), pathsOf(res))
	}
}

func TestDiffRefusesPathOutsideRoot(t *testing.T) {
	dir, repo := newRepo(t)
	write(t, dir, "f.txt", numbered("a"))
	commit(t, repo, []string{"f.txt"}, "base")

	for _, p := range []string{"../escape.txt", "/etc/passwd"} {
		if _, err := Diff(context.Background(), dir, RefHead, RefWorktree, p); err == nil {
			t.Fatalf("path %q was accepted; it must be refused", p)
		} else if !IsRefusal(err) {
			t.Fatalf("path %q: got %v, want a refusal", p, err)
		}
	}
}

func TestDiffScopedToWorkspaceSubtree(t *testing.T) {
	dir, repo := newRepo(t)
	write(t, dir, "outside.txt", numbered("secret"))
	write(t, dir, "sub/inside.txt", numbered("visible"))
	commit(t, repo, []string{"outside.txt", "sub/inside.txt"}, "base")
	write(t, dir, "outside.txt", numbered("secret changed"))
	write(t, dir, "sub/inside.txt", numbered("visible changed"))

	res, err := Diff(context.Background(), filepath.Join(dir, "sub"), RefHead, RefWorktree)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	got := pathsOf(res)
	if len(got) != 1 || got[0] != "inside.txt" {
		t.Fatalf("files = %v, want only inside.txt — the repository is above the workspace root", got)
	}
}

func TestStageHunkMovesOneHunkOnly(t *testing.T) {
	dir, repo := newRepo(t)
	base := numbered("l1", "l2", "l3", "l4", "l5", "l6", "l7", "l8", "l9",
		"l10", "l11", "l12", "l13", "l14", "l15", "l16", "l17", "l18", "l19", "l20")
	write(t, dir, "f.txt", base)
	commit(t, repo, []string{"f.txt"}, "base")

	edited := strings.Replace(base, "l3\n", "L3\n", 1)
	edited = strings.Replace(edited, "l18\n", "L18\n", 1)
	write(t, dir, "f.txt", edited)

	res, err := Diff(context.Background(), dir, RefIndex, RefWorktree)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	fd := fileByPath(t, res, "f.txt")
	if len(fd.Hunks) != 2 {
		t.Fatalf("hunks = %d, want 2", len(fd.Hunks))
	}
	first := fd.Hunks[0]

	applied, err := StageHunk(context.Background(), dir, HunkRef{
		Path: "f.txt", OldStart: first.OldStart, OldLines: first.OldLines,
		NewStart: first.NewStart, NewLines: first.NewLines,
		FromHash: fd.FromHash, ToHash: fd.ToHash,
	})
	if err != nil {
		t.Fatalf("stage hunk: %v", err)
	}
	if applied.Status == nil || applied.Status.Clean {
		t.Fatal("status after staging must still show the remaining unstaged hunk")
	}

	onDisk, err := os.ReadFile(filepath.Join(dir, "f.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(onDisk) != edited {
		t.Fatal("StageHunk modified the worktree file; it must only move the index")
	}

	staged, err := Diff(context.Background(), dir, RefHead, RefIndex)
	if err != nil {
		t.Fatalf("staged diff: %v", err)
	}
	sf := fileByPath(t, staged, "f.txt")
	if len(sf.Hunks) != 1 || sf.Additions != 1 {
		t.Fatalf("staged = %d hunks +%d, want exactly the first hunk", len(sf.Hunks), sf.Additions)
	}
	if sf.Hunks[0].NewStart != first.NewStart {
		t.Fatalf("staged hunk moved to %d, want %d", sf.Hunks[0].NewStart, first.NewStart)
	}

	rest, err := Diff(context.Background(), dir, RefIndex, RefWorktree)
	if err != nil {
		t.Fatalf("rest: %v", err)
	}
	rf := fileByPath(t, rest, "f.txt")
	if len(rf.Hunks) != 1 {
		t.Fatalf("remaining hunks = %d, want 1", len(rf.Hunks))
	}
	for _, l := range rf.Hunks[0].Lines {
		if l.Kind == LineAdd && l.Text != "L18" {
			t.Fatalf("remaining hunk adds %q, want L18", l.Text)
		}
	}
}

func TestStageHunkRefusesStaleHash(t *testing.T) {
	dir, repo := newRepo(t)
	write(t, dir, "f.txt", numbered("a", "b", "c", "d", "e"))
	commit(t, repo, []string{"f.txt"}, "base")
	write(t, dir, "f.txt", numbered("a", "B", "c", "d", "e"))

	res, err := Diff(context.Background(), dir, RefIndex, RefWorktree)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	fd := fileByPath(t, res, "f.txt")
	h := fd.Hunks[0]

	write(t, dir, "f.txt", numbered("a", "B", "c", "d", "E"))

	_, err = StageHunk(context.Background(), dir, HunkRef{
		Path: "f.txt", OldStart: h.OldStart, OldLines: h.OldLines,
		NewStart: h.NewStart, NewLines: h.NewLines,
		FromHash: fd.FromHash, ToHash: fd.ToHash,
	})
	if err == nil {
		t.Fatal("a stale hunk was applied; it must be refused")
	}
	if !IsRefusal(err) {
		t.Fatalf("got %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "changed since the diff") {
		t.Fatalf("refusal %q does not name the reason", err)
	}
}

func TestStageHunkRefusesUnknownCoordinates(t *testing.T) {
	dir, repo := newRepo(t)
	write(t, dir, "f.txt", numbered("a", "b", "c"))
	commit(t, repo, []string{"f.txt"}, "base")
	write(t, dir, "f.txt", numbered("a", "B", "c"))

	res, err := Diff(context.Background(), dir, RefIndex, RefWorktree)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	fd := fileByPath(t, res, "f.txt")
	_, err = StageHunk(context.Background(), dir, HunkRef{
		Path: "f.txt", OldStart: 99, OldLines: 3, NewStart: 99, NewLines: 3,
		FromHash: fd.FromHash, ToHash: fd.ToHash,
	})
	if err == nil || !IsRefusal(err) {
		t.Fatalf("got %v, want a refusal for coordinates no hunk has", err)
	}
}

func TestStageHunkOnUntrackedFileStagesIt(t *testing.T) {
	dir, repo := newRepo(t)
	write(t, dir, "seed.txt", numbered("seed"))
	commit(t, repo, []string{"seed.txt"}, "base")
	write(t, dir, "new.txt", numbered("x", "y"))

	res, err := Diff(context.Background(), dir, RefIndex, RefWorktree)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	fd := fileByPath(t, res, "new.txt")
	if fd.Status != StatusUntracked {
		t.Fatalf("status = %q, want untracked", fd.Status)
	}
	h := fd.Hunks[0]
	if _, err := StageHunk(context.Background(), dir, HunkRef{
		Path: "new.txt", OldStart: h.OldStart, OldLines: h.OldLines,
		NewStart: h.NewStart, NewLines: h.NewLines,
		FromHash: fd.FromHash, ToHash: fd.ToHash,
	}); err != nil {
		t.Fatalf("stage hunk: %v", err)
	}
	staged, err := Diff(context.Background(), dir, RefHead, RefIndex)
	if err != nil {
		t.Fatalf("staged: %v", err)
	}
	sf := fileByPath(t, staged, "new.txt")
	if sf.Status != StatusAdded || sf.Additions != 2 {
		t.Fatalf("staged new.txt = %s +%d, want added +2", sf.Status, sf.Additions)
	}
}

func TestUnstageHunkReturnsIndexTowardHead(t *testing.T) {
	dir, repo := newRepo(t)
	base := numbered("l1", "l2", "l3", "l4", "l5", "l6", "l7", "l8", "l9",
		"l10", "l11", "l12", "l13", "l14", "l15", "l16", "l17", "l18", "l19", "l20")
	write(t, dir, "f.txt", base)
	commit(t, repo, []string{"f.txt"}, "base")

	edited := strings.Replace(base, "l3\n", "L3\n", 1)
	edited = strings.Replace(edited, "l18\n", "L18\n", 1)
	write(t, dir, "f.txt", edited)
	stageAll(t, repo, "f.txt")

	staged, err := Diff(context.Background(), dir, RefHead, RefIndex)
	if err != nil {
		t.Fatalf("staged: %v", err)
	}
	fd := fileByPath(t, staged, "f.txt")
	if len(fd.Hunks) != 2 {
		t.Fatalf("staged hunks = %d, want 2", len(fd.Hunks))
	}
	h := fd.Hunks[0]
	if _, err := UnstageHunk(context.Background(), dir, HunkRef{
		Path: "f.txt", OldStart: h.OldStart, OldLines: h.OldLines,
		NewStart: h.NewStart, NewLines: h.NewLines,
		FromHash: fd.FromHash, ToHash: fd.ToHash,
	}); err != nil {
		t.Fatalf("unstage hunk: %v", err)
	}

	after, err := Diff(context.Background(), dir, RefHead, RefIndex)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	af := fileByPath(t, after, "f.txt")
	if len(af.Hunks) != 1 {
		t.Fatalf("staged hunks after unstage = %d, want 1", len(af.Hunks))
	}
	for _, l := range af.Hunks[0].Lines {
		if l.Kind == LineAdd && l.Text != "L18" {
			t.Fatalf("remaining staged hunk adds %q, want L18", l.Text)
		}
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "f.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(onDisk) != edited {
		t.Fatal("UnstageHunk modified the worktree file")
	}
}

func TestStageAndUnstageWholeFile(t *testing.T) {
	dir, repo := newRepo(t)
	write(t, dir, "f.txt", numbered("a"))
	commit(t, repo, []string{"f.txt"}, "base")
	write(t, dir, "f.txt", numbered("b"))
	write(t, dir, "new.txt", numbered("n"))

	if _, err := Stage(context.Background(), dir, []string{"f.txt", "new.txt"}); err != nil {
		t.Fatalf("stage: %v", err)
	}
	st, err := Status(context.Background(), dir)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Staged) != 2 || len(st.Untracked) != 0 {
		t.Fatalf("after stage: staged=%v untracked=%v", st.Staged, st.Untracked)
	}

	res, err := Unstage(context.Background(), dir, []string{"f.txt", "new.txt"})
	if err != nil {
		t.Fatalf("unstage: %v", err)
	}
	if len(res.Status.Staged) != 0 {
		t.Fatalf("after unstage: staged=%v", res.Status.Staged)
	}
	if len(res.Status.Untracked) != 1 || res.Status.Untracked[0] != "new.txt" {
		t.Fatalf("new.txt must go back to untracked, got %v", res.Status.Untracked)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "f.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(onDisk) != numbered("b") {
		t.Fatal("Unstage changed the worktree file; it must only move the index")
	}
}

func TestRefsListsBranchesAndCommits(t *testing.T) {
	dir, repo := newRepo(t)
	write(t, dir, "f.txt", numbered("a"))
	first := commit(t, repo, []string{"f.txt"}, "first")
	write(t, dir, "f.txt", numbered("b"))
	commit(t, repo, []string{"f.txt"}, "second")

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("side"), Hash: first, Create: true}); err != nil {
		t.Fatalf("branch: %v", err)
	}

	refs, err := Refs(context.Background(), dir, 0)
	if err != nil {
		t.Fatalf("refs: %v", err)
	}
	if len(refs.Branches) < 2 {
		t.Fatalf("branches = %v, want at least two", refs.Branches)
	}
	if refs.Current != "side" {
		t.Fatalf("current = %q, want side", refs.Current)
	}
	if len(refs.Commits) == 0 {
		t.Fatal("no commits listed")
	}
	if len(refs.PseudoRefs) != 3 {
		t.Fatalf("pseudoRefs = %v", refs.PseudoRefs)
	}
}

func TestNoRepositoryIsARefusal(t *testing.T) {
	dir := t.TempDir()
	if _, err := Status(context.Background(), dir); err == nil {
		t.Fatal("a directory outside any repository must be refused")
	} else if !IsRefusal(err) {
		t.Fatalf("got %v, want a refusal", err)
	}
}

// TestHunkRoundTrip is the correctness property that matters most: applying
// every hunk of a diff, in order, must reconstruct the `to` side exactly.
func TestHunkRoundTrip(t *testing.T) {
	cases := []struct{ from, to string }{
		{"", "a\nb\nc\n"},
		{"a\nb\nc\n", ""},
		{"a\nb\nc\n", "a\nb\nc\n"},
		{"a\nb\nc\n", "a\nB\nc\n"},
		{"a\nb\nc\n", "a\nb\nc"},
		{"a\nb\nc", "a\nb\nc\n"},
		{numbered("1", "2", "3", "4", "5", "6", "7", "8", "9", "10"),
			numbered("1", "X", "3", "4", "5", "6", "7", "8", "Y", "10")},
		{numbered("1", "2", "3", "4", "5", "6", "7", "8", "9", "10"),
			numbered("0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11")},
		{numbered("keep", "drop", "keep2"), numbered("keep", "keep2")},
		{"one line no newline", "another line no newline"},
	}
	for _, tc := range cases {
		hunks, truncated := diffHunks(tc.from, tc.to)
		if truncated {
			t.Fatalf("unexpected truncation for %q -> %q", tc.from, tc.to)
		}
		if recompose(decompose(tc.from)) != tc.from {
			t.Fatalf("decompose/recompose did not round-trip %q", tc.from)
		}
		// Apply from the last hunk backwards: earlier coordinates stay valid.
		content := tc.from
		for i := len(hunks) - 1; i >= 0; i-- {
			var ok bool
			content, ok = applyHunk(content, hunks[i])
			if !ok {
				t.Fatalf("applyHunk refused hunk %d of %q -> %q", i, tc.from, tc.to)
			}
		}
		if content != tc.to {
			t.Fatalf("round trip %q -> %q produced %q", tc.from, tc.to, content)
		}
	}
}

// TestReverseHunkRoundTrip is the same property for the unstage direction.
func TestReverseHunkRoundTrip(t *testing.T) {
	from := numbered("1", "2", "3", "4", "5", "6", "7", "8", "9", "10",
		"11", "12", "13", "14", "15", "16", "17", "18", "19", "20")
	to := numbered("1", "X", "3", "4", "5", "6", "7", "8", "9", "10",
		"11", "12", "13", "14", "15", "16", "17", "18", "Y", "20")
	hunks, _ := diffHunks(from, to)
	if len(hunks) != 2 {
		t.Fatalf("hunks = %d, want 2", len(hunks))
	}
	content := to
	for i := len(hunks) - 1; i >= 0; i-- {
		var ok bool
		content, ok = applyHunk(content, reverseHunk(hunks[i]))
		if !ok {
			t.Fatalf("reverse applyHunk refused hunk %d", i)
		}
	}
	if content != from {
		t.Fatalf("reverse round trip produced %q, want %q", content, from)
	}
}

func TestBinaryFileReportsBinaryNotHunks(t *testing.T) {
	dir, repo := newRepo(t)
	write(t, dir, "seed.txt", numbered("seed"))
	commit(t, repo, []string{"seed.txt"}, "base")
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0x00, 0x01, 0x02, 0x00}, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := Diff(context.Background(), dir, RefIndex, RefWorktree)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	fd := fileByPath(t, res, "blob.bin")
	if !fd.Binary || len(fd.Hunks) != 0 {
		t.Fatalf("binary=%v hunks=%d, want binary with no hunks", fd.Binary, len(fd.Hunks))
	}
	if _, err := StageHunk(context.Background(), dir, HunkRef{
		Path: "blob.bin", FromHash: fd.FromHash, ToHash: fd.ToHash,
	}); err == nil || !IsRefusal(err) {
		t.Fatalf("hunk-staging a binary file returned %v, want a refusal", err)
	}
}
