// Package fileaddr sources the candidates behind beam's @-mention picker:
// the files of the session's grant-bounded workspace root (via vfs, the only
// dependency on it under beamtui), filtered like the agent's own find_files,
// ranked and capped by comp/picker. [Source.Candidates] gives a flat ranked
// list; [Browser] walks it a directory at a time, scoped to its subtree.
package fileaddr

import (
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/contenox/beam/internal/services/vfs"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/picker"
)

const (
	// WalkBudget caps how many filesystem entries one Candidates call visits.
	WalkBudget = 5000

	// DefaultLimit is the candidate cap when the caller passes none.
	DefaultLimit = 20

	// ctxCheckStride is how often the walk polls for cancellation.
	ctxCheckStride = 64
)

// NoRootText is the fixed empty state for a session with no resolvable
// workspace root.
const NoRootText = "no workspace root — @ needs a granted directory"

// NoMatchText is the empty state when the root is fine but nothing matched.
const NoMatchText = "no matching files"

// Source enumerates @-mention candidates for one session's workspace root.
// A Source with no root (including the zero value and a nil *Source) is
// legal and inert: every query returns nothing and EmptyText explains why.
type Source struct {
	view *vfs.View // root-bound handle; nil means "no root"
	root string

	// truncated records whether the last Candidates call hit WalkBudget.
	truncated bool
}

// NewSource resolves cwd against the allowlist and returns a Source over the
// resulting directory; roots may be nil and cwd may be "" or "/". A refused
// or unresolvable cwd is not an error: it yields a rootless Source. The
// returned error is reserved for a directory the allowlist accepted that
// vfs then cannot open.
func NewSource(roots *vfs.Factory, cwd string) (*Source, error) {
	resolved, err := vfs.ResolveSessionCwd(roots, cwd, "")
	if err != nil || resolved == "" {
		return &Source{}, nil
	}
	var view *vfs.View
	if roots != nil {
		view, err = roots.Open(resolved)
	} else {
		// No allowlist: ResolveSessionCwd already accepted this absolute path.
		view, err = vfs.OpenView(resolved)
	}
	if err != nil {
		return nil, err
	}
	return &Source{view: view, root: view.Root()}, nil
}

// Root returns the resolved workspace root, or "" when there is none.
func (s *Source) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// HasRoot reports whether this Source can produce candidates at all.
func (s *Source) HasRoot() bool { return s != nil && s.view != nil }

// EmptyText is the line the picker should show when Candidates returned
// nothing.
func (s *Source) EmptyText() string {
	if !s.HasRoot() {
		return NoRootText
	}
	return NoMatchText
}

// Truncated reports whether the most recent Candidates call stopped at
// WalkBudget rather than reaching the end of the tree, so a caller can tell
// "stopped short" from "found nothing". False before the first call and for
// a rootless Source.
func (s *Source) Truncated() bool {
	return s != nil && s.truncated
}

// Candidates returns the files of the workspace root matching query, ranked
// by picker.Filter and capped at limit (DefaultLimit when limit <= 0).
// Item.Label is the root-relative slash path spliced after the "@";
// Item.Detail is the parent directory; Item.ID is the resolved absolute
// path. Directories are never candidates. A rootless Source returns
// (nil, nil); ctx cancellation aborts the walk.
func (s *Source) Candidates(ctx context.Context, query string, limit int) ([]picker.Item, error) {
	return s.candidatesIn(ctx, "", query, limit)
}

// candidatesIn is Candidates rooted at a subdirectory, with Labels still
// full root-relative paths. relDir is trusted to be root-relative and
// slash-separated ("" for the root); it is resolved through vfs before
// anything is read.
func (s *Source) candidatesIn(ctx context.Context, relDir, query string, limit int) ([]picker.Item, error) {
	if !s.HasRoot() {
		return nil, nil
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	items, truncated, err := s.walk(ctx, relDir)
	if err != nil {
		return nil, err
	}
	s.truncated = truncated
	out := picker.Filter(items, query)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// walk enumerates the files under relDir ("" for the whole root) subject to
// the noise filter and the budget, and reports whether the budget stopped
// it. Order is lexical depth-first (filepath.WalkDir's own order), kept
// deterministic since the budget makes order decide which files are
// reached. Item.Label is relative to the root, never to relDir.
func (s *Source) walk(ctx context.Context, relDir string) ([]picker.Item, bool, error) {
	ignore := gitignoreFor(s.root)

	start := s.root
	if relDir != "" {
		abs := filepath.Join(s.root, filepath.FromSlash(relDir))
		real, err := s.view.Resolve(abs)
		if err != nil {
			return nil, false, err
		}
		start = real
	}

	var items []picker.Item
	truncated := false
	visited := 0

	walkErr := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable entry is skipped, never fatal.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if visited%ctxCheckStride == 0 {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
		}

		rel, relErr := filepath.Rel(s.root, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}

		visited++
		if visited > WalkBudget {
			truncated = true
			return fs.SkipAll
		}

		if d.IsDir() {
			if skipDir(d.Name()) || ignore.Match(rel, true) {
				return fs.SkipDir
			}
			// Prune a denied subtree here rather than per file below.
			if _, resErr := s.view.Resolve(p); resErr != nil {
				return fs.SkipDir
			}
			return nil
		}

		if ignore.Match(rel, false) {
			return nil
		}

		// The containment gate: catches an in-root symlink pointing outside,
		// which WalkDir would otherwise hand over as an ordinary entry.
		real, resErr := s.view.Resolve(p)
		if resErr != nil {
			return nil
		}

		if !d.Type().IsRegular() {
			// A symlink etc.; only a regular-file target is a candidate.
			info, statErr := os.Stat(real)
			if statErr != nil || !info.Mode().IsRegular() {
				return nil
			}
		}

		items = append(items, picker.Item{
			ID:     real,
			Label:  rel,
			Detail: parentDir(rel),
		})
		return nil
	})
	if walkErr != nil {
		return nil, false, walkErr
	}
	return items, truncated, nil
}

// parentDir returns rel's directory for the picker's Detail column, or ""
// at the root.
func parentDir(rel string) string {
	d := path.Dir(rel)
	if d == "." || d == "/" {
		return ""
	}
	return d
}

// SessionItems adapts a session roster to picker items for the session
// picker; order is preserved until the user types.
func SessionItems(names []string, active string) []picker.Item {
	items := make([]picker.Item, 0, len(names))
	for _, n := range names {
		it := picker.Item{ID: n, Label: n}
		if n == active && n != "" {
			it.Detail = "active"
		}
		items = append(items, it)
	}
	return items
}
