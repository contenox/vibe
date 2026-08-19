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

const beamModule = "github.com/contenox/contenox/internal/surfaces/beam/"

// termOnlyImports is the set of packages restricted to beam/term (and
// its subpackages): the real terminal-control dependencies (raw mode,
// PTYs) and the stdlib package for reacting to a real terminal (SIGWINCH).
var termOnlyImports = map[string]bool{
	"golang.org/x/term":     true,
	"github.com/creack/pty": true,
	"os/signal":             true,
}

// TestUnit_ImportBoundaries walks every .go file under internal/surfaces/
// beam and enforces four structural import-boundary rules via
// go/parser's ImportsOnly mode:
//
//	(a) only beam/term (or a subpackage) may import golang.org/x/term,
//	    github.com/creack/pty, or os/signal;
//	(b) enginebridge imports nothing under beam/ except dialect and vfs —
//	    it is a runtime client, not a renderer, and vfs is the containment
//	    its fs/terminal capabilities answer the agent through;
//	(c) beam/comp/ packages import none of beam/term, beam/input,
//	    or beam/style — components are pure (state, width) -> frame.Line
//	    renderers;
//	(d) beam/frame imports only the standard library.
//	(e) no beam package imports this module outside beam/ except libacp and
//	    the one shared client-side fs+terminal server (internal/kernel/
//	    clientfsterm) — beam was a separate module the compiler kept to libacp;
//	    in-tree this rule is what preserves that, so the runtime's internals
//	    cannot leak back into a client (the drift that cost the previous in-tree
//	    TUI). The clientfsterm exception is the single package beam and the
//	    runtime's mission-unit path deliberately share to answer the agent's
//	    fs/terminal capabilities from one contained, env-scrubbed implementation.
//
// A violation reports the offending file and import path.
func TestUnit_ImportBoundaries(t *testing.T) {
	root := beamInternalDir(t)
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
			checkModuleBoundary(t, rel, importPath)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("testkit: walking %s: %v", root, walkErr)
	}
}

const contenoxModule = "github.com/contenox/contenox/"

// sharedClientFSTerm is the one runtime package outside beam/ that a beam
// production file may import (rule (e)'s narrow exception): the shared
// client-side fs+terminal capability server both beam and the runtime's
// mission-unit path consume, so the containment and env-scrub live in exactly
// one place rather than being duplicated in enginebridge.
const sharedClientFSTerm = contenoxModule + "internal/kernel/clientfsterm"

// checkModuleBoundary is rule (e): a beam production file may import this
// module only under beam/ (beamModule), as libacp, or the shared
// clientfsterm server; any other contenox/internal or contenox/lib* path is
// the leak the rule exists to stop. Test files are exempt — a wire-contract
// test legitimately imports the producer type it pins beam's decoder against,
// and a test is never shipped.
func checkModuleBoundary(t *testing.T, file, importPath string) {
	t.Helper()
	if strings.HasSuffix(file, "_test.go") {
		return
	}
	if !strings.HasPrefix(importPath, contenoxModule) {
		return
	}
	if strings.HasPrefix(importPath, beamModule) || importPath == contenoxModule+"libacp" || importPath == sharedClientFSTerm {
		return
	}
	t.Errorf("import boundary: %s imports %q — a beam package may reach this module only under beam/, libacp, or the shared clientfsterm server", file, importPath)
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
	t.Errorf("import boundary: %s imports %q, which only beam/term (or a term subpackage) may import", file, importPath)
}

// checkEngineBridgeBoundary is rule (b).
func checkEngineBridgeBoundary(t *testing.T, file, relDir, importPath string) {
	t.Helper()
	if !inPackageOrSub(relDir, "enginebridge") {
		return
	}
	if importPath == beamModule+"dialect" || importPath == beamModule+"vfs" {
		return
	}
	if strings.HasPrefix(importPath, beamModule) {
		t.Errorf("import boundary: %s (enginebridge) imports %q — enginebridge must stay UI-free and import nothing under beam/ except dialect and vfs", file, importPath)
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
		full := beamModule + name
		if importPath == full || strings.HasPrefix(importPath, full+"/") {
			t.Errorf("import boundary: %s (comp/) imports %q — components are pure renderers and must not import beam/%s", file, importPath, name)
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

func beamInternalDir(t *testing.T) string {
	t.Helper()

	root := moduleRoot(t)
	want := filepath.Join(root, "internal", "surfaces", "beam")

	if _, file, _, ok := runtime.Caller(0); ok {
		dir := filepath.Dir(filepath.Dir(file))
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
