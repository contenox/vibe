package fileaddr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/contenox/beam/internal/surfaces/beamtui/comp/picker"
	"github.com/contenox/beam/internal/surfaces/beamtui/sanitize"
	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
)

// DirEntry is one child of one directory: a name, whether it is a directory,
// and its ROOT-relative slash path. Rel is what makes the type useful past
// the directory it came from — it is the mention text for a file and the
// argument [Browser] navigates by for a directory, and neither of them has to
// know which directory produced the entry.
type DirEntry struct {
	Name string
	Dir  bool
	Rel  string
}

// ErrNoSuchDir is what [Browser.Descend] returns for a name that is not a
// directory of the current listing — including one that exists on disk but is
// filtered out as noise, which is the case worth being precise about: if the
// browser cannot SHOW you node_modules it must not walk you into it either.
var ErrNoSuchDir = errors.New("no such directory in this view")

// Directory-row glyphs. The marker is the collapsed/disclosure device — the
// same role transcript's ASCIIUser plays — and it prefixes a directory row's
// Detail, which is how a caller (or a reader of the screen) tells a row Enter
// OPENS from a row Enter INSERTS.
//
// Components may not import beamtui/style (blueprint rule c), so this package
// spells its own ASCII fallback out and it has to agree with
// style.Glyphs(Mono).Collapsed by hand. testkit's TestUnit_ASCIIGlyphParity
// is the inventory that holds those pairs together; ASCIIDirMarker belongs in
// its "collapsed" row and should be added there the next time testkit is
// touched (this package cannot edit it without owning it).
const (
	DirMarker = "▸"
	// ASCIIDirMarker == style.Glyphs(Mono).Collapsed == transcript.ASCIIUser.
	ASCIIDirMarker = ">"
)

// IsDir reports whether an item came from a directory row of
// [Browser.Entries]. It is the caller's Enter-key branch: a directory row
// descends, a file row splices.
//
// The test is the trailing slash on Label, which is the same thing the row
// SHOWS — no path on any supported filesystem ends in a separator, so the
// display convention and the predicate cannot drift apart.
func IsDir(it picker.Item) bool { return strings.HasSuffix(it.Label, "/") }

// DirName is the name to hand [Browser.Descend] for a directory row, or ""
// for a file row.
func DirName(it picker.Item) string {
	if !IsDir(it) {
		return ""
	}
	return strings.TrimSuffix(it.Label, "/")
}

// Browser walks the workspace root one directory at a time.
//
// It is the navigation half of file-addressing: [Source] answers "which files
// are in this tree" and Browser answers "which files are in THIS directory,
// and how do I get to the next one". It holds exactly one piece of state —
// the current directory, root-relative — and every listing it produces is
// read fresh, so a tree that changed under the user is simply the tree they
// see on the next keystroke.
//
// Two rules make it safe to hand to a keyboard:
//
//   - It cannot leave the root. [Browser.Ascend] returns false at the top
//     instead of walking into the parent, and [Browser.Descend] only accepts a
//     name the current listing actually offered — so a directory the noise
//     filter hides, or one whose vfs resolution escapes the root, is not
//     merely invisible but unreachable.
//   - Selecting a file yields its FULL root-relative path however deep the
//     browsing went, because that is what gets spliced after the "@". The
//     directory you happen to be standing in changes what you SEE, never what
//     a selection MEANS.
//
// # Intended key mapping
//
// The intended app-shell wiring reads:
//
//   - Enter on a row where [IsDir] is true: Descend(DirName(item)), then
//     refresh the picker from Entries and its header from Breadcrumb. The
//     query is cleared, because a query is scoped to the directory it was
//     typed in.
//   - Enter on a file row: splice item.Label — unchanged from today's flat
//     picker, which is why the Label of a file row is still the full path.
//   - Backspace with an EMPTY query: Ascend(); when it returns false the
//     browser is at the root and the key falls through to the composer, which
//     deletes back over the "@" and closes the overlay. With a non-empty
//     query Backspace edits the query as usual — navigation never eats a
//     keystroke the user meant for their text.
//   - Any typing: Query(ctx, q, limit). An empty q is browse mode (this
//     directory's entries); anything else is a recursive fuzzy search of this
//     directory's subtree.
//   - Esc: close the overlay. Re-opening builds a fresh Browser, so "@" always
//     starts at the workspace root rather than wherever the last mention was
//     hunted down.
//
// A Browser over a rootless Source is legal and inert, like the Source
// itself: no entries, no navigation, and a breadcrumb of "/". So is a nil
// *Browser, so a caller may hold one unconditionally.
//
// It is not safe for concurrent use — like every other component here it is
// owned by the single goroutine driving the UI loop.
type Browser struct {
	src   *Source
	cwd   string
	ascii bool
}

// NewBrowser returns a Browser positioned at the workspace root.
func NewBrowser(src *Source) *Browser { return &Browser{src: src} }

// SetASCII selects the Mono fallback for the directory marker. Callers pass
// the same ascii they pass to picker.Render and Breadcrumb; the default is
// the unicode set, matching every other component's baseline.
func (b *Browser) SetASCII(ascii bool) { b.ascii = ascii }

// Cwd returns the current directory, root-relative and slash-separated. It is
// "" at the root — not "." and not "/" — so it composes with path.Join and
// with Item.Label without a special case.
func (b *Browser) Cwd() string {
	if b == nil {
		return ""
	}
	return b.cwd
}

// Descend moves into a directory of the CURRENT listing. name is a bare entry
// name, with or without the trailing slash a directory row's Label carries,
// so both Descend(DirName(item)) and Descend(item.Label) work.
//
// Anything the current listing does not offer as a directory — a file, a
// filtered-out name, a path with a separator in it, ".." — returns
// [ErrNoSuchDir] and leaves the browser where it was. That check is a real
// listing, not a string test: it is what makes "you can only go where you can
// see" true rather than aspirational.
//
// It takes no context because it is a single directory read on a keystroke,
// never a walk.
func (b *Browser) Descend(name string) error {
	name = strings.TrimSuffix(name, "/")
	if b == nil || name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("descend %q: %w", name, ErrNoSuchDir)
	}
	ents, err := b.src.List(context.Background(), b.cwd)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.Dir && e.Name == name {
			b.cwd = e.Rel
			return nil
		}
	}
	return fmt.Errorf("descend %q: %w", name, ErrNoSuchDir)
}

// Ascend moves to the parent directory and reports whether it moved. It
// returns false at the root, which is the boundary: there is no parent of the
// workspace root as far as this package is concerned, and the caller's key
// handler uses the false to let the key mean something else (see the key
// mapping above).
func (b *Browser) Ascend() bool {
	if b == nil || b.cwd == "" {
		return false
	}
	parent := path.Dir(b.cwd)
	if parent == "." || parent == "/" {
		parent = ""
	}
	b.cwd = parent
	return true
}

// Breadcrumb renders the current directory for the picker's header: "/" at
// the root, "/src/nested" below it.
//
// Too narrow to fit, it elides from the MIDDLE and keeps whole segments off
// the end — "/…/comp/picker" — because the deepest segments are the ones that
// say where you are and half a segment name says nothing at all. The leading
// "/" survives every truncation, so a truncated crumb is never mistaken for a
// relative one. When not even one whole segment fits beside the ellipsis the
// result is a rune-safe cut of what remains, and it never exceeds width
// cells in either glyph mode.
func (b *Browser) Breadcrumb(width int, ascii bool) string {
	if width <= 0 {
		return ""
	}
	cwd := ""
	if b != nil {
		cwd = sanitize.Line(b.cwd)
	}
	if cwd == "" {
		return clipTo("/", width)
	}
	full := "/" + cwd
	if textwidth.Width(full) <= width {
		return full
	}

	ell := "…"
	if ascii {
		ell = "..."
	}
	segs := strings.Split(cwd, "/")
	for keep := len(segs) - 1; keep >= 1; keep-- {
		s := "/" + ell + "/" + strings.Join(segs[len(segs)-keep:], "/")
		if textwidth.Width(s) <= width {
			return s
		}
	}
	return clipTo("/"+ell+"/"+segs[len(segs)-1], width)
}

// clipTo is a rune-safe hard cut to width cells. It carries no ellipsis of
// its own: it is only reached when the ellipsis itself no longer fits, where
// adding one would spend the last cells announcing a truncation instead of
// showing the name.
func clipTo(s string, width int) string {
	if textwidth.Width(s) <= width {
		return s
	}
	return textwidth.Truncate(s, width, "")
}

// Entries returns the current directory's children as picker items: browse
// mode.
//
// Directories come first, then files, each lexicographic — the order [List]
// produces, kept because a browser whose rows move between keystrokes cannot
// be navigated by muscle memory.
//
// A directory row's Label is "name/" and its Detail begins with the collapsed
// marker (see [DirMarker]); a file row is exactly what the flat picker
// produced — Label is the FULL root-relative path, Detail its parent
// directory, ID its resolved absolute path. That asymmetry is deliberate: the
// Label of a file row is the text spliced after the "@", so it has to stay
// addressable from wherever the user is standing, while a directory row is
// never spliced at all and reads better as the bare name it is.
func (b *Browser) Entries(ctx context.Context) ([]picker.Item, error) {
	if b == nil {
		return nil, nil
	}
	ents, err := b.src.list(ctx, b.cwd)
	if err != nil {
		return nil, err
	}
	marker := DirMarker
	if b.ascii {
		marker = ASCIIDirMarker
	}
	items := make([]picker.Item, 0, len(ents))
	for _, e := range ents {
		if e.Dir {
			items = append(items, picker.Item{
				ID:     e.abs,
				Label:  e.Name + "/",
				Detail: marker,
			})
			continue
		}
		items = append(items, picker.Item{
			ID:     e.abs,
			Label:  e.Rel,
			Detail: parentDir(e.Rel),
		})
	}
	return items, nil
}

// Query returns the rows for a typed query: browse mode when q is empty
// (identical to [Browser.Entries]), and otherwise a RECURSIVE fuzzy search of
// the current directory's subtree, ranked by picker.Filter and capped at
// limit (DefaultLimit when limit <= 0).
//
// Scoping is the whole point of searching from a Browser rather than from
// [Source.Candidates]: a match outside the current directory never appears,
// so descending is a way to narrow a search and not merely a way to look
// around. The results are files only — a search returns things you can
// mention, and the walk that produces them is the same one, with the same
// budget and the same [Source.Truncated] reporting, rooted at Cwd instead of
// at the workspace root.
//
// Labels remain FULL root-relative paths, so selecting a deep result splices
// a path that resolves from the root however the user got to it.
func (b *Browser) Query(ctx context.Context, q string, limit int) ([]picker.Item, error) {
	if b == nil {
		return nil, nil
	}
	if q == "" {
		return b.Entries(ctx)
	}
	return b.src.candidatesIn(ctx, b.cwd, q, limit)
}

// List returns one directory level of the workspace root: relDir's immediate
// children, filtered exactly the way [Source.Candidates]' recursive walk
// filters, and ordered directories-first then lexicographically.
//
// relDir is root-relative and slash-separated; "" and "." mean the root, as
// does the bare "/" sentinel the runtime's own cwd resolution accepts. Any
// other leading slash is refused as an absolute path, and so is anything that
// climbs out with "..", or that vfs itself declines — an error rather than an
// empty listing, because a silent empty directory is how a containment bug
// hides.
//
// "Exactly the way the walk filters" is the contract that matters: a skip-dir
// name (.git, node_modules, vendor …) never appears, a .gitignore'd entry
// never appears, and a symlink is resolved through the view and dropped when
// its target leaves the root. A symlink to a DIRECTORY is dropped outright,
// matching the walk, which does not follow one either — listing it would
// offer a subtree the recursive search half cannot see, and it is how a
// browser gets a cycle.
//
// The filter binds relDir too, not only the entries: a directory the listing
// would hide is one this refuses to list the inside of, so ".git",
// "node_modules", a gitignored directory, and any path reached through a
// symlinked directory all return [ErrNoSuchDir] however the caller spells
// them. A filter that stopped at the entries would hide those directories
// from the eye and leave them one constructed path away.
//
// A rootless Source returns (nil, nil), like every other query on one.
func (s *Source) List(ctx context.Context, relDir string) ([]DirEntry, error) {
	ents, err := s.list(ctx, relDir)
	if err != nil || len(ents) == 0 {
		// nil rather than an empty slice, so "no root", "empty directory" and
		// "everything here was noise" read the same at the call site.
		return nil, err
	}
	out := make([]DirEntry, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.DirEntry)
	}
	return out, nil
}

// resolved is a DirEntry plus the vfs-resolved absolute path the containment
// check already produced. Entries needs it for Item.ID and would otherwise
// resolve every entry a second time; List drops it, because an absolute path
// is not something a listing's caller should be handed by default.
type resolved struct {
	DirEntry
	abs string
}

func (s *Source) list(ctx context.Context, relDir string) ([]resolved, error) {
	if !s.HasRoot() {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rel, err := cleanRelDir(relDir)
	if err != nil {
		return nil, err
	}

	ignore := gitignoreFor(s.root)
	// A directory the walk would not descend into is not a directory this
	// lists the inside of either. Without this the filter would be true of
	// the ENTRIES and false of the argument — List(".git") would happily
	// enumerate a directory the browser is built to hide, reachable by any
	// caller that constructs a path instead of navigating to one.
	if rel != "" && filteredDir(rel, ignore) {
		return nil, fmt.Errorf("list %q: %w", relDir, ErrNoSuchDir)
	}

	abs := s.root
	if rel != "" {
		abs = filepath.Join(s.root, filepath.FromSlash(rel))
	}
	// The one containment gate, the same call the walk makes per entry: it
	// symlink-resolves the directory and refuses anything outside the root or
	// inside the runtime's control plane.
	real, err := s.view.Resolve(abs)
	if err != nil {
		return nil, err
	}
	if rel != "" && real != abs {
		// The path traverses a symlink. The root is symlink-resolved by
		// construction, so this can only be the caller's tail — and since the
		// recursive walk does not follow directory links, listing through one
		// would offer a subtree under paths the search half never produces.
		return nil, fmt.Errorf("list %q: %w", relDir, ErrNoSuchDir)
	}
	des, err := os.ReadDir(real)
	if err != nil {
		return nil, err
	}

	out := make([]resolved, 0, len(des))
	for _, d := range des {
		name := d.Name()
		childRel := name
		if rel != "" {
			childRel = rel + "/" + name
		}
		target, resErr := s.view.Resolve(filepath.Join(real, name))
		if resErr != nil {
			continue
		}

		if d.IsDir() {
			if skipDir(name) || ignore.Match(childRel, true) {
				continue
			}
			out = append(out, resolved{DirEntry{Name: name, Dir: true, Rel: childRel}, target})
			continue
		}
		if ignore.Match(childRel, false) {
			continue
		}
		if !d.Type().IsRegular() {
			// A symlink (or device, socket, fifo). Its target is already known
			// to be in-root; take it only if that target is a regular file, so
			// a link to a directory is neither listed nor descended into.
			info, statErr := os.Stat(target)
			if statErr != nil || !info.Mode().IsRegular() {
				continue
			}
		}
		out = append(out, resolved{DirEntry{Name: name, Dir: false, Rel: childRel}, target})
	}

	// os.ReadDir already sorted by name, so a STABLE partition on Dir leaves
	// each group lexicographic. Directories first because that is the axis the
	// user is navigating: the rows that go somewhere sit above the rows that
	// end the interaction.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Dir && !out[j].Dir })
	return out, nil
}

// filteredDir reports whether any segment of rel is noise: a skip-listed
// directory name, or one a .gitignore rule excludes. Every segment is checked,
// not just the last, because "src/node_modules/dep" is as unreachable to the
// walk as "node_modules" is.
func filteredDir(rel string, ignore *ignoreMatcher) bool {
	prefix := ""
	for _, seg := range strings.Split(rel, "/") {
		if prefix == "" {
			prefix = seg
		} else {
			prefix += "/" + seg
		}
		if skipDir(seg) || ignore.Match(prefix, true) {
			return true
		}
	}
	return false
}

// cleanRelDir normalises a caller's relative directory to the "" / "a/b"
// form the rest of this package uses, and refuses anything that leaves the
// root. The refusal is belt-and-braces — vfs.View.Resolve would catch an
// escape too — but a "../.." that never reaches the filesystem is one that
// cannot depend on the filesystem's opinion of it.
func cleanRelDir(relDir string) (string, error) {
	r := strings.TrimPrefix(filepath.ToSlash(relDir), "./")
	r = strings.Trim(r, "/")
	if r == "" || r == "." {
		return "", nil
	}
	if filepath.IsAbs(relDir) || strings.HasPrefix(relDir, "/") {
		return "", fmt.Errorf("list %q: %w", relDir, errAbsDir)
	}
	clean := path.Clean(r)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("list %q: %w", relDir, errEscapes)
	}
	return clean, nil
}

var (
	errAbsDir  = errors.New("directory must be root-relative")
	errEscapes = errors.New("directory escapes the workspace root")
)
