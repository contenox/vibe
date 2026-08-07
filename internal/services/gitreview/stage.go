package gitreview

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// HunkRef names one hunk of one file, in the coordinates a Diff reported it.
//
// FromHash and ToHash are the FileDiff's hashes, taken with that diff. They are
// not optional: they are the whole stale-diff guard. StageHunk re-reads both
// sides, re-derives the hunks, and refuses unless the content still hashes to
// what the caller saw AND a hunk with exactly these coordinates still exists.
// Line numbers from a diff computed against different content would otherwise
// silently corrupt the file, which is the one outcome this package must never
// produce.
type HunkRef struct {
	Path     string `json:"path"`
	OldStart int    `json:"oldStart"`
	OldLines int    `json:"oldLines"`
	NewStart int    `json:"newStart"`
	NewLines int    `json:"newLines"`
	FromHash string `json:"fromHash"`
	ToHash   string `json:"toHash"`
}

// ApplyResult is what an index mutation answers with. Status is re-read after
// the write so a review pane re-renders from the response instead of racing a
// refetch against its own mutation.
type ApplyResult struct {
	Paths   []string      `json:"paths"`
	Staged  bool          `json:"staged"`
	Removed bool          `json:"removed"`
	Status  *StatusResult `json:"status"`
}

// StageHunk applies ONE hunk of the INDEX→WORKTREE diff to the index, leaving
// the rest of the file's worktree changes unstaged. It is `git add -p` for a
// single hunk, done in-process: the new index content is written as a fresh
// blob and the index entry re-pointed at it. The worktree file is never
// touched.
func StageHunk(ctx context.Context, root string, ref HunkRef) (*ApplyResult, error) {
	sc, err := open(root)
	if err != nil {
		return nil, err
	}
	return sc.applyHunkRef(ctx, ref, RefIndex, RefWorktree, true)
}

// UnstageHunk reverts ONE hunk of the HEAD→INDEX diff in the index, leaving the
// file's other staged changes staged. `git reset -p` for a single hunk. The
// worktree file is never touched.
func UnstageHunk(ctx context.Context, root string, ref HunkRef) (*ApplyResult, error) {
	sc, err := open(root)
	if err != nil {
		return nil, err
	}
	return sc.applyHunkRef(ctx, ref, RefHead, RefIndex, false)
}

// applyHunkRef is the shared body of StageHunk and UnstageHunk.
//
// stage=true rewrites the index toward the `to` side (the worktree); stage=false
// rewrites it back toward the `from` side (HEAD). Either way the index is the
// only thing that moves.
func (s *scope) applyHunkRef(ctx context.Context, ref HunkRef, fromRef, toRef string, stage bool) (*ApplyResult, error) {
	repoPath, err := s.toRepoPath(ref.Path)
	if err != nil {
		return nil, err
	}
	if repoPath == "" || repoPath == s.repoRel {
		return nil, refusef("path is required and must name a file")
	}
	fromSide, err := s.resolveSide(fromRef)
	if err != nil {
		return nil, err
	}
	toSide, err := s.resolveSide(toRef)
	if err != nil {
		return nil, err
	}
	fromBlob, err := s.read(fromSide, repoPath)
	if err != nil {
		return nil, err
	}
	toBlob, err := s.read(toSide, repoPath)
	if err != nil {
		return nil, err
	}

	// The stale guard. Both sides must still hash to what the caller was shown;
	// an edit, a concurrent stage, or a checkout between the diff and the apply
	// all land here as a refusal rather than as a shifted patch.
	if fromBlob.hash != ref.FromHash {
		return nil, refusef("%s changed since the diff was computed (%s side) — refresh the diff and try again", ref.Path, fromRef)
	}
	if toBlob.hash != ref.ToHash {
		return nil, refusef("%s changed since the diff was computed (%s side) — refresh the diff and try again", ref.Path, toRef)
	}
	if isBinary(fromBlob.content) || isBinary(toBlob.content) {
		return nil, refusef("%s is binary — stage or unstage the whole file", ref.Path)
	}
	if len(fromBlob.content) > maxDiffBytes || len(toBlob.content) > maxDiffBytes {
		return nil, refusef("%s is too large for hunk-level staging — stage or unstage the whole file", ref.Path)
	}

	hunks, truncated := diffHunks(fromBlob.content, toBlob.content)
	if truncated {
		return nil, refusef("%s changed too much to apply one hunk — stage or unstage the whole file", ref.Path)
	}
	match, ok := findHunk(hunks, ref)
	if !ok {
		return nil, refusef("no hunk at -%d,%d +%d,%d in %s — refresh the diff and try again",
			ref.OldStart, ref.OldLines, ref.NewStart, ref.NewLines, ref.Path)
	}

	var (
		content string
		applied bool
	)
	if stage {
		content, applied = applyHunk(fromBlob.content, match)
	} else {
		content, applied = applyHunk(toBlob.content, reverseHunk(match))
	}
	if !applied {
		return nil, refusef("the hunk does not match %s any more — refresh the diff and try again", ref.Path)
	}

	// An index entry whose content went to empty for a path that does not exist
	// on the far side is a removal, not a zero-byte file: keeping it would stage
	// an empty file nobody asked for.
	remove := content == "" && ((stage && !toBlob.exists) || (!stage && !fromBlob.exists))
	if err := s.writeIndexEntry(repoPath, content, remove); err != nil {
		return nil, err
	}
	st, err := s.status(ctx)
	if err != nil {
		return nil, err
	}
	return &ApplyResult{Paths: []string{ref.Path}, Staged: stage, Removed: remove, Status: st}, nil
}

// findHunk locates the hunk the caller named. Coordinates are an exact match:
// with both content hashes already verified the hunk list is deterministic, so
// a near-miss means the caller is describing a diff this repository never
// produced.
func findHunk(hunks []Hunk, ref HunkRef) (Hunk, bool) {
	for _, h := range hunks {
		if h.OldStart == ref.OldStart && h.OldLines == ref.OldLines &&
			h.NewStart == ref.NewStart && h.NewLines == ref.NewLines {
			return h, true
		}
	}
	return Hunk{}, false
}

// reverseHunk turns a from→to hunk into the to→from hunk that undoes it.
func reverseHunk(h Hunk) Hunk {
	r := Hunk{
		OldStart: h.NewStart,
		OldLines: h.NewLines,
		NewStart: h.OldStart,
		NewLines: h.OldLines,
		Lines:    make([]Line, 0, len(h.Lines)),
	}
	for _, l := range h.Lines {
		kind := l.Kind
		switch kind {
		case LineAdd:
			kind = LineDel
		case LineDel:
			kind = LineAdd
		}
		r.Lines = append(r.Lines, Line{Kind: kind, Text: l.Text})
	}
	return r
}

// writeIndexEntry points the index entry for repoPath at a blob holding
// content, creating the entry if it is new. remove drops the entry instead.
//
// The blob is written to the repository's object store first: an index entry
// naming an object that is not there would leave the repository unreadable, so
// the object always exists before anything references it.
func (s *scope) writeIndexEntry(repoPath, content string, remove bool) error {
	idx, err := s.repo.Storer.Index()
	if err != nil {
		return fmt.Errorf("gitreview: %w", err)
	}
	if remove {
		if _, err := idx.Remove(repoPath); err != nil && err != index.ErrEntryNotFound {
			return fmt.Errorf("gitreview: %w", err)
		}
		return s.setIndex(idx)
	}

	hash, err := s.writeBlob(content)
	if err != nil {
		return err
	}
	entry, err := idx.Entry(repoPath)
	if err != nil {
		entry = idx.Add(repoPath)
		entry.Mode = s.worktreeMode(repoPath)
		entry.CreatedAt = time.Now()
	}
	if entry.Mode == filemode.Empty {
		entry.Mode = s.worktreeMode(repoPath)
	}
	entry.Hash = hash
	entry.Size = uint32(len(content))
	entry.ModifiedAt = time.Now()
	// An intent-to-add entry carries no content; writing one gives it content,
	// so the flag has to go or git would keep reporting the path as unstaged.
	entry.IntentToAdd = false
	return s.setIndex(idx)
}

func (s *scope) setIndex(idx *index.Index) error {
	if err := s.repo.Storer.SetIndex(idx); err != nil {
		return fmt.Errorf("gitreview: %w", err)
	}
	return nil
}

// writeBlob stores content as a blob object and returns its hash.
func (s *scope) writeBlob(content string) (plumbing.Hash, error) {
	obj := s.repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(content)))
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("gitreview: %w", err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		_ = w.Close()
		return plumbing.ZeroHash, fmt.Errorf("gitreview: %w", err)
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("gitreview: %w", err)
	}
	hash, err := s.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("gitreview: %w", err)
	}
	return hash, nil
}

// worktreeMode is the file mode to record for a path, read from the worktree
// and defaulting to a regular file when it is not there.
func (s *scope) worktreeMode(repoPath string) filemode.FileMode {
	info, err := os.Lstat(filepath.Join(s.repoRoot, filepath.FromSlash(repoPath)))
	if err != nil {
		return filemode.Regular
	}
	mode, err := filemode.NewFromOSFileMode(info.Mode())
	if err != nil {
		return filemode.Regular
	}
	return mode
}

// ---------------------------------------------------------------------------
// File-level staging
// ---------------------------------------------------------------------------

// Stage stages whole paths. A directory stages everything changed under it.
// This is `git add`, and it is what a review surface falls back to for a binary
// or oversized file.
func Stage(ctx context.Context, root string, paths []string) (*ApplyResult, error) {
	sc, err := open(root)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, refusef("paths is required")
	}
	wt, err := sc.repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("gitreview: %w", err)
	}
	staged := make([]string, 0, len(paths))
	for _, p := range paths {
		repoPath, pErr := sc.toRepoPath(p)
		if pErr != nil {
			return nil, pErr
		}
		if repoPath == "" {
			// The workspace root itself. AddWithOptions(All) would reach the
			// whole repository, which may be wider than the root: stage the
			// scoped subtree explicitly instead.
			if err := sc.stageAllInScope(wt); err != nil {
				return nil, err
			}
			staged = append(staged, ".")
			continue
		}
		if err := wt.AddWithOptions(&git.AddOptions{Path: repoPath}); err != nil {
			return nil, refusef("cannot stage %s: %v", p, err)
		}
		staged = append(staged, p)
	}
	st, err := sc.status(ctx)
	if err != nil {
		return nil, err
	}
	return &ApplyResult{Paths: staged, Staged: true, Status: st}, nil
}

// stageAllInScope stages every changed path under the workspace subtree, one at
// a time, so a workspace root inside a larger repository never stages its
// siblings.
func (s *scope) stageAllInScope(wt *git.Worktree) error {
	st, err := s.worktreeStatus()
	if err != nil {
		return err
	}
	for _, repoPath := range sortedPaths(st) {
		if _, ok := s.inScope(repoPath); !ok {
			continue
		}
		fs := st[repoPath]
		if fs.Worktree == git.Unmodified {
			continue
		}
		if err := wt.AddWithOptions(&git.AddOptions{Path: repoPath}); err != nil {
			return refusef("cannot stage %s: %v", repoPath, err)
		}
	}
	return nil
}

// Unstage restores the index entry for whole paths back to HEAD, leaving the
// worktree file untouched. A path HEAD does not know is dropped from the index
// entirely, which is what `git reset` does to a newly added file.
func Unstage(ctx context.Context, root string, paths []string) (*ApplyResult, error) {
	sc, err := open(root)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, refusef("paths is required")
	}
	targets, err := sc.expandStaged(paths)
	if err != nil {
		return nil, err
	}
	head, _, err := headCommit(sc.repo)
	if err != nil {
		return nil, fmt.Errorf("gitreview: %w", err)
	}
	var headTree *object.Tree
	if head != nil {
		if headTree, err = head.Tree(); err != nil {
			return nil, fmt.Errorf("gitreview: %w", err)
		}
	}

	idx, err := sc.repo.Storer.Index()
	if err != nil {
		return nil, fmt.Errorf("gitreview: %w", err)
	}
	removed := false
	for _, repoPath := range targets {
		var te *object.TreeEntry
		if headTree != nil {
			if found, fErr := headTree.FindEntry(repoPath); fErr == nil {
				te = found
			}
		}
		if te == nil {
			if _, rErr := idx.Remove(repoPath); rErr != nil && rErr != index.ErrEntryNotFound {
				return nil, fmt.Errorf("gitreview: %w", rErr)
			}
			removed = true
			continue
		}
		entry, eErr := idx.Entry(repoPath)
		if eErr != nil {
			entry = idx.Add(repoPath)
			entry.CreatedAt = time.Now()
		}
		entry.Hash = te.Hash
		entry.Mode = te.Mode
		entry.ModifiedAt = time.Now()
		entry.IntentToAdd = false
		if blob, bErr := sc.repo.BlobObject(te.Hash); bErr == nil {
			entry.Size = uint32(blob.Size)
		}
	}
	if err := sc.setIndex(idx); err != nil {
		return nil, err
	}
	st, err := sc.status(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(targets))
	for _, repoPath := range targets {
		if rel, ok := sc.inScope(repoPath); ok && rel != "" {
			out = append(out, rel)
		}
	}
	return &ApplyResult{Paths: out, Staged: false, Removed: removed, Status: st}, nil
}

// expandStaged turns caller paths into the concrete staged repo-relative paths
// they cover: a file is itself, a directory is every staged path under it.
func (s *scope) expandStaged(paths []string) ([]string, error) {
	st, err := s.worktreeStatus()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		repoPath, pErr := s.toRepoPath(p)
		if pErr != nil {
			return nil, pErr
		}
		for _, candidatePath := range sortedPaths(st) {
			if _, ok := s.inScope(candidatePath); !ok {
				continue
			}
			fs := st[candidatePath]
			if fs.Staging == git.Unmodified || fs.Staging == git.Untracked {
				continue
			}
			if !underPath(candidatePath, repoPath) {
				continue
			}
			if seen[candidatePath] {
				continue
			}
			seen[candidatePath] = true
			out = append(out, candidatePath)
		}
	}
	if len(out) == 0 {
		return nil, refusef("nothing staged under %s", strings.Join(paths, ", "))
	}
	return out, nil
}

func sortedPaths(st git.Status) []string {
	out := make([]string, 0, len(st))
	for p := range st {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
