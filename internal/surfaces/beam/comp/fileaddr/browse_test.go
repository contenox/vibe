package fileaddr_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/surfaces/beam/comp/fileaddr"
	"github.com/contenox/contenox/internal/surfaces/beam/comp/picker"
	"github.com/contenox/contenox/internal/surfaces/beam/textwidth"
)

// buildBrowseWorkspace lays out a tree with real depth for navigation tests:
// two levels of directories, noise at every level, both flavours of escaping
// symlink. Returns the root.
func buildBrowseWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	outside := t.TempDir()

	writeFile(t, root, ".gitignore", strings.Join([]string{
		"*.log",
		"generated/",
		"src/ignored-here.go",
	}, "\n")+"\n")

	writeFile(t, root, "README.md", "# hi")
	writeFile(t, root, "main.go", "package main")
	writeFile(t, root, "src/app.go", "package src")
	writeFile(t, root, "src/util.go", "package src")
	writeFile(t, root, "src/ignored-here.go", "gitignored by path")
	writeFile(t, root, "src/trace.log", "gitignored by glob")
	writeFile(t, root, "src/nested/deep.go", "package nested")
	writeFile(t, root, "src/nested/deeper/bottom.go", "package deeper")
	writeFile(t, root, "docs/guide.md", "guide")

	writeFile(t, root, "node_modules/pkg/index.js", "noise")
	writeFile(t, root, ".git/config", "[core]")
	writeFile(t, root, "generated/gen.go", "noise")
	writeFile(t, root, "src/node_modules/dep/dep.js", "noise at depth")

	// In-root regular-file symlink: legitimate.
	if err := os.Symlink(filepath.Join(root, "main.go"), filepath.Join(root, "link-main.go")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	// Escaping-file and escaping-directory symlinks: neither listed nor enterable.
	writeFile(t, outside, "outside-secret.txt", "off limits")
	if err := os.Symlink(filepath.Join(outside, "outside-secret.txt"), filepath.Join(root, "escape.txt")); err != nil {
		t.Fatalf("symlink escape.txt: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape-dir")); err != nil {
		t.Fatalf("symlink escape-dir: %v", err)
	}
	// In-root directory symlink: the walk does not follow it.
	if err := os.Symlink(filepath.Join(root, "src"), filepath.Join(root, "src-link")); err != nil {
		t.Fatalf("symlink src-link: %v", err)
	}
	return root
}

// entryNames renders a listing as "name" / "name/" for compact comparison.
func entryNames(ents []fileaddr.DirEntry) []string {
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.Dir {
			out = append(out, e.Name+"/")
			continue
		}
		out = append(out, e.Name)
	}
	return out
}

func TestUnit_FileAddrList_OneLevel(t *testing.T) {
	root := buildBrowseWorkspace(t)
	s := newSource(t, root)

	cases := []struct {
		name   string
		relDir string
		want   []string
	}{
		{
			name:   "root",
			relDir: "",
			want: []string{
				"docs/", "src/",
				".gitignore", "README.md", "link-main.go", "main.go",
			},
		},
		{
			name:   "the root by its sentinel spellings",
			relDir: ".",
			want: []string{
				"docs/", "src/",
				".gitignore", "README.md", "link-main.go", "main.go",
			},
		},
		{
			name:   "a subdirectory keeps filtering",
			relDir: "src",
			want:   []string{"nested/", "app.go", "util.go"},
		},
		{
			name:   "a trailing slash is the same directory",
			relDir: "src/",
			want:   []string{"nested/", "app.go", "util.go"},
		},
		{
			name:   "two levels down",
			relDir: "src/nested",
			want:   []string{"deeper/", "deep.go"},
		},
		{
			name:   "the bottom of the tree",
			relDir: "src/nested/deeper",
			want:   []string{"bottom.go"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ents, err := s.List(context.Background(), tc.relDir)
			if err != nil {
				t.Fatalf("List(%q): %v", tc.relDir, err)
			}
			got := entryNames(ents)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("List(%q) =\n %v\nwant\n %v", tc.relDir, got, tc.want)
			}
			// Rel is root-relative, whatever level it came from.
			for _, e := range ents {
				want := e.Name
				if tc.relDir != "" && tc.relDir != "." {
					want = strings.Trim(filepath.ToSlash(tc.relDir), "/") + "/" + e.Name
				}
				if e.Rel != want {
					t.Errorf("entry %q has Rel %q, want %q", e.Name, e.Rel, want)
				}
			}
		})
	}
}

func TestUnit_FileAddrList_RefusesEscapesAndMissingDirs(t *testing.T) {
	root := buildBrowseWorkspace(t)
	s := newSource(t, root)

	// The sentinel spellings of the root are the one thing here that works.
	if _, err := s.List(context.Background(), "/"); err != nil {
		t.Errorf("List(\"/\") = %v, want the root listing", err)
	}

	cases := []struct {
		rel      string
		why      string
		filtered bool // refused as noise, i.e. errors.Is(err, ErrNoSuchDir)
	}{
		{rel: "..", why: "the parent of the root"},
		{rel: "../..", why: "two levels out"},
		{rel: "src/../..", why: "an escape that cleans to one"},
		{rel: "/etc", why: "an absolute path"},
		{rel: "no-such-dir", why: "a directory that is not there"},
		{rel: "main.go", why: "a file"},
		{rel: "escape-dir", why: "a symlink out of the root"},
		{rel: ".git", why: "a skip-listed directory", filtered: true},
		{rel: "node_modules", why: "a skip-listed directory", filtered: true},
		{rel: "node_modules/pkg", why: "a path under a skip-listed directory", filtered: true},
		{rel: "src/node_modules", why: "a skip-listed directory at depth", filtered: true},
		{rel: "generated", why: "a gitignored directory", filtered: true},
		{rel: "src-link", why: "a symlink to an in-root directory", filtered: true},
		{rel: "src-link/nested", why: "a path through a symlinked directory", filtered: true},
	}
	for _, tc := range cases {
		ents, err := s.List(context.Background(), tc.rel)
		if err == nil {
			t.Errorf("List(%q) returned %v, want an error (%s)", tc.rel, entryNames(ents), tc.why)
			continue
		}
		if tc.filtered && !errors.Is(err, fileaddr.ErrNoSuchDir) {
			t.Errorf("List(%q) = %v, want ErrNoSuchDir (%s)", tc.rel, err, tc.why)
		}
	}
}

func TestUnit_FileAddrList_RootlessSourceIsInert(t *testing.T) {
	s, err := fileaddr.NewSource(nil, "")
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	ents, err := s.List(context.Background(), "")
	if err != nil || len(ents) != 0 {
		t.Fatalf("List on a rootless Source = (%v, %v), want (nil, nil)", ents, err)
	}
	if ents, err := (*fileaddr.Source)(nil).List(context.Background(), "src"); err != nil || ents != nil {
		t.Fatalf("List on a nil Source = (%v, %v), want (nil, nil)", ents, err)
	}

	b := fileaddr.NewBrowser(s)
	if got, err := b.Entries(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("Entries on a rootless Browser = (%v, %v)", got, err)
	}
	if b.Ascend() {
		t.Fatal("a rootless Browser ascended")
	}
	if err := b.Descend("src"); !errors.Is(err, fileaddr.ErrNoSuchDir) {
		t.Fatalf("Descend on a rootless Browser = %v, want ErrNoSuchDir", err)
	}
	if got := b.Breadcrumb(20, false); got != "/" {
		t.Fatalf("rootless Breadcrumb = %q, want %q", got, "/")
	}

	var nilB *fileaddr.Browser
	if nilB.Cwd() != "" || nilB.Ascend() || nilB.Breadcrumb(20, false) != "/" {
		t.Fatal("a nil Browser is not inert")
	}
	if err := nilB.Descend("src"); !errors.Is(err, fileaddr.ErrNoSuchDir) {
		t.Fatalf("Descend on a nil Browser = %v, want ErrNoSuchDir", err)
	}
	if got, err := nilB.Entries(context.Background()); err != nil || got != nil {
		t.Fatalf("Entries on a nil Browser = (%v, %v)", got, err)
	}
	if got, err := nilB.Query(context.Background(), "go", 10); err != nil || got != nil {
		t.Fatalf("Query on a nil Browser = (%v, %v)", got, err)
	}
}

func TestUnit_FileAddrList_CancelledContext(t *testing.T) {
	root := buildBrowseWorkspace(t)
	s := newSource(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.List(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("List with a cancelled context = %v, want context.Canceled", err)
	}
}

func TestUnit_FileAddrBrowser_Navigation(t *testing.T) {
	root := buildBrowseWorkspace(t)
	b := fileaddr.NewBrowser(newSource(t, root))

	if b.Cwd() != "" {
		t.Fatalf("a new Browser starts at %q, want the root \"\"", b.Cwd())
	}
	if b.Ascend() {
		t.Fatal("Ascend at the root returned true — the root is the boundary")
	}
	if b.Cwd() != "" {
		t.Fatalf("a refused Ascend moved to %q", b.Cwd())
	}

	steps := []struct {
		name string
		cwd  string
	}{
		{"src", "src"},
		{"nested", "src/nested"},
		{"deeper", "src/nested/deeper"},
	}
	for _, st := range steps {
		if err := b.Descend(st.name); err != nil {
			t.Fatalf("Descend(%q): %v", st.name, err)
		}
		if b.Cwd() != st.cwd {
			t.Fatalf("after Descend(%q) Cwd = %q, want %q", st.name, b.Cwd(), st.cwd)
		}
	}
	for _, want := range []string{"src/nested", "src", ""} {
		if !b.Ascend() {
			t.Fatalf("Ascend from %q returned false", b.Cwd())
		}
		if b.Cwd() != want {
			t.Fatalf("Ascend landed at %q, want %q", b.Cwd(), want)
		}
	}
	if b.Ascend() {
		t.Fatal("Ascend walked out of the root")
	}

	if err := b.Descend("src/"); err != nil {
		t.Fatalf("Descend(\"src/\"): %v", err)
	}
	if b.Cwd() != "src" {
		t.Fatalf("Cwd = %q, want src", b.Cwd())
	}
	if err := b.Descend("nested"); err != nil {
		t.Fatalf("Descend(nested): %v", err)
	}
	b.Ascend()
	b.Ascend()

	refusals := []struct {
		name string
		why  string
	}{
		{"no-such-dir", "a name that is not there"},
		{"main.go", "a file"},
		{"..", "the parent"},
		{".", "the current directory"},
		{"", "nothing"},
		{"src/nested", "a multi-segment path — Descend is one level"},
		{"node_modules", "a skip-listed directory the listing hides"},
		{"generated", "a gitignored directory the listing hides"},
		{"escape-dir", "a symlink out of the root"},
		{"src-link", "a symlink to a directory, which the walk cannot follow"},
	}
	for _, tc := range refusals {
		t.Run("refuses "+tc.why, func(t *testing.T) {
			err := b.Descend(tc.name)
			if !errors.Is(err, fileaddr.ErrNoSuchDir) {
				t.Fatalf("Descend(%q) = %v, want ErrNoSuchDir", tc.name, err)
			}
			if b.Cwd() != "" {
				t.Fatalf("a refused Descend moved to %q", b.Cwd())
			}
		})
	}
}

func TestUnit_FileAddrBrowser_Breadcrumb(t *testing.T) {
	root := buildBrowseWorkspace(t)
	b := fileaddr.NewBrowser(newSource(t, root))

	if got := b.Breadcrumb(80, false); got != "/" {
		t.Fatalf("root Breadcrumb = %q, want %q", got, "/")
	}

	for _, name := range []string{"src", "nested", "deeper"} {
		if err := b.Descend(name); err != nil {
			t.Fatalf("Descend(%q): %v", name, err)
		}
	}
	if b.Cwd() != "src/nested/deeper" {
		t.Fatalf("Cwd = %q", b.Cwd())
	}

	cases := []struct {
		width int
		ascii bool
		want  string
	}{
		{80, false, "/src/nested/deeper"},
		{40, false, "/src/nested/deeper"},
		{20, false, "/src/nested/deeper"},
		{18, false, "/src/nested/deeper"},
		{17, false, "/…/nested/deeper"},
		{15, false, "/…/deeper"},
		{17, true, "/.../deeper"},
		{9, false, "/…/deeper"},
		{8, false, "/…/deepe"}, // below the last whole segment: a rune-safe cut
		{1, false, "/"},
		{0, false, ""},
	}
	for _, tc := range cases {
		got := b.Breadcrumb(tc.width, tc.ascii)
		if got != tc.want {
			t.Errorf("Breadcrumb(%d, ascii=%v) = %q, want %q", tc.width, tc.ascii, got, tc.want)
		}
	}

	for _, ascii := range []bool{false, true} {
		for w := 0; w <= 60; w++ {
			got := b.Breadcrumb(w, ascii)
			if n := textwidth.Width(got); n > w {
				t.Fatalf("Breadcrumb(%d, ascii=%v) = %q, %d cells", w, ascii, got, n)
			}
			if w > 0 && !strings.HasPrefix(got, "/") {
				t.Fatalf("Breadcrumb(%d, ascii=%v) = %q, want a leading /", w, ascii, got)
			}
		}
	}
}

// TestUnit_FileAddrBrowser_BreadcrumbAtGoldenWidths pins the resize matrix
// against a path deep enough to need every branch.
func TestUnit_FileAddrBrowser_BreadcrumbAtGoldenWidths(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "internal/surfaces/beamtui/comp/fileaddr/keep.go", "package fileaddr")
	b := fileaddr.NewBrowser(newSource(t, root))
	for _, name := range []string{"internal", "surfaces", "beamtui", "comp", "fileaddr"} {
		if err := b.Descend(name); err != nil {
			t.Fatalf("Descend(%q): %v", name, err)
		}
	}
	const full = "/internal/surfaces/beamtui/comp/fileaddr"

	cases := []struct {
		width int
		ascii bool
		want  string
	}{
		{80, false, full},
		{40, false, full},
		{20, false, "/…/comp/fileaddr"},
		{20, true, "/.../comp/fileaddr"},
		{80, true, full},
		{40, true, full},
	}
	for _, tc := range cases {
		if got := b.Breadcrumb(tc.width, tc.ascii); got != tc.want {
			t.Errorf("Breadcrumb(%d, ascii=%v) = %q, want %q", tc.width, tc.ascii, got, tc.want)
		}
	}
}

func TestUnit_FileAddrBrowser_EntriesRowShapes(t *testing.T) {
	root := buildBrowseWorkspace(t)
	b := fileaddr.NewBrowser(newSource(t, root))

	items, err := b.Entries(context.Background())
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	got := labelsOf(items)
	want := []string{"docs/", "src/", ".gitignore", "README.md", "link-main.go", "main.go"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("root Entries =\n %v\nwant\n %v", got, want)
	}

	for _, it := range items {
		if fileaddr.IsDir(it) {
			if !strings.HasPrefix(it.Detail, fileaddr.DirMarker) {
				t.Errorf("directory row %q has Detail %q, want the collapsed marker %q as its prefix",
					it.Label, it.Detail, fileaddr.DirMarker)
			}
			if fileaddr.DirName(it) != strings.TrimSuffix(it.Label, "/") {
				t.Errorf("DirName(%q) = %q", it.Label, fileaddr.DirName(it))
			}
			probe := fileaddr.NewBrowser(newSource(t, root))
			if err := probe.Descend(fileaddr.DirName(it)); err != nil {
				t.Errorf("Descend(DirName(%q)): %v", it.Label, err)
			}
			continue
		}
		if fileaddr.DirName(it) != "" {
			t.Errorf("DirName on the file row %q = %q, want empty", it.Label, fileaddr.DirName(it))
		}
		if !filepath.IsAbs(it.ID) {
			t.Errorf("file row %q has ID %q, want an absolute path", it.Label, it.ID)
		}
	}

	b.SetASCII(true)
	asciiItems, err := b.Entries(context.Background())
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	for _, it := range asciiItems {
		if !fileaddr.IsDir(it) {
			continue
		}
		if !strings.HasPrefix(it.Detail, fileaddr.ASCIIDirMarker) {
			t.Errorf("ascii directory row %q has Detail %q, want the %q prefix",
				it.Label, it.Detail, fileaddr.ASCIIDirMarker)
		}
		for _, r := range it.Detail {
			if r > 0x7f {
				t.Errorf("ascii directory row Detail %q carries %U", it.Detail, r)
			}
		}
	}
}

// TestUnit_FileAddrBrowser_FileRowsSpliceTheFullPath: browsing changes what
// you see, never what a selection means — a file chosen three directories
// down still splices the path that resolves from the workspace root.
func TestUnit_FileAddrBrowser_FileRowsSpliceTheFullPath(t *testing.T) {
	root := buildBrowseWorkspace(t)
	b := fileaddr.NewBrowser(newSource(t, root))
	for _, name := range []string{"src", "nested", "deeper"} {
		if err := b.Descend(name); err != nil {
			t.Fatalf("Descend(%q): %v", name, err)
		}
	}

	items, err := b.Entries(context.Background())
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("deepest directory has %v, want one file", labelsOf(items))
	}
	it := items[0]
	if it.Label != "src/nested/deeper/bottom.go" {
		t.Fatalf("mention text = %q, want the full root-relative path", it.Label)
	}
	if it.Detail != "src/nested/deeper" {
		t.Fatalf("Detail = %q, want the parent directory", it.Detail)
	}
	if want := filepath.Join(root, "src", "nested", "deeper", "bottom.go"); it.ID != want {
		t.Fatalf("ID = %q, want %q", it.ID, want)
	}

	found, err := b.Query(context.Background(), "bottom", 20)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(found) == 0 || found[0].Label != it.Label {
		t.Fatalf("Query(bottom) = %v, want %q first", labelsOf(found), it.Label)
	}
	if found[0].ID != it.ID {
		t.Fatalf("browse ID %q and search ID %q disagree", it.ID, found[0].ID)
	}
}

func TestUnit_FileAddrBrowser_QueryIsScopedToTheSubtree(t *testing.T) {
	root := buildBrowseWorkspace(t)
	b := fileaddr.NewBrowser(newSource(t, root))

	all, err := b.Query(context.Background(), "go", 100)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !contains(labelsOf(all), "main.go") || !contains(labelsOf(all), "src/nested/deeper/bottom.go") {
		t.Fatalf("a root search missed files: %v", labelsOf(all))
	}

	if err := b.Descend("src"); err != nil {
		t.Fatalf("Descend(src): %v", err)
	}
	scoped, err := b.Query(context.Background(), "go", 100)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	labels := labelsOf(scoped)
	for _, want := range []string{"src/app.go", "src/util.go", "src/nested/deep.go", "src/nested/deeper/bottom.go"} {
		if !contains(labels, want) {
			t.Errorf("scoped search missed %q: %v", want, labels)
		}
	}
	for _, forbidden := range []string{"main.go", "link-main.go", "docs/guide.md"} {
		if contains(labels, forbidden) {
			t.Errorf("scoped search returned %q, which is outside %q", forbidden, b.Cwd())
		}
	}
	// Scoping does not suspend the noise filter.
	for _, forbidden := range []string{"src/trace.log", "src/ignored-here.go", "src/node_modules/dep/dep.js"} {
		if contains(labels, forbidden) {
			t.Errorf("scoped search returned filtered path %q", forbidden)
		}
	}

	if err := b.Descend("nested"); err != nil {
		t.Fatalf("Descend(nested): %v", err)
	}
	deep, err := b.Query(context.Background(), "go", 100)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	for _, l := range labelsOf(deep) {
		if !strings.HasPrefix(l, "src/nested/") {
			t.Errorf("search under src/nested returned %q", l)
		}
	}

	browse, err := b.Query(context.Background(), "", 100)
	if err != nil {
		t.Fatalf("Query(\"\"): %v", err)
	}
	if got, want := labelsOf(browse), []string{"deeper/", "src/nested/deep.go"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("browse mode = %v, want %v", got, want)
	}

	// The limit is honoured and a non-positive one means the default.
	if got, err := b.Query(context.Background(), "go", 1); err != nil || len(got) != 1 {
		t.Fatalf("Query with limit 1 = (%v, %v)", labelsOf(got), err)
	}
	if got, err := b.Query(context.Background(), "go", 0); err != nil || len(got) > fileaddr.DefaultLimit {
		t.Fatalf("Query with limit 0 returned %d, want <= %d", len(got), fileaddr.DefaultLimit)
	}
}

// TestUnit_FileAddrBrowser_QueryRanksWithTheFuzzyScorer: a scoped search is
// still ranked by picker.Filter, so the browser inherits the fuzzy tier
// rather than re-implementing an order of its own.
func TestUnit_FileAddrBrowser_QueryRanksWithTheFuzzyScorer(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "app/keybindings.go", "package app")
	writeFile(t, root, "app/workbench_dashboard.go", "package app")
	b := fileaddr.NewBrowser(newSource(t, root))
	if err := b.Descend("app"); err != nil {
		t.Fatalf("Descend(app): %v", err)
	}
	got, err := b.Query(context.Background(), "kbd", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	want := []string{"app/keybindings.go", "app/workbench_dashboard.go"}
	if strings.Join(labelsOf(got), ",") != strings.Join(want, ",") {
		t.Fatalf("Query(kbd) = %v, want %v", labelsOf(got), want)
	}
	if got[0].Rank != picker.RankSubsequence {
		t.Fatalf("best match tier = %d, want the fuzzy tier %d", got[0].Rank, picker.RankSubsequence)
	}
}

// TestUnit_FileAddrBrowser_QueryReportsTruncation: a scoped search reuses the
// walk's budget and must reuse its truncation reporting too.
func TestUnit_FileAddrBrowser_QueryReportsTruncation(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < fileaddr.WalkBudget+50; i++ {
		writeFile(t, root, filepath.Join("big", "f"+pad(i)+".txt"), "")
	}
	writeFile(t, root, "small/only.txt", "")
	s := newSource(t, root)
	b := fileaddr.NewBrowser(s)

	if err := b.Descend("small"); err != nil {
		t.Fatalf("Descend(small): %v", err)
	}
	if _, err := b.Query(context.Background(), "only", 20); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if s.Truncated() {
		t.Fatal("a search of a small subtree reported truncation")
	}

	b.Ascend()
	if err := b.Descend("big"); err != nil {
		t.Fatalf("Descend(big): %v", err)
	}
	if _, err := b.Query(context.Background(), "txt", 20); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !s.Truncated() {
		t.Fatal("a search that hit the walk budget did not report it")
	}
}

// pad zero-pads to five digits so lexical order is numeric order.
func pad(n int) string {
	s := "00000" + itoa(n)
	return s[len(s)-5:]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestUnit_FileAddrBrowser_CancelledContext(t *testing.T) {
	root := buildBrowseWorkspace(t)
	b := fileaddr.NewBrowser(newSource(t, root))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Entries(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Entries with a cancelled context = %v, want context.Canceled", err)
	}
	if _, err := b.Query(ctx, "go", 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("Query with a cancelled context = %v, want context.Canceled", err)
	}
}

// TestUnit_FileAddrBrowser_FeedsThePickerHeader: breadcrumb as header, entries
// as items, all inside the row budget.
func TestUnit_FileAddrBrowser_FeedsThePickerHeader(t *testing.T) {
	root := buildBrowseWorkspace(t)
	b := fileaddr.NewBrowser(newSource(t, root))
	if err := b.Descend("src"); err != nil {
		t.Fatalf("Descend(src): %v", err)
	}
	items, err := b.Entries(context.Background())
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}

	p := picker.New()
	p.SetItems(items)
	const width, rows = 60, 4
	p.SetHeader(b.Breadcrumb(width, false))

	lines := p.Render(width, rows, false)
	if len(lines) > rows {
		t.Fatalf("rendered %d lines, budget is %d", len(lines), rows)
	}
	if got := lines[0].Text(); got != "/src" {
		t.Fatalf("first line = %q, want the breadcrumb", got)
	}
	if got := lines[1].Text(); !strings.Contains(got, "nested/") {
		t.Fatalf("first row = %q, want the directory row on top", got)
	}
}
