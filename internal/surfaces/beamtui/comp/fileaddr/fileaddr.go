// Package fileaddr sources the candidates behind beam's @-mention picker: the
// files of the session's actual, grant-bounded workspace root, filtered with
// the same noise rules the agent's own find_files applies, ranked and capped
// by comp/picker.
//
// It is the SERVICE-SIDE adapter for file-addressing (blueprint 4.12) and the
// only package under beamtui that may touch vfs. The rule it exists to
// enforce: candidates come from the session's resolved workspace root via the
// vfs allowlist, NEVER from the raw process cwd, and control-plane paths and
// out-of-root symlink targets never surface. Rendering is comp/picker's job;
// splicing the chosen path back into the draft and building the wire-shape
// ContentBlocks are the composer's and file-addressing component's.
//
// # How the root is resolved
//
// Exactly the way the runtime resolves every other session cwd:
// vfs.ResolveSessionCwd, the ONE decision procedure the ACP session/new,
// session/load, session/resume and REST fleet-dispatch paths all share. A cwd
// the allowlist refuses does not become an error the user has to decode — it
// becomes a Source with no root, whose queries return nothing and whose
// EmptyText says why. That is the blueprint's "no root -> fixed empty state".
//
// # How the tree is enumerated
//
// vfs exposes containment, not enumeration: Contain/Within/View.Resolve
// answer "is this path inside this root", and there is no Walk or ReadDir in
// the package. So this walks the way the agent's own find_files walks
// (internal/services/localtools/fs.go): filepath.WalkDir rooted at the
// ALREADY-RESOLVED, allowlisted root, with every single entry run back
// through vfs.View.Resolve before it is offered. That is what makes the
// symlink and control-plane guarantees inherited rather than re-implemented —
// a link inside the root pointing outside it resolves to its real target and
// is refused by vfs, not by a check written here.
//
// # Flat list, or browsing
//
// There are two ways to reach a file and they share every rule above.
// [Source.Candidates] is the flat one: a single ranked list of the whole
// tree, the right shape when the user knows roughly what the file is called.
// [Browser] is the other — one directory level at a time over [Source.List],
// for when they do not — and typing inside it searches the subtree they are
// standing in rather than the whole repository. Whichever way a file was
// found, selecting it yields the same full root-relative path, because that
// is the text the composer splices after the "@".
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
	// WalkBudget caps how many filesystem entries one Candidates call visits,
	// independent of how many it returns. A completion list is redrawn on a
	// debounced keystroke, so an unbounded walk of a monorepo would be a hang
	// the user experiences as a frozen composer. Past the budget the walk
	// stops; because the walk order is deterministic, WHICH files were
	// reached is reproducible rather than a race.
	WalkBudget = 5000

	// DefaultLimit is the candidate cap when the caller passes none — the
	// blueprint's 20, which the picker renders with a "+N more" footer.
	DefaultLimit = 20

	// ctxCheckStride is how often the walk polls for cancellation. Per-entry
	// polling would dominate a cheap lexical walk; every 64 entries bounds the
	// overshoot at a fraction of a millisecond.
	ctxCheckStride = 64
)

// NoRootText is the fixed empty state for a session with no resolvable
// workspace root. It names the reason instead of showing an empty box, so a
// user who typed @ and got nothing knows this is a grant problem, not an
// empty directory.
const NoRootText = "no workspace root — @ needs a granted directory"

// NoMatchText is the empty state when the root is fine but nothing matched.
const NoMatchText = "no matching files"

// Source enumerates @-mention candidates for one session's workspace root.
// A Source with no root is legal and inert: every query returns nothing and
// EmptyText explains why. The zero value and a nil *Source behave the same
// way, so a caller may hold one unconditionally.
type Source struct {
	// view is the vfs-vended, root-bound handle every candidate is validated
	// through. nil means "no root".
	view *vfs.View
	root string

	// truncated records whether the last walk hit WalkBudget. It is state
	// about the most recent Candidates call, not about the Source, and like
	// everything else here it is owned by the single goroutine driving the
	// UI loop.
	truncated bool
}

// NewSource resolves cwd against the allowlist and returns a Source over the
// resulting directory.
//
// roots may be nil (the stdio/editor path, where no server-side envelope
// exists) and cwd may be "" or the "/" sentinel; vfs.ResolveSessionCwd
// applies the same rules here as on every other session-opening path, so a
// beam session and an ACP session started with identical arguments address
// identical files.
//
// A refused or unresolvable cwd is NOT an error: it yields a rootless Source
// (see the type doc). The returned error is reserved for the genuinely
// broken case — a directory the allowlist ACCEPTED that vfs then cannot open
// — which is a fault the caller should surface rather than paper over with a
// silent empty list.
func NewSource(roots *vfs.Factory, cwd string) (*Source, error) {
	resolved, err := vfs.ResolveSessionCwd(roots, cwd, "")
	if err != nil || resolved == "" {
		return &Source{}, nil
	}
	var view *vfs.View
	if roots != nil {
		view, err = roots.Open(resolved)
	} else {
		// No allowlist configured: the editor owns the filesystem and
		// ResolveSessionCwd already accepted this absolute path. OpenView is
		// vfs's constructor for exactly that case — a single fixed root,
		// trusted by construction, with containment still enforced.
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
// nothing: the no-root explanation when there is no root, the ordinary
// no-match line otherwise. Callers pass it straight to picker.SetEmptyText.
func (s *Source) EmptyText() string {
	if !s.HasRoot() {
		return NoRootText
	}
	return NoMatchText
}

// Truncated reports whether the most recent Candidates call stopped at
// WalkBudget rather than reaching the end of the tree.
//
// It exists because the budget is otherwise INVISIBLE, and invisible is the
// wrong shape for it: in a monorepo the walk stops after 5000 entries, the
// picker shows a perfectly ordinary "no matching files", and the user
// concludes their file is not there. It is — the walk simply never reached
// it. Reporting the fact lets a caller say so.
//
// Nothing in beam surfaces it yet; this is the minimum API a caller needs and
// nothing more, so the copy decision stays with whoever writes the empty
// state rather than being pre-empted here. It reflects the last call only,
// and is false before the first one and for a rootless Source.
func (s *Source) Truncated() bool {
	return s != nil && s.truncated
}

// Candidates returns the files of the workspace root matching query, ranked
// by picker.Filter and capped at limit (DefaultLimit when limit <= 0).
//
// Item.Label is the root-relative slash path — exactly the text the composer
// splices after the "@" — Item.Detail is the parent directory ("" at the
// root), and Item.ID is the resolved absolute path, which is what the
// resource_link wire shape is built from. Directories are never candidates
// (blueprint 4.12: files-only in MVP; directory mentions are "Later").
//
// A rootless Source returns (nil, nil). ctx cancellation aborts the walk and
// is returned as-is, so a superseded keystroke's walk stops rather than
// racing the next one to the screen.
func (s *Source) Candidates(ctx context.Context, query string, limit int) ([]picker.Item, error) {
	return s.candidatesIn(ctx, "", query, limit)
}

// candidatesIn is Candidates rooted at a subdirectory: the same walk, the
// same budget, the same Truncated flag, and Labels that are still FULL
// root-relative paths — a mention spliced from a subtree search addresses the
// same file as one spliced from the root, which is the property that lets
// [Browser] scope a search without changing what selecting a row means.
//
// relDir is trusted to be root-relative and slash-separated ("" for the
// root); the walk resolves it through vfs before reading anything, so a
// caller that passes something escaping gets an error rather than a listing.
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
// the noise filter and the budget, and reports whether the budget is what
// stopped it.
//
// Order is lexical depth-first: filepath.WalkDir reads each directory's
// entries sorted by name and descends immediately, which is deterministic
// across runs and platforms. That determinism is the point of documenting it
// — with a walk budget, the ORDER decides which files exist as far as the
// user is concerned, so it must not depend on directory-entry iteration luck.
//
// Item.Label is relative to the ROOT, never to relDir, whatever the walk
// started from.
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
			// An unreadable entry is skipped, never fatal: one
			// permission-denied directory must not blank the whole list.
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
			// Containment/control-plane check on the directory too, so a
			// denied subtree is pruned instead of being descended into and
			// rejected file by file.
			if _, resErr := s.view.Resolve(p); resErr != nil {
				return fs.SkipDir
			}
			return nil
		}

		if ignore.Match(rel, false) {
			return nil
		}

		// The one containment gate. vfs.View.Resolve symlink-resolves p and
		// refuses anything that escapes the root or lands in the runtime's
		// control plane — which is how an in-root symlink pointing outside
		// is caught, since WalkDir does not follow links and would otherwise
		// hand it to us as an ordinary entry.
		real, resErr := s.view.Resolve(p)
		if resErr != nil {
			return nil
		}

		if !d.Type().IsRegular() {
			// A symlink (or device, socket, fifo). Its target is already
			// known to be in-root; include it only if that target is a
			// regular file. A link to a directory is not descended into —
			// no candidate, and no way to build a symlink cycle.
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
// for a file sitting at the root (where a bare "." would be noise).
func parentDir(rel string) string {
	d := path.Dir(rel)
	if d == "." || d == "/" {
		return ""
	}
	return d
}

// SessionItems adapts a session roster to picker items for the session
// picker (blueprint 4.8 "Later: real picker overlay"). It lives here only
// because it is three lines and needs no package of its own; it touches no
// filesystem and no vfs. The roster itself comes from engine-bridge's
// ListSessions — this never fetches one.
//
// Order is preserved, which matters: picker.Filter leaves an empty query
// unsorted, so whatever recency or grouping the caller established survives
// until the user types.
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
