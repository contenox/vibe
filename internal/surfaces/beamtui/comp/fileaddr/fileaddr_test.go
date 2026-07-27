package fileaddr_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/beam/internal/services/vfs"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/fileaddr"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/picker"
)

// writeFile creates dir/name with content, making parents as needed.
func writeFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return p
}

// labelsOf collects Labels in returned order.
func labelsOf(items []picker.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Label)
	}
	return out
}

func contains(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// newSource builds a Factory allowlisting root and a Source over it — the
// same two calls the app makes, so the tests exercise the real resolution
// path rather than a hand-built View.
func newSource(t *testing.T, root string) *fileaddr.Source {
	t.Helper()
	f, err := vfs.NewFactory(root)
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	s, err := fileaddr.NewSource(f, root)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if !s.HasRoot() {
		t.Fatalf("NewSource(%q) produced a rootless Source", root)
	}
	return s
}

// buildWorkspace lays out the fixture tree: real files, gitignored noise,
// skip-dir noise, an in-root symlink, and a symlink whose target is outside
// the root entirely. It returns the workspace root and the outside file's
// basename.
func buildWorkspace(t *testing.T) (root, outsideName string) {
	t.Helper()
	root = t.TempDir()
	outside := t.TempDir() // a SIBLING of root, so it is genuinely out of root

	writeFile(t, root, ".gitignore", strings.Join([]string{
		"# noise",
		"node_modules/",
		"secret.txt",
		"*.log",
		"/rooted-only.txt",
		"generated/",
		// Last match wins: this re-includes one file the *.log rule excluded.
		// (A negation UNDER an excluded directory would be unreachable, in
		// this matcher and in git alike, so it is not what we test here.)
		"!important.log",
	}, "\n")+"\n")

	writeFile(t, root, "keep.go", "package keep")
	writeFile(t, root, "README.md", "# hi")
	writeFile(t, root, "src/app.go", "package src")
	writeFile(t, root, "src/util.go", "package src")
	writeFile(t, root, "src/nested/deep.go", "package nested")

	// gitignored, by three different rule shapes
	writeFile(t, root, "secret.txt", "shh")
	writeFile(t, root, "debug.log", "noise")
	writeFile(t, root, "src/trace.log", "noise")
	writeFile(t, root, "important.log", "re-included by the negation rule")
	writeFile(t, root, "rooted-only.txt", "anchored")
	writeFile(t, root, "src/rooted-only.txt", "not anchored here")
	writeFile(t, root, "node_modules/pkg/index.js", "noise")
	writeFile(t, root, "generated/gen.go", "noise")

	// skip-dir noise that .gitignore says nothing about
	writeFile(t, root, ".git/config", "[core]")
	writeFile(t, root, "vendor/lib/lib.go", "package lib")
	writeFile(t, root, "dist/bundle.js", "noise")

	// A symlink to an in-root regular file: a legitimate candidate.
	if err := os.Symlink(filepath.Join(root, "keep.go"), filepath.Join(root, "inside-link.go")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	// A symlink whose target is outside the root: must never surface.
	outsideName = "outside-secret.txt"
	writeFile(t, outside, outsideName, "off limits")
	if err := os.Symlink(filepath.Join(outside, outsideName), filepath.Join(root, "escape.txt")); err != nil {
		t.Fatalf("symlink escape.txt: %v", err)
	}
	// A symlink to the outside DIRECTORY: the whole subtree must stay out.
	if err := os.Symlink(outside, filepath.Join(root, "escape-dir")); err != nil {
		t.Fatalf("symlink escape-dir: %v", err)
	}
	return root, outsideName
}

func TestUnit_FileAddrCandidates_ExcludesNoiseAndOutOfRootTargets(t *testing.T) {
	root, outsideName := buildWorkspace(t)
	s := newSource(t, root)

	items, err := s.Candidates(context.Background(), "", 10000)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	labels := labelsOf(items)

	want := []string{
		".gitignore",
		"README.md",
		"important.log", // gitignore negation, last match wins
		"inside-link.go",
		"keep.go",
		"src/app.go",
		"src/nested/deep.go",
		"src/rooted-only.txt", // the anchored rule only ignores the root copy
		"src/util.go",
	}
	for _, w := range want {
		if !contains(labels, w) {
			t.Errorf("expected candidate %q, got %v", w, labels)
		}
	}

	// Every exclusion, named individually so a regression says which rule broke.
	forbidden := map[string]string{
		"secret.txt":                "gitignore literal",
		"debug.log":                 "gitignore glob",
		"src/trace.log":             "gitignore glob at depth",
		"rooted-only.txt":           "gitignore root-anchored rule",
		"node_modules/pkg/index.js": "gitignore dir rule + skip-dir",
		"generated/gen.go":          "gitignore dir rule",
		".git/config":               "skip-dir",
		"vendor/lib/lib.go":         "skip-dir",
		"dist/bundle.js":            "skip-dir",
		"escape.txt":                "symlink target outside the root",
	}
	for bad, why := range forbidden {
		if contains(labels, bad) {
			t.Errorf("candidate %q leaked (%s)", bad, why)
		}
	}

	// Nothing reached through the escaping directory link, under any label.
	for _, l := range labels {
		if strings.HasPrefix(l, "escape-dir") || strings.Contains(l, outsideName) {
			t.Errorf("out-of-root path leaked as candidate %q", l)
		}
	}

	// Labels are root-relative slash paths; IDs are absolute and in-root.
	for _, it := range items {
		if filepath.IsAbs(it.Label) || strings.Contains(it.Label, "\\") {
			t.Errorf("label %q is not a root-relative slash path", it.Label)
		}
		if !filepath.IsAbs(it.ID) {
			t.Errorf("ID %q is not absolute", it.ID)
		}
		if !vfs.Within(root, it.ID) {
			t.Errorf("ID %q is outside the workspace root %q", it.ID, root)
		}
	}

	// Detail is the parent directory, empty at the root.
	details := map[string]string{}
	for _, it := range items {
		details[it.Label] = it.Detail
	}
	if got := details["src/nested/deep.go"]; got != "src/nested" {
		t.Errorf("Detail for src/nested/deep.go = %q, want %q", got, "src/nested")
	}
	if got := details["keep.go"]; got != "" {
		t.Errorf("Detail for a root-level file = %q, want empty", got)
	}
}

func TestUnit_FileAddrCandidates_RanksAndCaps(t *testing.T) {
	root, _ := buildWorkspace(t)
	s := newSource(t, root)

	items, err := s.Candidates(context.Background(), "app", 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("no candidates for query \"app\"")
	}
	if items[0].Label != "src/app.go" {
		t.Fatalf("best match = %q, want src/app.go", items[0].Label)
	}
	if items[0].Rank != picker.RankBasenamePrefix {
		t.Fatalf("best match tier = %d, want %d", items[0].Rank, picker.RankBasenamePrefix)
	}

	// The limit is honoured, and a non-positive limit means the default.
	capped, err := s.Candidates(context.Background(), "", 3)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(capped) != 3 {
		t.Fatalf("limit 3 returned %d candidates", len(capped))
	}
	defaulted, err := s.Candidates(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(defaulted) > fileaddr.DefaultLimit {
		t.Fatalf("limit 0 returned %d candidates, want <= %d", len(defaulted), fileaddr.DefaultLimit)
	}

	// A query nothing matches is an empty list, not an error — the caller
	// renders the picker's empty state.
	none, err := s.Candidates(context.Background(), "zzz-no-such-thing-zzz", 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("expected no candidates, got %v", labelsOf(none))
	}
	if s.EmptyText() != fileaddr.NoMatchText {
		t.Fatalf("EmptyText with a root = %q, want %q", s.EmptyText(), fileaddr.NoMatchText)
	}
}

func TestUnit_FileAddrCandidates_BudgetRespectedAndDeterministic(t *testing.T) {
	root := t.TempDir()
	// Comfortably more files than the budget, zero-padded so lexical order
	// (the documented walk order) is also numeric order.
	const total = fileaddr.WalkBudget + 1000
	for i := 1; i <= total; i++ {
		p := filepath.Join(root, fmt.Sprintf("f%05d.txt", i))
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	s := newSource(t, root)

	start := time.Now()
	items, err := s.Candidates(context.Background(), "", total*2)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 30*time.Second {
		t.Fatalf("walk took %v — the budget is not bounding it", elapsed)
	}

	if len(items) != fileaddr.WalkBudget {
		t.Fatalf("got %d candidates, want exactly the budget %d", len(items), fileaddr.WalkBudget)
	}
	labels := labelsOf(items)
	// The budget cuts at a KNOWN point because the order is lexical.
	if labels[0] != "f00001.txt" {
		t.Fatalf("first candidate = %q, want f00001.txt (lexical order)", labels[0])
	}
	if last, want := labels[len(labels)-1], fmt.Sprintf("f%05d.txt", fileaddr.WalkBudget); last != want {
		t.Fatalf("last candidate = %q, want %q", last, want)
	}
	if contains(labels, fmt.Sprintf("f%05d.txt", fileaddr.WalkBudget+1)) {
		t.Fatal("a file past the walk budget was returned")
	}

	// The budget must be REPORTABLE. Without this the walk stopping is
	// indistinguishable from the tree ending, so a monorepo shows an ordinary
	// "no matching files" and the user concludes their file is not there.
	if !s.Truncated() {
		t.Fatal("Truncated() = false after the walk stopped at the budget")
	}

	// Same tree, same answer: the truncation point is reproducible, not a
	// race against directory-entry iteration order.
	again, err := s.Candidates(context.Background(), "", total*2)
	if err != nil {
		t.Fatalf("Candidates (second run): %v", err)
	}
	if strings.Join(labelsOf(again), "\n") != strings.Join(labels, "\n") {
		t.Fatal("two identical queries returned different candidate lists")
	}
}

// TestUnit_FileAddrTruncated_FalseWhenTheWalkFinished: the flag reports the
// BUDGET, not "there were files". A tree the walk saw all of has nothing
// hidden behind it, and saying otherwise would teach the operator to ignore
// the notice on the one repository where it matters.
func TestUnit_FileAddrTruncated_FalseWhenTheWalkFinished(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	s := newSource(t, root)

	if s.Truncated() {
		t.Fatal("Truncated() = true before any walk")
	}
	if _, err := s.Candidates(context.Background(), "", 0); err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if s.Truncated() {
		t.Fatal("Truncated() = true after a walk that reached the end of the tree")
	}

	// A rootless Source never walks, so it never truncates.
	rootless, err := fileaddr.NewSource(nil, "")
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if _, err := rootless.Candidates(context.Background(), "", 0); err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if rootless.Truncated() {
		t.Fatal("a rootless Source reported a truncated walk")
	}
	if (*fileaddr.Source)(nil).Truncated() {
		t.Fatal("a nil Source reported a truncated walk")
	}
}

func TestUnit_FileAddrSource_NoRootIsTheFixedEmptyState(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) *fileaddr.Source
	}{
		{"no factory and no cwd", func(t *testing.T) *fileaddr.Source {
			s, err := fileaddr.NewSource(nil, "")
			if err != nil {
				t.Fatalf("NewSource: %v", err)
			}
			return s
		}},
		{"cwd outside every allowlisted root", func(t *testing.T) *fileaddr.Source {
			allowed := t.TempDir()
			denied := t.TempDir() // a sibling, under no configured root
			f, err := vfs.NewFactory(allowed)
			if err != nil {
				t.Fatalf("NewFactory: %v", err)
			}
			s, err := fileaddr.NewSource(f, denied)
			if err != nil {
				t.Fatalf("NewSource: %v", err)
			}
			return s
		}},
		{"relative cwd is refused outright", func(t *testing.T) *fileaddr.Source {
			f, err := vfs.NewFactory(t.TempDir())
			if err != nil {
				t.Fatalf("NewFactory: %v", err)
			}
			s, err := fileaddr.NewSource(f, "some/relative/dir")
			if err != nil {
				t.Fatalf("NewSource: %v", err)
			}
			return s
		}},
		{"nil Source", func(t *testing.T) *fileaddr.Source { return nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.build(t)
			if s.HasRoot() {
				t.Fatalf("HasRoot = true, want false")
			}
			if s.Root() != "" {
				t.Fatalf("Root = %q, want empty", s.Root())
			}
			if s.EmptyText() != fileaddr.NoRootText {
				t.Fatalf("EmptyText = %q, want %q", s.EmptyText(), fileaddr.NoRootText)
			}
			items, err := s.Candidates(context.Background(), "anything", 20)
			if err != nil {
				t.Fatalf("Candidates on a rootless Source: %v", err)
			}
			if len(items) != 0 {
				t.Fatalf("rootless Source returned %d candidates", len(items))
			}
		})
	}
}

func TestUnit_FileAddrSource_ResolvesRootLikeTheRuntime(t *testing.T) {
	allowed := t.TempDir()
	writeFile(t, allowed, "sub/child.go", "package sub")
	f, err := vfs.NewFactory(allowed)
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	// The "/" sentinel and the empty cwd both mean "the default root" — the
	// compat story ResolveSessionCwd owns and beam sends today.
	for _, cwd := range []string{"", "/"} {
		s, err := fileaddr.NewSource(f, cwd)
		if err != nil {
			t.Fatalf("NewSource(%q): %v", cwd, err)
		}
		if !s.HasRoot() {
			t.Fatalf("NewSource(%q) has no root", cwd)
		}
		if got, want := s.Root(), f.Default(); got != want {
			t.Fatalf("NewSource(%q).Root() = %q, want the default root %q", cwd, got, want)
		}
	}

	// A subdirectory of a granted root is itself a legal session cwd, and
	// candidates are then relative to THAT directory.
	sub := filepath.Join(allowed, "sub")
	s, err := fileaddr.NewSource(f, sub)
	if err != nil {
		t.Fatalf("NewSource(sub): %v", err)
	}
	items, err := s.Candidates(context.Background(), "", 20)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if got := labelsOf(items); len(got) != 1 || got[0] != "child.go" {
		t.Fatalf("candidates under the subdirectory = %v, want [child.go]", got)
	}

	// With no allowlist (the stdio/editor path) an absolute cwd is adopted
	// as-is, which is exactly ResolveSessionCwd's rule 4.
	stdio, err := fileaddr.NewSource(nil, allowed)
	if err != nil {
		t.Fatalf("NewSource(nil, abs): %v", err)
	}
	if !stdio.HasRoot() {
		t.Fatal("an absolute cwd with no allowlist should still have a root")
	}
}

func TestUnit_FileAddrCandidates_CancellationStopsTheWalk(t *testing.T) {
	root, _ := buildWorkspace(t)
	s := newSource(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Candidates(ctx, "", 20); err == nil {
		t.Fatal("a cancelled context returned no error")
	} else if err != context.Canceled {
		t.Fatalf("error = %v, want context.Canceled unwrapped", err)
	}
}

func TestUnit_FileAddrSessionItems(t *testing.T) {
	items := fileaddr.SessionItems([]string{"zeta", "alpha", "mid"}, "alpha")
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	// Caller order survives — the roster's own ordering is authoritative.
	for i, want := range []string{"zeta", "alpha", "mid"} {
		if items[i].Label != want || items[i].ID != want {
			t.Fatalf("item %d = {%q,%q}, want %q", i, items[i].ID, items[i].Label, want)
		}
	}
	if items[1].Detail != "active" {
		t.Fatalf("active session Detail = %q, want %q", items[1].Detail, "active")
	}
	if items[0].Detail != "" || items[2].Detail != "" {
		t.Fatal("a non-active session was marked active")
	}
	// An empty active name marks nothing.
	for _, it := range fileaddr.SessionItems([]string{"a", ""}, "") {
		if it.Detail != "" {
			t.Fatalf("empty active name marked %q", it.Label)
		}
	}
	if got := fileaddr.SessionItems(nil, ""); len(got) != 0 {
		t.Fatalf("SessionItems(nil) = %v, want empty", got)
	}
}
