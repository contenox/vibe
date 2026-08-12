// Package gitreview projects a repository's changes as structured data for a human review surface, diffing between any two of WORKTREE, INDEX, HEAD, or a named revision. Containment is the caller's workspace root, not the repository root: everything reported or touched stays inside that subtree even when the repository sits above it.
package gitreview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/services/vfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Pseudo-refs accepted by Diff in either position, alongside any revision go-git can resolve (branch, tag, hash, HEAD~2).
const (
	RefWorktree = "WORKTREE"
	RefIndex    = "INDEX"
	RefHead     = "HEAD"
)

// File statuses: fixed spellings a renderer switches on.
const (
	StatusAdded     = "added"
	StatusDeleted   = "deleted"
	StatusModified  = "modified"
	StatusRenamed   = "renamed"
	StatusUntracked = "untracked"
)

const maxRefCommits = 100

// Refusal is a caller-fixable rejection — a bad ref, a path outside the root, a stale hunk — mapped by the bus onto invalid-params; anything else is an internal error.
type Refusal struct{ msg string }

func (e *Refusal) Error() string { return e.msg }

func refusef(format string, args ...any) error {
	return &Refusal{msg: fmt.Sprintf(format, args...)}
}

// IsRefusal reports whether err is a caller-fixable rejection.
func IsRefusal(err error) bool {
	var r *Refusal
	return errors.As(err, &r)
}

// ErrNoRepository is the refusal for a workspace root that is not under version control, surfaced as a Refusal rather than an internal failure.
var ErrNoRepository = &Refusal{msg: "this workspace is not inside a git repository"}

type scope struct {
	repo     *git.Repository
	repoRoot string
	root     string
	repoRel  string
}

func open(root string) (*scope, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, refusef("gitreview: no workspace root")
	}
	resolved, err := vfs.ResolveRoot(root)
	if err != nil {
		return nil, refusef("gitreview: %v", err)
	}
	repoRoot, ok := findRepoRoot(resolved)
	if !ok {
		return nil, ErrNoRepository
	}
	repo, err := git.PlainOpenWithOptions(repoRoot, &git.PlainOpenOptions{EnableDotGitCommonDir: true})
	if err != nil {
		return nil, refusef("gitreview: cannot open the git repository at %s: %v", repoRoot, err)
	}
	rel, err := filepath.Rel(repoRoot, resolved)
	if err != nil {
		return nil, refusef("gitreview: %s is not inside the repository at %s", resolved, repoRoot)
	}
	prefix := filepath.ToSlash(rel)
	if prefix == "." {
		prefix = ""
	}
	return &scope{repo: repo, repoRoot: repoRoot, root: resolved, repoRel: prefix}, nil
}

func findRepoRoot(start string) (string, bool) {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func (s *scope) inScope(repoPath string) (string, bool) {
	if s.repoRel == "" {
		return repoPath, true
	}
	if repoPath == s.repoRel {
		return "", true
	}
	prefix := s.repoRel + "/"
	if !strings.HasPrefix(repoPath, prefix) {
		return "", false
	}
	return strings.TrimPrefix(repoPath, prefix), true
}

func (s *scope) toRepoPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "." {
		return s.repoRel, nil
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return "", refusef("path %q must be relative to the workspace root", p)
	}
	abs, err := vfs.Contain(s.root, p)
	if err != nil {
		if errors.Is(err, vfs.ErrEscape) {
			return "", refusef("path %q escapes the workspace root", p)
		}
		return "", refusef("path %q: %v", p, err)
	}
	rel, err := filepath.Rel(s.repoRoot, abs)
	if err != nil {
		return "", refusef("path %q is not inside the repository", p)
	}
	cleaned := filepath.ToSlash(rel)
	if cleaned == "." {
		cleaned = ""
	}
	if strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return "", refusef("path %q escapes the workspace root", p)
	}
	return cleaned, nil
}

func underPath(repoPath, filter string) bool {
	if filter == "" {
		return true
	}
	return repoPath == filter || strings.HasPrefix(repoPath, filter+"/")
}

func headCommit(repo *git.Repository) (c *object.Commit, ok bool, err error) {
	ref, err := repo.Head()
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, false, err
	}
	return commit, true, nil
}

func shortHash(h plumbing.Hash) string {
	s := h.String()
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func subjectOf(msg string) string {
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return strings.TrimSpace(msg)
}

// StatusEntry is one changed path as the review surface addresses it: Path is
// workspace-relative, Code is git's own single letter.
type StatusEntry struct {
	Path string `json:"path"`
	Code string `json:"code"`
}

// StatusResult is the review surface's status: Staged, Unstaged and Untracked are the three section lists a review pane renders, and Clean is true when all three are empty.
type StatusResult struct {
	Branch    string        `json:"branch"`
	Head      string        `json:"head,omitempty"`
	HeadShort string        `json:"headShort,omitempty"`
	Subject   string        `json:"subject,omitempty"`
	Detached  bool          `json:"detached"`
	Clean     bool          `json:"clean"`
	Staged    []StatusEntry `json:"staged"`
	Unstaged  []StatusEntry `json:"unstaged"`
	Untracked []string      `json:"untracked"`
}

// Status reports the repository state restricted to the workspace subtree.
func Status(ctx context.Context, root string) (*StatusResult, error) {
	sc, err := open(root)
	if err != nil {
		return nil, err
	}
	return sc.status(ctx)
}

func (s *scope) status(_ context.Context) (*StatusResult, error) {
	st, err := s.worktreeStatus()
	if err != nil {
		return nil, err
	}
	out := &StatusResult{
		Staged:    []StatusEntry{},
		Unstaged:  []StatusEntry{},
		Untracked: []string{},
	}
	if ref, hErr := s.repo.Head(); hErr == nil {
		if ref.Name().IsBranch() {
			out.Branch = ref.Name().Short()
		} else {
			out.Detached = true
			out.Branch = "(detached)"
		}
	} else {
		out.Branch = "(no commits yet)"
	}
	if commit, ok, cErr := headCommit(s.repo); cErr == nil && ok {
		out.Head = commit.Hash.String()
		out.HeadShort = shortHash(commit.Hash)
		out.Subject = subjectOf(commit.Message)
	}

	paths := make([]string, 0, len(st))
	for p := range st {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, repoPath := range paths {
		rel, ok := s.inScope(repoPath)
		if !ok || rel == "" {
			continue
		}
		fs := st[repoPath]
		if fs.Staging == git.Untracked && fs.Worktree == git.Untracked {
			out.Untracked = append(out.Untracked, rel)
			continue
		}
		if fs.Staging != git.Unmodified && fs.Staging != git.Untracked {
			out.Staged = append(out.Staged, StatusEntry{Path: rel, Code: string(fs.Staging)})
		}
		if fs.Worktree != git.Unmodified && fs.Worktree != git.Untracked {
			out.Unstaged = append(out.Unstaged, StatusEntry{Path: rel, Code: string(fs.Worktree)})
		}
	}
	out.Clean = len(out.Staged)+len(out.Unstaged)+len(out.Untracked) == 0
	return out, nil
}

func (s *scope) worktreeStatus() (git.Status, error) {
	wt, err := s.repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("gitreview: %w", err)
	}
	st, err := wt.Status()
	if err != nil {
		return nil, fmt.Errorf("gitreview: %w", err)
	}
	return st, nil
}

// Branch is one local branch as a ref picker lists it.
type Branch struct {
	Name    string `json:"name"`
	Hash    string `json:"hash"`
	Short   string `json:"short"`
	Current bool   `json:"current"`
}

// Commit is one commit as a ref picker lists it.
type Commit struct {
	Hash    string `json:"hash"`
	Short   string `json:"short"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
	Email   string `json:"email"`
	Date    string `json:"date"`
}

// RefsResult is everything a ref picker needs: the pseudo-refs it may offer,
// the local branches, and recent history.
type RefsResult struct {
	Current    string   `json:"current"`
	Detached   bool     `json:"detached"`
	PseudoRefs []string `json:"pseudoRefs"`
	Branches   []Branch `json:"branches"`
	Commits    []Commit `json:"commits"`
}

// Refs lists the branches and recent commits of the repository containing root; limit bounds the commit list (0 means a sensible default).
func Refs(ctx context.Context, root string, limit int) (*RefsResult, error) {
	sc, err := open(root)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > maxRefCommits {
		limit = maxRefCommits
	}

	out := &RefsResult{
		PseudoRefs: []string{RefWorktree, RefIndex, RefHead},
		Branches:   []Branch{},
		Commits:    []Commit{},
	}
	if ref, hErr := sc.repo.Head(); hErr == nil {
		if ref.Name().IsBranch() {
			out.Current = ref.Name().Short()
		} else {
			out.Detached = true
			out.Current = shortHash(ref.Hash())
		}
	}

	iter, err := sc.repo.Branches()
	if err != nil {
		return nil, fmt.Errorf("gitreview: %w", err)
	}
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().Short()
		out.Branches = append(out.Branches, Branch{
			Name:    name,
			Hash:    ref.Hash().String(),
			Short:   shortHash(ref.Hash()),
			Current: !out.Detached && name == out.Current,
		})
		return nil
	})
	iter.Close()
	if err != nil {
		return nil, fmt.Errorf("gitreview: %w", err)
	}
	sort.Slice(out.Branches, func(i, j int) bool { return out.Branches[i].Name < out.Branches[j].Name })

	logIter, err := sc.repo.Log(&git.LogOptions{Order: git.LogOrderCommitterTime})
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return out, nil
		}
		return nil, fmt.Errorf("gitreview: %w", err)
	}
	defer logIter.Close()
	count := 0
	err = logIter.ForEach(func(c *object.Commit) error {
		if count >= limit {
			// go-git's ForEach stops on any non-nil error; this is the stop signal, not a real failure.
			return errStopIteration
		}
		count++
		out.Commits = append(out.Commits, Commit{
			Hash:    c.Hash.String(),
			Short:   shortHash(c.Hash),
			Subject: subjectOf(c.Message),
			Author:  c.Author.Name,
			Email:   c.Author.Email,
			Date:    c.Author.When.Format(time.RFC3339),
		})
		return ctx.Err()
	})
	if err != nil && !errors.Is(err, errStopIteration) {
		return nil, fmt.Errorf("gitreview: %w", err)
	}
	return out, nil
}

var errStopIteration = errors.New("stop iteration")
