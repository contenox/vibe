package libtracker

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// guardedRoots are the trees the rule covers: every line of this project's own
// logic. libacp/ is a published protocol library with its own rules and tools/
// and examples/ are not the product, so they are out of scope here.
var guardedRoots = []string{"internal", "cmd"}

// slogSinkAllowlist is the CLOSED set of files permitted to import log/slog,
// keyed by module-root-relative slash path. A trailing "/" marks a directory
// prefix; everything else is one exact file.
//
// The distinction the allowlist encodes is CONFIGURING A SINK versus CALLING A
// LOGGER. libtracker's ActivityTracker is this repo's only instrumentation
// seam, and slog is the sink one implementation of it happens to write through
// — so pointing the default handler at a file is plumbing the seam, while
// slog.Warn("...", "args", args) in command or service logic bypasses the seam
// entirely. That is not a style preference: values reported through the tracker
// are scrubbed by field name before they are written (redact.go), so a direct
// slog call is also a credential-redaction bypass, which is why the rule is a
// test and not a comment.
//
// Entries are individual FILES, never packages, outside libtracker itself. A
// package-wide exemption is how this regrows: one legitimate SetDefault in a
// composition root would license every future slog.Warn added beside it.
var slogSinkAllowlist = map[string]string{
	// The sink adapter itself. slog is this package's OUTPUT FORMAT: it builds
	// a tracker over an *slog.Logger, stamps request/trace/span IDs onto every
	// record, and redacts on the way out. It is the one place a package-wide
	// allowance is correct, because being the slog boundary is the whole job.
	"internal/libtracker/": "the tracker's slog sink adapter — slog is its output, not its API",

	// Composition roots that CONFIGURE the sink, named file by file.
	"internal/surfaces/contenoxcli/cli.go":      "setupTelemetryLogging: tees the default handler to <data-dir>/telemetry.log when the operator sets telemetry-enabled",
	"internal/surfaces/contenoxcli/beam_cmd.go": "redirectBeamLogsToFile: moves the default handler OFF stderr into beam.log — beam owns the terminal, so a stray record would be drawn over the transcript",

	// Tests that must observe the sink wiring to prove it works.
	"internal/surfaces/contenoxcli/beam_cmd_test.go": "asserts redirectBeamLogsToFile actually retargets slog.Default() and restores it on failure — it has to call slog to see that",
}

// TestUnit_NoDirectSlogOutsideSinks is the guard that keeps libtracker the only
// instrumentation seam in this repo.
//
// It walks every .go file under internal/ and cmd/ with go/parser in
// ImportsOnly mode (fast — function bodies are never parsed) and fails on any
// import of log/slog from a file that is not on slogSinkAllowlist above, whose
// doc comment carries each entry's justification.
//
// It lives in libtracker rather than in a test-only package because the rule is
// libtracker's own invariant: the reason direct slog is forbidden is that the
// tracker redacts and correlates and a bypass does neither, so the guard is
// findable by the next person reading the seam it protects. The test imports
// nothing from the tree it walks (it reads files, it does not link them), so
// there is no dependency cycle and no reason to add a package just to hold it.
//
// If this fails on a file you just wrote, the fix is almost never the
// allowlist: report through an ActivityTracker (see the usage pattern on the
// interface in activitytracker.go), or, if the thing you are writing is a
// message an operator must READ AND ACT ON, print it to the command's stderr
// instead — that was never a log line to begin with.
func TestUnit_NoDirectSlogOutsideSinks(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	var offenders []string
	for _, guarded := range guardedRoots {
		dir := filepath.Join(root, guarded)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("libtracker: guarded root %s is missing — this guard is walking the wrong tree", dir)
		}
		walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// testdata holds golden files and fixtures, not compiled code.
				if d.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			rel = filepath.ToSlash(rel)

			f, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if parseErr != nil {
				return fmt.Errorf("parse %s: %w", rel, parseErr)
			}
			for _, imp := range f.Imports {
				importPath, uErr := strconv.Unquote(imp.Path.Value)
				if uErr != nil || importPath != "log/slog" {
					continue
				}
				if allowed(rel) {
					continue
				}
				offenders = append(offenders, rel)
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("libtracker: walking %s: %v", dir, walkErr)
		}
	}

	if len(offenders) == 0 {
		return
	}
	sort.Strings(offenders)
	t.Errorf(`%d file(s) import "log/slog" directly:

%s

log/slog is the ActivityTracker's OUTPUT SINK, not an API for project logic to
call. Values reported through the tracker are redacted by field name before they
are written (internal/libtracker/redact.go) and stamped with request/trace/span
IDs from the context; a direct slog call gets neither, so it can put a token in
a log file.

Replace it with one of:
  - TELEMETRY (an event about the program's own behavior) -> report it through
    an ActivityTracker. The pattern is on the interface in
    internal/libtracker/activitytracker.go; a worked consumer, including the
    nil-tracker-degrades-to-Noop idiom, is internal/services/reportrouter.
  - A MESSAGE THE OPERATOR MUST ACT ON -> print it, in the surrounding command's
    voice, to that command's stderr. It was never a log line.
  - GENUINE SINK WIRING (slog.SetDefault / building a handler, and NOTHING else
    in the file) -> add the file, by exact path and with its reason, to
    slogSinkAllowlist in %s.`, len(offenders), "  "+strings.Join(offenders, "\n  "), "internal/libtracker/slog_guard_test.go")
}

// allowed reports whether rel is on slogSinkAllowlist, as an exact file or
// under an allowed directory prefix.
func allowed(rel string) bool {
	for entry := range slogSinkAllowlist {
		if strings.HasSuffix(entry, "/") {
			if strings.HasPrefix(rel, entry) {
				return true
			}
			continue
		}
		if rel == entry {
			return true
		}
	}
	return false
}

// moduleRoot is the nearest ancestor of the working directory holding a go.mod.
// Go runs a test binary with its working directory set to the package
// directory, so this always lands in the module under test — which matters for
// a guard: one that silently walks a different checkout passes for the wrong
// reason.
func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("libtracker: os.Getwd: %v", err)
	}
	for dir := wd; ; {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("libtracker: no go.mod above %s", wd)
			return ""
		}
		dir = parent
	}
}
