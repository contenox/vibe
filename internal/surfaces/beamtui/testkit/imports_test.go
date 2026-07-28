package testkit

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// beamtuiModule is the import-path prefix every package under
// internal/surfaces/beamtui carries; rules (b) and (c) test against it
// since Go import specs are always fully-qualified.
const beamtuiModule = "github.com/contenox/contenox/internal/surfaces/beamtui/"

// termOnlyImports is the set of packages restricted to beamtui/term (and
// its subpackages): the real terminal-control dependencies (raw mode,
// PTYs) and the stdlib package for reacting to a real terminal (SIGWINCH).
var termOnlyImports = map[string]bool{
	"golang.org/x/term":     true,
	"github.com/creack/pty": true,
	"os/signal":             true,
}

// TestUnit_ImportBoundaries walks every .go file under internal/surfaces/
// beamtui and enforces four structural import-boundary rules via
// go/parser's ImportsOnly mode:
//
//	(a) only beamtui/term (or a subpackage) may import golang.org/x/term,
//	    github.com/creack/pty, or os/signal;
//	(b) enginebridge imports nothing under beamtui/ — it is a runtime
//	    client, not a renderer;
//	(c) beamtui/comp/ packages import none of beamtui/term, beamtui/input,
//	    or beamtui/style — components are pure (state, width) -> frame.Line
//	    renderers;
//	(d) beamtui/frame imports only the standard library.
//
// A violation reports the offending file and import path.
func TestUnit_ImportBoundaries(t *testing.T) {
	root := beamtuiDir(t)
	fset := token.NewFileSet()

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		relDir := filepath.ToSlash(filepath.Dir(rel))
		if relDir == "." {
			relDir = ""
		}

		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("testkit: parse %s: %w", path, err)
		}

		for _, imp := range f.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			checkTermBoundary(t, rel, relDir, importPath)
			checkEngineBridgeBoundary(t, rel, relDir, importPath)
			checkCompBoundary(t, rel, relDir, importPath)
			checkFrameBoundary(t, rel, relDir, importPath)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("testkit: walking %s: %v", root, walkErr)
	}
}

// inPackageOrSub reports whether relDir is exactly name or a subpackage of
// it (name/anything). Both count as "the name package" for these rules: a
// term-internal helper package still only talks to a real terminal.
func inPackageOrSub(relDir, name string) bool {
	return relDir == name || strings.HasPrefix(relDir, name+"/")
}

// checkTermBoundary is rule (a).
func checkTermBoundary(t *testing.T, file, relDir, importPath string) {
	t.Helper()
	if !termOnlyImports[importPath] {
		return
	}
	if inPackageOrSub(relDir, "term") {
		return
	}
	t.Errorf("import boundary: %s imports %q, which only beamtui/term (or a term subpackage) may import", file, importPath)
}

// checkEngineBridgeBoundary is rule (b).
func checkEngineBridgeBoundary(t *testing.T, file, relDir, importPath string) {
	t.Helper()
	if !inPackageOrSub(relDir, "enginebridge") {
		return
	}
	if strings.HasPrefix(importPath, beamtuiModule) {
		t.Errorf("import boundary: %s (enginebridge) imports %q — enginebridge must stay UI-free and import nothing under beamtui/", file, importPath)
	}
}

// compForbidden is rule (c)'s closed list: the packages a pure renderer
// under comp/ must never reach for.
var compForbidden = []string{"term", "input", "style"}

// checkCompBoundary is rule (c).
func checkCompBoundary(t *testing.T, file, relDir, importPath string) {
	t.Helper()
	if !inPackageOrSub(relDir, "comp") {
		return
	}
	for _, name := range compForbidden {
		full := beamtuiModule + name
		if importPath == full || strings.HasPrefix(importPath, full+"/") {
			t.Errorf("import boundary: %s (comp/) imports %q — components are pure renderers and must not import beamtui/%s", file, importPath, name)
		}
	}
}

// checkFrameBoundary is rule (d).
func checkFrameBoundary(t *testing.T, file, relDir, importPath string) {
	t.Helper()
	if relDir != "frame" {
		return
	}
	if isStdlib(importPath) {
		return
	}
	t.Errorf("import boundary: %s (frame) imports %q — frame may depend only on the standard library", file, importPath)
}

// isStdlib reports whether importPath looks like a standard-library path:
// its first component contains no dot, unlike every module path. It is a
// heuristic, not a go/build lookup, but exact for every import this repo or
// the standard library actually uses.
func isStdlib(importPath string) bool {
	first := importPath
	if i := strings.Index(importPath, "/"); i >= 0 {
		first = importPath[:i]
	}
	return !strings.Contains(first, ".")
}

// beamtuiDir locates internal/surfaces/beamtui on disk, so the walk in
// TestUnit_ImportBoundaries works whether the test runs from the repo root
// or from this package's own directory.
//
// The module root (found by walking up for go.mod) is the authority;
// runtime.Caller(0) is only a shortcut checked against it, since it reports
// where this file was compiled from, which can be a different tree (module
// cache, vendored copy) than the one under test.
func beamtuiDir(t *testing.T) string {
	t.Helper()

	root := moduleRoot(t)
	want := filepath.Join(root, "internal", "surfaces", "beamtui")

	if _, file, _, ok := runtime.Caller(0); ok {
		dir := filepath.Dir(filepath.Dir(file)) // ".../testkit/imports_test.go" -> ".../beamtui"
		if dir == want {
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				return dir
			}
		}
	}

	if info, err := os.Stat(want); err == nil && info.IsDir() {
		return want
	}
	t.Fatalf("testkit: %s is not a directory (module root %s)", want, root)
	return ""
}

// moduleRoot is the nearest ancestor of the working directory holding a
// go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("testkit: os.Getwd: %v", err)
	}
	for dir := wd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("testkit: no go.mod above %s", wd)
			return ""
		}
		dir = parent
	}
}
