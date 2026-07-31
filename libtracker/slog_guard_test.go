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

// guardedRoots covers this project's own logic; libacp is a published library
// with its own rules, and tools/examples aren't the product. libbus, libdbexec,
// and libkvstore now live under internal/ (already walked). liblease was only
// ever consumed by modeld's device-lease code, which now lives entirely in the
// separate modeld repo, so it has no importers left here and was removed;
// libtracker still sits beside internal/ rather than inside it so it stays
// importable from outside the module, but it is still this project's logic,
// so the guard keeps walking it.
var guardedRoots = []string{"internal", "cmd", "libtracker"}

// slogSinkAllowlist is the closed set of files permitted to import log/slog,
// keyed by module-root-relative slash path (trailing "/" = directory prefix,
// else an exact file). The rule distinguishes configuring the sink from
// calling a logger: libtracker's ActivityTracker is the only instrumentation
// seam and redacts by field name before writing, so a direct slog call
// bypasses redaction, not just style. Entries are individual files, never
// packages, outside libtracker itself — a package-wide exemption would
// license every future slog call added beside it.
var slogSinkAllowlist = map[string]string{
	// The sink adapter itself: builds a tracker over an *slog.Logger, stamps
	// request/trace/span IDs, and redacts on the way out.
	"libtracker/": "the tracker's slog sink adapter — slog is its output, not its API",

	// Composition roots that configure the sink, named file by file.
	"internal/surfaces/contenoxcli/cli.go":      "setupTelemetryLogging: tees the default handler to <data-dir>/telemetry.log when the operator sets telemetry-enabled",
	"internal/surfaces/contenoxcli/beam_cmd.go": "redirectBeamLogsToFile: moves the default handler OFF stderr into beam.log — beam owns the terminal, so a stray record would be drawn over the transcript",

	// Tests that must observe the sink wiring to prove it works.
	"internal/surfaces/contenoxcli/beam_cmd_test.go": "asserts redirectBeamLogsToFile actually retargets slog.Default() and restores it on failure — it has to call slog to see that",
}

// TestUnit_NoDirectSlogOutsideSinks keeps libtracker the only instrumentation
// seam: it walks internal/ and cmd/, parsing imports only, and fails on any
// log/slog import from a file not on slogSinkAllowlist. On failure, report
// through an ActivityTracker instead, or print operator-facing messages to
// the command's stderr; extend the allowlist only for genuine sink wiring.
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
are written (libtracker/redact.go) and stamped with request/trace/span
IDs from the context; a direct slog call gets neither, so it can put a token in
a log file.

Replace it with one of:
  - TELEMETRY (an event about the program's own behavior) -> report it through
    an ActivityTracker. The pattern is on the interface in
    libtracker/activitytracker.go; a worked consumer, including the
    nil-tracker-degrades-to-Noop idiom, is internal/services/reportrouter.
  - A MESSAGE THE OPERATOR MUST ACT ON -> print it, in the surrounding command's
    voice, to that command's stderr. It was never a log line.
  - GENUINE SINK WIRING (slog.SetDefault / building a handler, and NOTHING else
    in the file) -> add the file, by exact path and with its reason, to
    slogSinkAllowlist in %s.`, len(offenders), "  "+strings.Join(offenders, "\n  "), "libtracker/slog_guard_test.go")
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

// moduleRoot is the nearest ancestor of the working directory holding a
// go.mod, so the guard always walks the module under test.
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
