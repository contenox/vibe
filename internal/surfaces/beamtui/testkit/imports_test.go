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
// internal/surfaces/beamtui carries. Rules (b) and (c) below test against
// it rather than a relative path, because Go import specs are always
// fully-qualified.
const beamtuiModule = "github.com/contenox/beam/internal/surfaces/beamtui/"

// termOnlyImports is the set of packages blueprint section 5 restricts to
// beamtui/term (and its subpackages): the two real terminal-control
// dependencies (raw mode, PTYs) and the one stdlib package that exists to
// react to a real terminal (SIGWINCH). Nothing under beamtui/ needs any of
// these except the package whose entire job is talking to a real terminal.
var termOnlyImports = map[string]bool{
	"golang.org/x/term":     true,
	"github.com/creack/pty": true,
	"os/signal":             true,
}

// TestUnit_ImportBoundaries is beam's import-boundary gate (blueprint 4.21):
// it walks every .go file under internal/surfaces/beamtui and asserts, via
// go/parser's ImportsOnly mode (fast — it does not parse function bodies),
// the four seams the blueprint's cross-component contracts (section 5) and
// component catalog (section 4) name as structural, not conventions any
// component is trusted to keep on its own:
//
//	(a) only beamtui/term (or a term subpackage) imports golang.org/x/term,
//	    github.com/creack/pty, or os/signal;
//	(b) enginebridge imports nothing under internal/surfaces/beamtui/ — it
//	    is a runtime client, not a renderer (see enginebridge's package doc);
//	(c) packages under beamtui/comp/ import neither beamtui/term nor
//	    beamtui/input nor beamtui/style — components are pure renderers of
//	    (state, width) -> frame.Line, never terminal-state readers, raw-key
//	    consumers, or SGR-table owners;
//	(d) beamtui/frame imports only the standard library — it is the
//	    dependency-free rendering schema every other package builds on.
//
// A violation is reported with the offending file and import path so an
// agent can self-correct from `go test` output alone, per the blueprint's
// acceptance criterion for this whole package.
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

// isStdlib approximates "is a standard-library import path" the way every
// Go import-linter does: a standard-library path's first component never
// contains a dot (no domain), while every module path — this repo's own
// packages included — does. It is a heuristic, not a lookup against go/build
// (which would need build context and network-free module resolution to be
// robust); it is exact for every import this repo or the Go standard library
// actually uses.
func isStdlib(importPath string) bool {
	first := importPath
	if i := strings.Index(importPath, "/"); i >= 0 {
		first = importPath[:i]
	}
	return !strings.Contains(first, ".")
}

// beamtuiDir locates internal/surfaces/beamtui on disk so the walk in
// TestUnit_ImportBoundaries works identically whether the test runs as part
// of `go test ./...` from the repo root or as `go test .` from inside this
// package's own directory.
//
// The MODULE ROOT is the authority, found by walking up from the working
// directory for the go.mod: Go execs a test binary with its working directory
// set to the package directory, so that walk always lands in the module under
// test. runtime.Caller(0) is only a shortcut on top of it, and one that has to
// be checked rather than trusted — it reports the path this file was COMPILED
// from, which in a module cache, a vendored copy, or a build whose sources
// moved is a different tree than the one being tested. A boundary test that
// silently walks somebody else's checkout passes for the wrong reason, which
// is worse than not running.
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
