package gitreview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const (
	maxDiffFiles = 300

	maxDiffBytes = 2 << 20

	binarySniff = 8000
)

// FileDiff is one file's change; Path/OldPath are workspace-relative, and FromHash/ToHash (content hashes, "" for an absent side) let StageHunk/UnstageHunk refuse a hunk that has moved on.
type FileDiff struct {
	Path      string `json:"path"`
	OldPath   string `json:"oldPath,omitempty"`
	Status    string `json:"status"`
	Binary    bool   `json:"binary"`
	Truncated bool   `json:"truncated"`
	FromHash  string `json:"fromHash"`
	ToHash    string `json:"toHash"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Hunks     []Hunk `json:"hunks"`
}

// DiffResult is one ref-to-ref comparison; Truncated reports the file list was cut at maxDiffFiles, a per-file truncation lives on the file itself.
type DiffResult struct {
	From      string     `json:"from"`
	To        string     `json:"to"`
	Files     []FileDiff `json:"files"`
	Truncated bool       `json:"truncated"`
}

type sideKind int

const (
	sideWorktree sideKind = iota
	sideIndex
	sideCommit
)

type side struct {
	kind   sideKind
	name   string
	commit *object.Commit
}

func (s *scope) resolveSide(ref string) (*side, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, refusef("a ref is required (%s, %s, %s, a branch, a tag, or a commit)", RefWorktree, RefIndex, RefHead)
	}
	switch ref {
	case RefWorktree:
		return &side{kind: sideWorktree, name: ref}, nil
	case RefIndex:
		return &side{kind: sideIndex, name: ref}, nil
	case RefHead:
		commit, ok, err := headCommit(s.repo)
		if err != nil {
			return nil, fmt.Errorf("gitreview: %w", err)
		}
		if !ok {
			return &side{kind: sideCommit, name: ref}, nil
		}
		return &side{kind: sideCommit, name: ref, commit: commit}, nil
	}
	hash, err := s.repo.ResolveRevision(plumbing.Revision(ref))
	if err != nil {
		return nil, refusef("cannot resolve %q in this repository — pass %s, %s, %s, a branch, a tag, or a commit hash", ref, RefWorktree, RefIndex, RefHead)
	}
	commit, err := s.repo.CommitObject(*hash)
	if err != nil {
		return nil, refusef("%q does not name a commit: %v", ref, err)
	}
	return &side{kind: sideCommit, name: ref, commit: commit}, nil
}

type blob struct {
	exists  bool
	content string
	hash    string
}

func contentHash(exists bool, content string) string {
	if !exists {
		return ""
	}
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (s *scope) read(sd *side, repoPath string) (blob, error) {
	switch sd.kind {
	case sideWorktree:
		data, err := os.ReadFile(filepath.Join(s.repoRoot, filepath.FromSlash(repoPath)))
		if err != nil {
			if os.IsNotExist(err) {
				return blob{}, nil
			}
			return blob{}, fmt.Errorf("gitreview: read %s: %w", repoPath, err)
		}
		return blob{exists: true, content: string(data), hash: contentHash(true, string(data))}, nil
	case sideIndex:
		return s.readIndex(repoPath)
	default:
		if sd.commit == nil {
			return blob{}, nil
		}
		f, err := sd.commit.File(repoPath)
		if err != nil {
			return blob{}, nil
		}
		content, err := f.Contents()
		if err != nil {
			return blob{}, fmt.Errorf("gitreview: read %s at %s: %w", repoPath, sd.name, err)
		}
		return blob{exists: true, content: content, hash: contentHash(true, content)}, nil
	}
}

func (s *scope) readIndex(repoPath string) (blob, error) {
	idx, err := s.repo.Storer.Index()
	if err != nil {
		return blob{}, fmt.Errorf("gitreview: %w", err)
	}
	entry, err := idx.Entry(repoPath)
	if err != nil {
		return blob{}, nil
	}
	obj, err := s.repo.BlobObject(entry.Hash)
	if err != nil {
		return blob{}, nil
	}
	rc, err := obj.Reader()
	if err != nil {
		return blob{}, fmt.Errorf("gitreview: %w", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return blob{}, fmt.Errorf("gitreview: %w", err)
	}
	return blob{exists: true, content: string(data), hash: contentHash(true, string(data))}, nil
}

type candidate struct {
	path    string
	oldPath string
}

func (s *scope) candidates(ctx context.Context, from, to *side, filter string) ([]candidate, error) {
	seen := map[string]candidate{}
	add := func(path, oldPath string) {
		if path == "" || !underPath(path, filter) {
			return
		}
		if _, ok := s.inScope(path); !ok {
			return
		}
		existing, ok := seen[path]
		if ok && existing.oldPath != "" {
			return
		}
		seen[path] = candidate{path: path, oldPath: oldPath}
	}

	needStatus := from.kind != sideCommit || to.kind != sideCommit
	if needStatus {
		st, err := s.worktreeStatus()
		if err != nil {
			return nil, err
		}
		for path := range st {
			add(path, "")
		}
		head, _, err := headCommit(s.repo)
		if err != nil {
			return nil, fmt.Errorf("gitreview: %w", err)
		}
		for _, sd := range []*side{from, to} {
			if sd.kind != sideCommit || sd.commit == nil {
				continue
			}
			if head != nil && sd.commit.Hash == head.Hash {
				continue
			}
			if err := s.addTreeDiff(ctx, sd.commit, head, add); err != nil {
				return nil, err
			}
		}
	} else {
		if err := s.addTreeDiff(ctx, from.commit, to.commit, add); err != nil {
			return nil, err
		}
	}

	out := make([]candidate, 0, len(seen))
	for _, c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

func (s *scope) addTreeDiff(ctx context.Context, from, to *object.Commit, add func(path, oldPath string)) error {
	fromTree, err := commitTree(from)
	if err != nil {
		return err
	}
	toTree, err := commitTree(to)
	if err != nil {
		return err
	}
	changes, err := object.DiffTreeWithOptions(ctx, fromTree, toTree, object.DefaultDiffTreeOptions)
	if err != nil {
		return fmt.Errorf("gitreview: %w", err)
	}
	for _, ch := range changes {
		switch {
		case ch.From.Name == "":
			add(ch.To.Name, "")
		case ch.To.Name == "":
			add(ch.From.Name, "")
		case ch.From.Name != ch.To.Name:
			add(ch.To.Name, ch.From.Name)
		default:
			add(ch.To.Name, "")
		}
	}
	return nil
}

func commitTree(c *object.Commit) (*object.Tree, error) {
	if c == nil {
		return &object.Tree{}, nil
	}
	t, err := c.Tree()
	if err != nil {
		return nil, fmt.Errorf("gitreview: %w", err)
	}
	return t, nil
}

func isBinary(content string) bool {
	if len(content) > binarySniff {
		content = content[:binarySniff]
	}
	return bytes.IndexByte([]byte(content), 0) >= 0
}

// Diff compares two refs (WORKTREE, INDEX, HEAD, or any revision go-git can resolve) over the workspace subtree at root, narrowed to paths when given.
func Diff(ctx context.Context, root, from, to string, paths ...string) (*DiffResult, error) {
	sc, err := open(root)
	if err != nil {
		return nil, err
	}
	fromSide, err := sc.resolveSide(from)
	if err != nil {
		return nil, err
	}
	toSide, err := sc.resolveSide(to)
	if err != nil {
		return nil, err
	}

	filters, err := sc.pathFilters(paths)
	if err != nil {
		return nil, err
	}

	out := &DiffResult{From: fromSide.name, To: toSide.name, Files: []FileDiff{}}
	collected := map[string]bool{}
	for _, filter := range filters {
		cands, cErr := sc.candidates(ctx, fromSide, toSide, filter)
		if cErr != nil {
			return nil, cErr
		}
		for _, c := range cands {
			if collected[c.path] {
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if len(out.Files) >= maxDiffFiles {
				out.Truncated = true
				break
			}
			fd, ok, fErr := sc.fileDiff(fromSide, toSide, c)
			if fErr != nil {
				return nil, fErr
			}
			collected[c.path] = true
			if ok {
				out.Files = append(out.Files, fd)
			}
		}
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	return out, nil
}

func (s *scope) pathFilters(paths []string) ([]string, error) {
	filters := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		rel, err := s.toRepoPath(p)
		if err != nil {
			return nil, err
		}
		filters = append(filters, rel)
	}
	if len(filters) == 0 {
		filters = append(filters, s.repoRel)
	}
	return filters, nil
}

func (s *scope) fileDiff(from, to *side, c candidate) (FileDiff, bool, error) {
	fromPath := c.oldPath
	if fromPath == "" {
		fromPath = c.path
	}
	fromBlob, err := s.read(from, fromPath)
	if err != nil {
		return FileDiff{}, false, err
	}
	toBlob, err := s.read(to, c.path)
	if err != nil {
		return FileDiff{}, false, err
	}
	if !fromBlob.exists && !toBlob.exists {
		return FileDiff{}, false, nil
	}
	if fromBlob.exists && toBlob.exists && fromBlob.content == toBlob.content && c.oldPath == "" {
		return FileDiff{}, false, nil
	}

	rel, ok := s.inScope(c.path)
	if !ok || rel == "" {
		return FileDiff{}, false, nil
	}
	fd := FileDiff{
		Path:     rel,
		Status:   StatusModified,
		FromHash: fromBlob.hash,
		ToHash:   toBlob.hash,
		Hunks:    []Hunk{},
	}
	if c.oldPath != "" {
		if oldRel, inScope := s.inScope(c.oldPath); inScope && oldRel != "" {
			fd.OldPath = oldRel
			fd.Status = StatusRenamed
		}
	}
	switch {
	case !fromBlob.exists:
		fd.Status = StatusAdded
		if to.kind == sideWorktree && !s.tracked(c.path) {
			fd.Status = StatusUntracked
		}
	case !toBlob.exists:
		fd.Status = StatusDeleted
	}

	if isBinary(fromBlob.content) || isBinary(toBlob.content) {
		fd.Binary = true
		return fd, true, nil
	}
	if len(fromBlob.content) > maxDiffBytes || len(toBlob.content) > maxDiffBytes {
		fd.Truncated = true
		return fd, true, nil
	}

	hunks, truncated := diffHunks(fromBlob.content, toBlob.content)
	fd.Hunks = hunks
	fd.Truncated = truncated
	for _, h := range hunks {
		for _, l := range h.Lines {
			switch l.Kind {
			case LineAdd:
				fd.Additions++
			case LineDel:
				fd.Deletions++
			}
		}
	}
	return fd, true, nil
}

func (s *scope) tracked(repoPath string) bool {
	idx, err := s.repo.Storer.Index()
	if err != nil {
		return false
	}
	_, err = idx.Entry(repoPath)
	return err == nil
}
