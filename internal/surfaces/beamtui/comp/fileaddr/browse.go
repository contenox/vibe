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

// DirEntry is one child of one directory. Rel is root-relative.
type DirEntry struct {
	Name string
	Dir  bool
	Rel  string
}

// ErrNoSuchDir is returned by [Browser.Descend] for a name that is not a
// directory of the current listing, including one filtered out as noise.
var ErrNoSuchDir = errors.New("no such directory in this view")

// Directory-row glyphs, prefixed to a directory row's Detail. ASCIIDirMarker
// must stay in sync with style.Glyphs(Mono).Collapsed (checked by testkit's
// TestUnit_ASCIIGlyphParity, since this package may not import style).
const (
	DirMarker      = "▸"
	ASCIIDirMarker = ">"
)

// IsDir reports whether an item came from a directory row of
// [Browser.Entries], via the trailing slash on Label.
func IsDir(it picker.Item) bool { return strings.HasSuffix(it.Label, "/") }

// DirName is the name to hand [Browser.Descend] for a directory row, or ""
// for a file row.
func DirName(it picker.Item) string {
	if !IsDir(it) {
		return ""
	}
	return strings.TrimSuffix(it.Label, "/")
}

// Browser walks the workspace root one directory at a time, holding the
// current directory as its only state; every listing is read fresh. It
// cannot leave the root, and a selected file always yields its full
// root-relative path regardless of depth. A rootless Source or nil *Browser
// is legal and inert. Not safe for concurrent use.
type Browser struct {
	src   *Source
	cwd   string
	ascii bool
}

// NewBrowser returns a Browser positioned at the workspace root.
func NewBrowser(src *Source) *Browser { return &Browser{src: src} }

// SetASCII selects the Mono fallback for the directory marker; the default
// is the unicode set.
func (b *Browser) SetASCII(ascii bool) { b.ascii = ascii }

// Cwd returns the current directory, root-relative and slash-separated, ""
// at the root.
func (b *Browser) Cwd() string {
	if b == nil {
		return ""
	}
	return b.cwd
}

// Descend moves into a directory of the current listing. name works with or
// without a trailing slash. Anything not offered as a directory by the
// current listing returns [ErrNoSuchDir] and leaves the browser unmoved.
// Takes no context: it is a single directory read, never a walk.
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

// Ascend moves to the parent directory and reports whether it moved; it
// returns false at the root.
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
// the root, "/src/nested" below it. Too narrow to fit, it elides from the
// middle keeping whole segments ("/…/comp/picker"); the leading "/" always
// survives, and the result never exceeds width cells.
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

// clipTo is a rune-safe hard cut to width cells, with no ellipsis of its own;
// reached only when the ellipsis itself no longer fits.
func clipTo(s string, width int) string {
	if textwidth.Width(s) <= width {
		return s
	}
	return textwidth.Truncate(s, width, "")
}

// Entries returns the current directory's children as picker items, browse
// mode: directories first then files, each lexicographic ([List]'s order).
// A directory row's Label is "name/" with Detail prefixed by [DirMarker]; a
// file row's Label is the full root-relative path, Detail its parent
// directory, ID its resolved absolute path.
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

// Query returns the rows for a typed query: browse mode ([Browser.Entries])
// when q is empty, otherwise a recursive fuzzy search of the current
// directory's subtree, ranked and capped at limit (DefaultLimit when
// limit <= 0), with Labels still full root-relative paths.
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
// children, filtered exactly like [Source.Candidates]'s recursive walk,
// directories-first then lexicographic. relDir is root-relative
// ("", ".", "/" all mean the root); an absolute or escaping path, or one
// filtered as noise, returns [ErrNoSuchDir] rather than an empty listing. A
// rootless Source returns (nil, nil).
func (s *Source) List(ctx context.Context, relDir string) ([]DirEntry, error) {
	ents, err := s.list(ctx, relDir)
	if err != nil || len(ents) == 0 {
		// nil, not an empty slice: no root and an empty directory read the
		// same at the call site.
		return nil, err
	}
	out := make([]DirEntry, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.DirEntry)
	}
	return out, nil
}

// resolved is a DirEntry plus the vfs-resolved absolute path, so Entries can
// use it for Item.ID without resolving twice. List drops it.
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
	// A directory the walk would not descend into is not one this lists
	// the inside of either.
	if rel != "" && filteredDir(rel, ignore) {
		return nil, fmt.Errorf("list %q: %w", relDir, ErrNoSuchDir)
	}

	abs := s.root
	if rel != "" {
		abs = filepath.Join(s.root, filepath.FromSlash(rel))
	}
	// The containment gate the walk also applies per entry.
	real, err := s.view.Resolve(abs)
	if err != nil {
		return nil, err
	}
	if rel != "" && real != abs {
		// Traverses a symlink; the walk does not follow directory links.
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
			// A symlink etc.; only a regular-file target is listed.
			info, statErr := os.Stat(target)
			if statErr != nil || !info.Mode().IsRegular() {
				continue
			}
		}
		out = append(out, resolved{DirEntry{Name: name, Dir: false, Rel: childRel}, target})
	}

	// os.ReadDir already sorted by name, so a stable partition on Dir leaves
	// each group lexicographic, directories first.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Dir && !out[j].Dir })
	return out, nil
}

// filteredDir reports whether any segment of rel is noise, checking every
// segment (not just the last).
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

// cleanRelDir normalises a caller's relative directory to the "" / "a/b" form
// this package uses, refusing anything that leaves the root before it
// touches the filesystem.
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
