package gointel

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
)

// E2E hardening: hostile arguments cannot panic or escape the workspace, concurrent queries/edits/teardown do not race, and real-world module shapes answer rather than hang. Tests loading this repository are gated on -short.

// repoRootDir walks up to the module root of this repository.
func repoRootDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// repoIndex returns an index over this repository, the only workspace big enough for the budget and cross-package claims to mean anything.
func repoIndex(t *testing.T) *index {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping: loads this repository (100+ packages, ~110 MB warm)")
	}
	return newTestIndex(t, repoRootDir(t))
}

// writeModule materialises a throwaway Go module: go.mod plus the named files, paths relative to the module root. Stdlib-only content keeps `go list` off the network.
func writeModule(t *testing.T, dir, modulePath string, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	all := map[string]string{"go.mod": "module " + modulePath + "\n\ngo 1.21\n"}
	for name, body := range files {
		all[name] = body
	}
	for name, body := range all {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// exec runs one tool through the real ToolsRepo dispatch — the same entry point the engine calls, argument coercion and all.
func execTool(t *testing.T, repo taskengine.ToolsRepo, tool string, args map[string]any) (out any, err error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("%s(%v) PANICKED: %v", tool, args, p)
		}
	}()
	out, _, err = repo.Exec(context.Background(), time.Now(), args, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: tool})
	return out, err
}

// assertTeachingError checks a refusal names a severity, is bounded, and never leaks a raw control character.
func assertTeachingError(t *testing.T, label string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	if !strings.Contains(msg, severityRecoverable) && !strings.Contains(msg, severityFatalToken) {
		t.Errorf("%s: error %q carries no severity marker", label, msg)
	}
	if len(msg) > maxErrorBytes {
		t.Errorf("%s: error is %d bytes (cap %d) — a model-supplied argument is being echoed unbounded", label, len(msg), maxErrorBytes)
	}
	for _, r := range msg {
		if r == 0 || (r < 0x20 && r != '\n' && r != '\t') {
			t.Errorf("%s: error embeds a raw control character %U: %q", label, r, msg)
			break
		}
	}
}

// maxErrorBytes is what a single teaching error may cost: generous enough for the ambiguity and diagnostics refusals, far below an unclamped echo.
const maxErrorBytes = 4096

// hostileStrings are the argument values a model can be talked into emitting; each is passed to every string argument of every tool.
var hostileStrings = map[string]string{
	"empty":            "",
	"blank":            "   ",
	"traversal":        "../../etc/passwd",
	"deep_traversal":   "../../../../../../../../etc/passwd",
	"absolute":         "/etc/passwd",
	"absolute_shadow":  "/etc",
	"nul_byte":         "pkg\x00.Ident",
	"format_verbs":     "%s%s%n%p%#v",
	"bidi":             "\u202eIdent\u202d\u200b",
	"rtl_mixed":        "frame.\u0627\u0644\u0639\u0631\u0628\u064a\u0629",
	"newlines":         "frame.\nStyleBrand\r\n",
	"shell":            "$(rm -rf /); `id`",
	"glob":             "**/*",
	"dotdot_only":      "..",
	"dot":              ".",
	"windows_absolute": `C:\Windows\System32`,
	"url":              "https://example.com/pkg.Ident",
	"huge":             strings.Repeat("Aa0", 3500), // ~10 KB
	"huge_dotted":      strings.Repeat("pkg.", 2600),
	"nul_only":         "\x00",
}

// TestSystem_GoIntel_HostileSymbolArgumentsAreRefusedNotObeyed pins: no panic, a bounded teaching error or a clean result, never a result outside the workspace.
func TestSystem_GoIntel_HostileSymbolArgumentsAreRefusedNotObeyed(t *testing.T) {
	root := newFixture(t, "fixture")
	repo := NewTools(newTestIndex(t, root))

	for _, tool := range []string{ToolDescribe, ToolDefinition, ToolReferences, ToolImplementations} {
		for name, value := range hostileStrings {
			label := tool + "/" + name
			out, err := execTool(t, repo, tool, map[string]any{"symbol": value})
			if err == nil {
				// A hostile symbol that resolves must resolve inside the fixture.
				assertResultStaysInWorkspace(t, label, root, out)
				continue
			}
			assertTeachingError(t, label, err)
		}
	}
}

// TestSystem_GoIntel_HostileDirArgumentsCannotEscapeTheWorkspace pins: a dir escaping the allowed directory is always refused with ErrOutsideAllowedDir.
func TestSystem_GoIntel_HostileDirArgumentsCannotEscapeTheWorkspace(t *testing.T) {
	outside := t.TempDir()
	writeModule(t, outside, "example.com/outside", map[string]string{
		"secret/secret.go": "package secret\n\n// Secret must never be reachable.\nconst Secret = \"leaked\"\n",
	})

	root := newFixture(t, "fixture")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	repo := NewTools(newTestIndex(t, root))

	escaping := map[string]string{
		"traversal":         "../..",
		"traversal_to_root": "../../../../../../../..",
		"absolute_outside":  outside,
		"absolute_etc":      "/etc",
		"symlink_escape":    "escape",
		"symlink_deep":      "escape/secret",
	}
	for name, dir := range escaping {
		label := "dir/" + name
		out, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": "shapes.Rect", "dir": dir})
		if err == nil {
			t.Errorf("%s: dir %q was ACCEPTED (result %#v) — containment is the whole boundary", label, dir, out)
			continue
		}
		if !errors.Is(err, ErrOutsideAllowedDir) {
			t.Errorf("%s: error %v is not ErrOutsideAllowedDir — a caller cannot tell containment from any other refusal", label, err)
		}
		if !strings.Contains(err.Error(), "allowed directory") {
			t.Errorf("%s: error %q does not name containment as the cause", label, err)
		}
		assertTeachingError(t, label, err)
	}

	// The symbol that only exists outside must not be reachable by any spelling.
	for _, symbol := range []string{"secret.Secret", "Secret", "example.com/outside/secret.Secret"} {
		if _, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": symbol}); err == nil {
			t.Errorf("symbol %q resolved into a module outside the workspace", symbol)
		}
	}
}

// TestSystem_GoIntel_HostileDirArgumentsThatStayInsideAnswerCleanly pins: a dir inside the boundary always answers, even if nonexistent or a file.
func TestSystem_GoIntel_HostileDirArgumentsThatStayInsideAnswerCleanly(t *testing.T) {
	root := newFixture(t, "fixture")
	repo := NewTools(newTestIndex(t, root))

	for name, dir := range map[string]string{
		"nonexistent":      "no/such/dir",
		"nonexistent_file": "no/such/file.go",
		"a_file_not_a_dir": "shapes/shapes.go",
		"dot":              ".",
		"dot_slash":        "./shapes",
		"trailing_slash":   "shapes/",
		"redundant":        "shapes/../report",
	} {
		label := "dir/" + name
		out, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": "shapes.Rect", "dir": dir})
		if err != nil {
			assertTeachingError(t, label, err)
			continue
		}
		res, ok := out.(*DefinitionResult)
		if !ok {
			t.Fatalf("%s: result is %T", label, out)
		}
		if res.Location != "shapes/shapes.go:25:6" {
			t.Errorf("%s: location = %q, want the fixture's own declaration", label, res.Location)
		}
	}
}

// TestSystem_GoIntel_HostileCapsAreClampedNotObeyed pins: an out-of-range max clamps to the ceiling or falls back to the default, never obeyed verbatim.
func TestSystem_GoIntel_HostileCapsAreClampedNotObeyed(t *testing.T) {
	root := newFixture(t, "fixture")
	repo := NewTools(newTestIndex(t, root))

	for name, max := range map[string]any{
		"zero":          0,
		"negative":      -1,
		"very_negative": -1 << 40,
		"billion":       1e9,
		"float_huge":    1e30,
		"nan":           math.NaN(),
		"inf":           math.Inf(1),
		"string_junk":   "not-a-number",
		"string_huge":   "999999999999999999999999",
		"bool":          true,
	} {
		for _, tc := range []struct {
			tool string
			args map[string]any
			cap  int
		}{
			{ToolReferences, map[string]any{"symbol": "shapes.Unit", "max": max}, maxRefCap},
			{ToolSymbols, map[string]any{"target": "shapes", "max": max}, maxSymbolCap},
			{ToolDiagnostics, map[string]any{"scope": "all", "max": max}, maxDiagCap},
		} {
			label := fmt.Sprintf("%s/max=%s", tc.tool, name)
			out, err := execTool(t, repo, tc.tool, tc.args)
			if err != nil {
				assertTeachingError(t, label, err)
				continue
			}
			shown := shownOf(t, out)
			if shown > tc.cap {
				t.Errorf("%s: returned %d entries, above the %d ceiling", label, shown, tc.cap)
			}
		}
	}
}

// TestSystem_GoIntel_HostilePassesAndScopesAreRefusedByName pins: an unknown scope or pass is refused regardless of ordering relative to valid values.
func TestSystem_GoIntel_HostilePassesAndScopesAreRefusedByName(t *testing.T) {
	root := newFixture(t, "fixture")
	repo := NewTools(newTestIndex(t, root))

	for name, scope := range map[string]string{
		"garbage":   "everything",
		"traversal": "../../etc",
		"huge":      hostileStrings["huge"],
		"nul":       "all\x00",
		"nearly":    "alll",
		"verbs":     "%s%n",
	} {
		label := "scope/" + name
		_, err := execTool(t, repo, ToolDiagnostics, map[string]any{"scope": scope})
		if err == nil {
			t.Errorf("%s: scope %q was accepted", label, scope)
			continue
		}
		if !strings.Contains(err.Error(), "unknown diagnostics scope") {
			t.Errorf("%s: error %q does not name the argument", label, err)
		}
		assertTeachingError(t, label, err)
	}

	// Pass sets that must be refused, including "all" mixed with an unknown name in both orders.
	for name, passes := range map[string]string{
		"unknown":         "notapass",
		"unknown_first":   "notapass,all",
		"unknown_last":    "all,notapass",
		"unknown_between": "printf,notapass,all",
		"huge":            hostileStrings["huge"],
		"traversal":       "../../etc/passwd",
	} {
		label := "passes/" + name
		_, err := execTool(t, repo, ToolDiagnostics, map[string]any{"scope": "all", "passes": passes})
		if err == nil {
			t.Errorf("%s: passes %q was accepted; an unknown pass must never be silently dropped", label, passes)
			continue
		}
		if !strings.Contains(err.Error(), "unknown vet pass") {
			t.Errorf("%s: error %q does not name the argument", label, err)
		}
		assertTeachingError(t, label, err)
	}

	// And the ones that must work, so the refusal above is not just strictness.
	for name, passes := range map[string]string{
		"all":        "all",
		"all_spaced": " all ",
		"all_dup":    "all,all",
		"one":        "printf",
		"several":    "printf, unreachable, nilfunc",
		"duplicated": "printf,printf",
		"empty":      "",
	} {
		if _, err := execTool(t, repo, ToolDiagnostics, map[string]any{"scope": "all", "passes": passes}); err != nil {
			t.Errorf("passes/%s (%q) was refused: %v", name, passes, err)
		}
	}
}

// TestSystem_GoIntel_UnknownArgumentNamesAreBounded pins: a hostile argument name is refused and clamped, not just its value.
func TestSystem_GoIntel_UnknownArgumentNamesAreBounded(t *testing.T) {
	root := newFixture(t, "fixture")
	repo := NewTools(newTestIndex(t, root))

	_, err := execTool(t, repo, ToolDefinition, map[string]any{
		"symbol":                 "shapes.Rect",
		hostileStrings["huge"]:   1,
		"line\x00\u202ebreaking": 2,
	})
	if err == nil {
		t.Fatal("unknown argument names were accepted")
	}
	assertTeachingError(t, "unknown-args", err)
	if !strings.Contains(err.Error(), "unknown argument(s)") {
		t.Errorf("error %q lost the local_fs unknown-argument voice", err)
	}
}

// TestSystem_GoIntel_HostileTargetsAreRefusedNotObeyed pins the same contract as symbol arguments for go_symbols' and go_diagnostics' target.
func TestSystem_GoIntel_HostileTargetsAreRefusedNotObeyed(t *testing.T) {
	root := newFixture(t, "fixture")
	repo := NewTools(newTestIndex(t, root))

	for name, value := range hostileStrings {
		for _, tc := range []struct {
			tool string
			args map[string]any
		}{
			{ToolSymbols, map[string]any{"target": value}},
			{ToolDiagnostics, map[string]any{"scope": "package", "target": value}},
		} {
			label := tc.tool + "/target/" + name
			out, err := execTool(t, repo, tc.tool, tc.args)
			if err != nil {
				assertTeachingError(t, label, err)
				continue
			}
			assertResultStaysInWorkspace(t, label, root, out)
		}
	}
}

func shownOf(t *testing.T, out any) int {
	t.Helper()
	switch v := out.(type) {
	case *ReferencesResult:
		return v.Shown
	case *SymbolsResult:
		return v.Shown
	case *DiagnosticsResult:
		return v.Shown
	}
	t.Fatalf("unexpected result type %T", out)
	return 0
}

// assertResultStaysInWorkspace fails when any location in a result is absolute or starts with "..", since every gointel anchor is workspace-relative.
func assertResultStaysInWorkspace(t *testing.T, label, root string, out any) {
	t.Helper()
	var locations []string
	switch v := out.(type) {
	case *DefinitionResult:
		locations = append(locations, v.Location)
	case *DescribeResult:
		locations = append(locations, v.Location)
		for _, m := range v.Fields {
			locations = append(locations, m.Location)
		}
		for _, m := range v.Methods {
			locations = append(locations, m.Location)
		}
	case *ReferencesResult:
		locations = append(locations, v.Definition)
		for _, f := range v.Files {
			locations = append(locations, f.File)
		}
	case *ImplementationsResult:
		for _, e := range append(append([]ImplEntry{}, v.Implementers...), v.Interfaces...) {
			locations = append(locations, e.Location)
		}
	case *SymbolsResult:
		for _, s := range v.Symbols {
			locations = append(locations, s.Location)
		}
	case *DiagnosticsResult:
		for _, d := range v.Diagnostics {
			locations = append(locations, d.Location)
		}
	}
	for _, loc := range locations {
		if loc == "" {
			continue
		}
		if filepath.IsAbs(loc) || strings.HasPrefix(loc, "..") {
			t.Errorf("%s: result names %q, which is not inside the workspace %s", label, loc, root)
		}
	}
}

// TestSystem_GoIntel_ConcurrentQueriesEditsAndInvalidationsDoNotRace pins: concurrent queries, invalidation, source rewrites and Shutdown never race (run under -race) or deadlock.
func TestSystem_GoIntel_ConcurrentQueriesEditsAndInvalidationsDoNotRace(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := newTestIndex(t, root)
	repo := NewTools(ix)
	ctx := context.Background()

	// Warm one snapshot so queries exercise both the cached and rebuild paths.
	if _, err := ix.Definition(ctx, Request{Symbol: "shapes.Rect"}); err != nil {
		t.Fatalf("warm-up: %v", err)
	}

	calls := []struct {
		tool string
		args map[string]any
	}{
		{ToolDefinition, map[string]any{"symbol": "shapes.Rect"}},
		{ToolDescribe, map[string]any{"symbol": "shapes.Shape"}},
		{ToolReferences, map[string]any{"symbol": "shapes.Unit", "max": 10}},
		{ToolImplementations, map[string]any{"symbol": "shapes.Shape"}},
		{ToolSymbols, map[string]any{"target": "report"}},
		{ToolDiagnostics, map[string]any{"scope": "changed"}},
		{ToolDefinition, map[string]any{"symbol": "does.NotExist"}},
		{ToolDefinition, map[string]any{"symbol": "shapes.Rect", "dir": "../.."}},
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				c := calls[(seed+n)%len(calls)]
				out, _, err := repo.Exec(ctx, time.Now(), c.args, false,
					&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: c.tool})
				if err != nil && !isExpectedConcurrentError(err) {
					t.Errorf("%s: unexpected error %v", c.tool, err)
					return
				}
				_ = out
			}
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			ix.Invalidate(filepath.Join(root, "shapes", "shapes.go"), filepath.Join(root, "go.mod"))
			time.Sleep(time.Millisecond)
		}
	}()

	// Writer: rename rather than truncate-and-write so `go list` never reads a half-written file.
	wg.Add(1)
	go func() {
		defer wg.Done()
		base, err := os.ReadFile(filepath.Join(root, "shapes", "shapes.go"))
		if err != nil {
			t.Errorf("read fixture: %v", err)
			return
		}
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			body := fmt.Sprintf("%s\n// Churn%d exists only to move the file.\nfunc Churn%d() float64 { return Unit }\n", base, n, n)
			atomicWrite(t, filepath.Join(root, "shapes", "shapes.go"), body)
			time.Sleep(5 * time.Millisecond)
		}
	}()

	time.Sleep(750 * time.Millisecond)

	// Teardown mid-query: Shutdown must return promptly even with readers in flight and a rebuild possibly running.
	done := make(chan struct{})
	shutdownStart := time.Now()
	go func() {
		ix.Shutdown()
		close(done)
	}()
	select {
	case <-done:
		t.Logf("Shutdown under load returned in %v", time.Since(shutdownStart))
	case <-time.After(30 * time.Second):
		t.Fatal("Shutdown did not return within 30s under concurrent load — teardown is not bounded")
	}

	close(stop)
	wgDone := make(chan struct{})
	go func() { wg.Wait(); close(wgDone) }()
	select {
	case <-wgDone:
	case <-time.After(30 * time.Second):
		t.Fatal("workers did not drain within 30s after Shutdown")
	}
}

// isExpectedConcurrentError accepts the refusals the storm legitimately produces, including a transient load failure from a mid-rewrite source file.
func isExpectedConcurrentError(err error) bool {
	switch {
	case errors.Is(err, ErrNotFound),
		errors.Is(err, ErrAmbiguous),
		errors.Is(err, ErrOutsideAllowedDir),
		errors.Is(err, ErrShutdown),
		errors.Is(err, ErrLoad):
		return true
	}
	return false
}

func atomicWrite(t *testing.T, path, body string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		t.Errorf("write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Errorf("rename %s: %v", tmp, err)
	}
}

// TestSystem_GoIntel_QueriesAfterShutdownFailTypedRatherThanRebuild pins: a query after Shutdown returns ErrShutdown rather than rebuilding.
func TestSystem_GoIntel_QueriesAfterShutdownFailTypedRatherThanRebuild(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := NewIndex(Config{AllowedDir: root}).(*index)
	repo := NewTools(ix)
	ctx := context.Background()

	if _, err := ix.Definition(ctx, Request{Symbol: "shapes.Rect"}); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	ix.Shutdown()

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{ToolDefinition, map[string]any{"symbol": "shapes.Rect"}},
		{ToolDescribe, map[string]any{"symbol": "shapes.Rect"}},
		{ToolReferences, map[string]any{"symbol": "shapes.Unit"}},
		{ToolImplementations, map[string]any{"symbol": "shapes.Shape"}},
		{ToolSymbols, map[string]any{"target": "shapes"}},
		{ToolDiagnostics, map[string]any{"scope": "all"}},
	} {
		done := make(chan error, 1)
		go func() {
			_, _, err := repo.Exec(ctx, time.Now(), tc.args, false,
				&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: tc.tool})
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrShutdown) {
				t.Errorf("%s after Shutdown: error = %v, want ErrShutdown", tc.tool, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s after Shutdown HUNG — a closed index must refuse, not block", tc.tool)
		}
	}

	// And nothing was resurrected behind the refusal.
	ix.mu.Lock()
	n := len(ix.entries)
	ix.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d cache entries after Shutdown + queries — a late query rebuilt into a cache nothing will reap", n)
	}

	// Invalidate after Shutdown is a no-op, not a panic.
	ix.Invalidate(filepath.Join(root, "shapes", "shapes.go"))
	ix.Shutdown()
}

// TestSystem_GoIntel_FreshnessThroughTheToolPath pins: with no Invalidate call, the mtime sweep alone catches an edit, a brand-new file, and a deletion.
func TestSystem_GoIntel_FreshnessThroughTheToolPath(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := newTestIndex(t, root)
	repo := NewTools(ix)

	if _, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": "shapes.Rect"}); err != nil {
		t.Fatalf("warm-up: %v", err)
	}

	shapesFile := filepath.Join(root, "shapes", "shapes.go")
	original, err := os.ReadFile(shapesFile)
	if err != nil {
		t.Fatal(err)
	}

	// (1) An edit to an existing file, no chtimes bump.
	if err := os.WriteFile(shapesFile, append(original, []byte(
		"\n// Added lands via a plain write, the way write_file lands one.\nfunc Added() float64 { return Unit }\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": "shapes.Added"})
	if err != nil {
		t.Fatalf("go_definition immediately after a write: %v (the mtime sweep did not see the edit)", err)
	}
	if loc := res.(*DefinitionResult).Location; !strings.HasPrefix(loc, "shapes/shapes.go:") {
		t.Errorf("location = %q", loc)
	}

	// go_references must see the new call site through the same sweep.
	reportFile := filepath.Join(root, "report", "report.go")
	reportSrc, err := os.ReadFile(reportFile)
	if err != nil {
		t.Fatal(err)
	}
	before, err := execTool(t, repo, ToolReferences, map[string]any{"symbol": "shapes.Unit"})
	if err != nil {
		t.Fatalf("references baseline: %v", err)
	}
	baseline := before.(*ReferencesResult).Uses

	if err := os.WriteFile(reportFile, append(reportSrc, []byte(
		"\n// UsesUnit is a call site added after the snapshot was built.\nfunc UsesUnit() float64 { return shapes.Unit + shapes.Unit }\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := execTool(t, repo, ToolReferences, map[string]any{"symbol": "shapes.Unit"})
	if err != nil {
		t.Fatalf("references after a write: %v", err)
	}
	if got := after.(*ReferencesResult).Uses; got != baseline+2 {
		t.Errorf("uses = %d, want %d — a stale snapshot would still say %d", got, baseline+2, baseline)
	}

	// (2) A brand-new file in an existing package, invisible to per-file stat.
	newFile := filepath.Join(root, "shapes", "brandnew.go")
	if err := os.WriteFile(newFile, []byte(
		"package shapes\n\n// BrandNew arrived in a file that did not exist.\nfunc BrandNew() float64 { return Unit }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": "shapes.BrandNew"}); err != nil {
		t.Fatalf("go_definition after a NEW file appeared: %v", err)
	}

	// go_diagnostics must report the changed package without being told which.
	diag, err := execTool(t, repo, ToolDiagnostics, map[string]any{"scope": "changed"})
	if err != nil {
		t.Fatalf("go_diagnostics scope=changed: %v", err)
	}
	if pkgs := diag.(*DiagnosticsResult).Packages; len(pkgs) == 0 {
		t.Errorf("scope=changed saw no packages after three writes; note = %q", diag.(*DiagnosticsResult).Note)
	}

	// (3) A deletion.
	if err := os.Remove(newFile); err != nil {
		t.Fatal(err)
	}
	if _, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": "shapes.BrandNew"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("after deleting the file, go_definition = %v, want ErrNotFound", err)
	}
}

// TestSystem_GoIntel_DiagnosticsSeeAFreshlyBrokenPackage pins: breaking a file and querying immediately surfaces the new type error.
func TestSystem_GoIntel_DiagnosticsSeeAFreshlyBrokenPackage(t *testing.T) {
	root := newFixture(t, "fixture")
	repo := NewTools(newTestIndex(t, root))

	clean, err := execTool(t, repo, ToolDiagnostics, map[string]any{"scope": "all"})
	if err != nil {
		t.Fatalf("baseline diagnostics: %v", err)
	}
	if got := clean.(*DiagnosticsResult).TypeErrors; got != 0 {
		t.Fatalf("the fixture starts with %d type errors", got)
	}

	if err := os.WriteFile(filepath.Join(root, "report", "report.go"),
		[]byte("package report\n\nfunc Broken() int { return \"not an int\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := execTool(t, repo, ToolDiagnostics, map[string]any{"scope": "all"})
	if err != nil {
		t.Fatalf("diagnostics after breaking a file: %v", err)
	}
	res := got.(*DiagnosticsResult)
	if res.TypeErrors == 0 {
		t.Fatalf("diagnostics reported no type errors after the package was broken: %+v", res)
	}
	found := false
	for _, d := range res.Diagnostics {
		if strings.HasPrefix(d.Location, "report/report.go:") && d.Severity == "type-error" {
			found = true
		}
	}
	if !found {
		t.Errorf("no type error anchored in the file that was just broken: %+v", res.Diagnostics)
	}
}

// TestSystem_GoIntel_BrokenPackageDoesNotBlindTheHealthyOnes pins: one non-compiling package does not stop queries about healthy siblings.
func TestSystem_GoIntel_BrokenPackageDoesNotBlindTheHealthyOnes(t *testing.T) {
	root := writeModule(t, t.TempDir(), "example.com/mixed", map[string]string{
		"healthy/healthy.go": "package healthy\n\n// Good is fine.\ntype Good struct{ N int }\n\n// Use returns N.\nfunc Use(g Good) int { return g.N }\n",
		"broken/broken.go":   "package broken\n\nfunc Broken() int { return \n",
	})
	repo := NewTools(newTestIndex(t, root))

	res, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": "healthy.Good"})
	if err != nil {
		t.Fatalf("a query about a HEALTHY package failed because a sibling is broken: %v", err)
	}
	if loc := res.(*DefinitionResult).Location; !strings.HasPrefix(loc, "healthy/healthy.go:") {
		t.Errorf("location = %q", loc)
	}
	if _, err := execTool(t, repo, ToolReferences, map[string]any{"symbol": "healthy.Good"}); err != nil {
		t.Errorf("go_references on a healthy package: %v", err)
	}

	diag, err := execTool(t, repo, ToolDiagnostics, map[string]any{"scope": "all"})
	if err != nil {
		t.Fatalf("go_diagnostics scope=all: %v", err)
	}
	d := diag.(*DiagnosticsResult)
	if d.TypeErrors == 0 {
		t.Fatalf("diagnostics did not report the broken package: %+v", d)
	}
	named := false
	for _, entry := range d.Diagnostics {
		if strings.Contains(entry.Location, "broken/broken.go") {
			named = true
		}
	}
	if !named {
		t.Errorf("diagnostics never names the broken file: %+v", d.Diagnostics)
	}
}

// TestSystem_GoIntel_MissingDependencyAnswersPromptlyAndNamesTheRealCause pins: an unresolvable import answers promptly, resolvable symbols still answer, and go_diagnostics names go.mod as the cause rather than the import line.
func TestSystem_GoIntel_MissingDependencyAnswersPromptlyAndNamesTheRealCause(t *testing.T) {
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOPROXY", "off")

	const missing = "example.com/definitely/not/a/real/module"
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/missingdep\n\ngo 1.21\n\nrequire "+missing+" v1.2.3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"),
		[]byte("package main\n\nimport _ \""+missing+"\"\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := NewTools(newTestIndex(t, root))

	type result struct {
		out any
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		out, err := execTool(t, repo, ToolDiagnostics, map[string]any{"scope": "all"})
		done <- result{out, err}
	}()

	var got result
	select {
	case got = <-done:
		t.Logf("diagnostics over a module with an unresolvable dependency answered in %v", time.Since(start))
	case <-time.After(60 * time.Second):
		t.Fatal("a module with an unresolvable dependency HUNG — the failure must be prompt")
	}
	if got.err != nil {
		assertTeachingError(t, "missing-dep", got.err)
		return
	}

	res := got.out.(*DiagnosticsResult)
	if res.TypeErrors == 0 {
		t.Fatalf("diagnostics reported nothing about the unresolvable dependency: %+v", res)
	}
	named, explained := false, false
	for _, d := range res.Diagnostics {
		if strings.Contains(d.Message, missing) {
			named = true
			if strings.Contains(d.Message, "go.mod") {
				explained = true
			}
		}
	}
	if !named {
		t.Errorf("no diagnostic names the unresolvable dependency: %+v", res.Diagnostics)
	}
	if !explained {
		t.Errorf("the diagnostic points at the import line without saying the cause is module resolution — an agent will 'fix' correct source: %+v", res.Diagnostics)
	}

	// The module's own, resolvable symbol still answers.
	if _, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": "main.main"}); err != nil {
		t.Errorf("a symbol that DID resolve was refused because a dependency is missing: %v", err)
	}
}

// TestSystem_GoIntel_EmptyModuleAndNonGoDirectoryAreTeachingErrors pins: an empty module and a directory with no go.mod both return ErrNoModule.
func TestSystem_GoIntel_EmptyModuleAndNonGoDirectoryAreTeachingErrors(t *testing.T) {
	t.Run("empty module", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/empty\n\ngo 1.21\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		repo := NewTools(newTestIndex(t, root))
		_, err := execTool(t, repo, ToolSymbols, map[string]any{})
		if !errors.Is(err, ErrNoModule) {
			t.Fatalf("error = %v, want ErrNoModule", err)
		}
		if !strings.Contains(err.Error(), "no Go packages") {
			t.Errorf("error %q does not say what is missing", err)
		}
		assertTeachingError(t, "empty-module", err)
	})

	t.Run("non-Go directory", func(t *testing.T) {
		root := t.TempDir()
		for _, name := range []string{"README.md", "package.json", "src/index.ts"} {
			path := filepath.Join(root, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		repo := NewTools(newTestIndex(t, root))
		_, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": "Anything"})
		if !errors.Is(err, ErrNoModule) {
			t.Fatalf("error = %v, want ErrNoModule", err)
		}
		assertTeachingError(t, "non-go-dir", err)
	})
}

// TestSystem_GoIntel_NestedModuleResolvesToTheInnermostRoot pins: a go.mod inside a go.mod resolves to the innermost module containing the query dir.
func TestSystem_GoIntel_NestedModuleResolvesToTheInnermostRoot(t *testing.T) {
	root := writeModule(t, t.TempDir(), "example.com/outer", map[string]string{
		"outerpkg/outer.go": "package outerpkg\n\n// OuterOnly lives in the outer module.\nconst OuterOnly = 1\n",
	})
	inner := writeModule(t, filepath.Join(root, "tools"), "example.com/inner", map[string]string{
		"innerpkg/inner.go": "package innerpkg\n\n// InnerOnly lives in the inner module.\nconst InnerOnly = 2\n",
	})
	_ = inner

	ix := newTestIndex(t, root)
	repo := NewTools(ix)

	// From the workspace root: the outer module.
	out, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": "outerpkg.OuterOnly"})
	if err != nil {
		t.Fatalf("outer symbol from the workspace root: %v", err)
	}
	if got := out.(*DefinitionResult).Module; got != "example.com/outer" {
		t.Errorf("module = %q, want example.com/outer", got)
	}
	if _, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": "innerpkg.InnerOnly"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("the inner module's symbol resolved from the outer root: %v", err)
	}

	// From inside the nested module: the inner one, innermost-wins.
	out, err = execTool(t, repo, ToolDefinition, map[string]any{"symbol": "innerpkg.InnerOnly", "dir": "tools/innerpkg"})
	if err != nil {
		t.Fatalf("inner symbol with dir inside the nested module: %v", err)
	}
	if got := out.(*DefinitionResult).Module; got != "example.com/inner" {
		t.Errorf("module = %q, want example.com/inner — the walk-up must stop at the innermost go.mod", got)
	}
	if _, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": "outerpkg.OuterOnly", "dir": "tools"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("the outer module's symbol resolved from inside the nested one: %v", err)
	}

	// Both roots are cached independently, and the LRU bound still holds.
	ix.mu.Lock()
	n := len(ix.entries)
	ix.mu.Unlock()
	if n > defaultMaxRoots {
		t.Errorf("cached %d roots, above the %d bound", n, defaultMaxRoots)
	}
}

// TestSystem_GoIntel_ThisRepoExcludesTestFiles pins: with Tests:false, a _test.go declaration does not exist to any query against this repository.
func TestSystem_GoIntel_ThisRepoExcludesTestFiles(t *testing.T) {
	ix := repoIndex(t)
	repo := NewTools(ix)

	// Declared only in _test.go files of this repository.
	for _, symbol := range []string{
		"contenoxcli.TestReadinessDefaults",
		"gointel.TestUnit_Tools_SchemaShape",
		"gointel.newTestTools",
	} {
		if _, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": symbol}); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s resolved, but tests are excluded from the build context: err = %v", symbol, err)
		}
	}

	// The non-test declaration right next to them still resolves: the exclusion is about test files, not the package.
	if _, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": "gointel.NewTools"}); err != nil {
		t.Errorf("a non-test symbol in the same package did not resolve: %v", err)
	}

	// And a references answer over the repo counts no test-file call sites.
	out, err := execTool(t, repo, ToolReferences, map[string]any{"symbol": "gointel.ToolsProviderName", "max": 200})
	if err != nil {
		t.Fatalf("go_references: %v", err)
	}
	for _, f := range out.(*ReferencesResult).Files {
		if strings.HasSuffix(f.File, "_test.go") {
			t.Errorf("references names the test file %s despite tests being excluded", f.File)
		}
	}
}

// TestSystem_GoIntel_WarmQueryBudgetsOnThisRepo is a regression fence, not a benchmark: budgets sit orders of magnitude above measured warm-query cost, so only a cache-missing regression can blow them.
func TestSystem_GoIntel_WarmQueryBudgetsOnThisRepo(t *testing.T) {
	ix := repoIndex(t)
	repo := NewTools(ix)

	cold := time.Now()
	if _, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": "frame.StyleBrand"}); err != nil {
		t.Fatalf("cold definition: %v", err)
	}
	t.Logf("cold go_definition (packages.Load of this repo): %v", time.Since(cold))

	for _, tc := range []struct {
		name   string
		tool   string
		args   map[string]any
		budget time.Duration
	}{
		{"warm definition", ToolDefinition, map[string]any{"symbol": "frame.StyleBrand"}, 50 * time.Millisecond},
		{"warm describe", ToolDescribe, map[string]any{"symbol": "frame.StyleID"}, 50 * time.Millisecond},
		{"references across the repo", ToolReferences, map[string]any{"symbol": "frame.StyleBrand", "max": 200}, 500 * time.Millisecond},
		{"diagnostics scope=all", ToolDiagnostics, map[string]any{"scope": "all"}, 2 * time.Second},
	} {
		start := time.Now()
		if _, err := execTool(t, repo, tc.tool, tc.args); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		took := time.Since(start)
		t.Logf("%-28s %v (budget %v)", tc.name, took, tc.budget)
		if took > tc.budget {
			t.Errorf("%s took %v, over the %v budget — the most likely cause is a cold load per query", tc.name, took, tc.budget)
		}
	}
}

// TestSystem_GoIntel_OneSnapshotPerRootAcrossManyQueries pins: fifty queries against an unchanged module produce exactly one packages.Load and one entry.
func TestSystem_GoIntel_OneSnapshotPerRootAcrossManyQueries(t *testing.T) {
	ix := repoIndex(t)
	repo := NewTools(ix)
	root := repoRootDir(t)

	if _, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": "frame.StyleBrand"}); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	e := ix.testEntry(t, root)
	builds := e.builds.Load()
	snap := e.snap.Load()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	calls := []struct {
		tool string
		args map[string]any
	}{
		{ToolDefinition, map[string]any{"symbol": "frame.StyleBrand"}},
		{ToolDescribe, map[string]any{"symbol": "frame.StyleID"}},
		{ToolSymbols, map[string]any{"target": "frame", "max": 50}},
		{ToolReferences, map[string]any{"symbol": "frame.StyleBrand", "max": 20}},
		{ToolDefinition, map[string]any{"symbol": "gointel.NewTools"}},
	}
	for i := 0; i < 50; i++ {
		c := calls[i%len(calls)]
		if _, err := execTool(t, repo, c.tool, c.args); err != nil {
			t.Fatalf("query %d (%s): %v", i, c.tool, err)
		}
	}

	if got := e.builds.Load(); got != builds {
		t.Errorf("packages.Load ran %d extra times across 50 queries on an unchanged module", got-builds)
	}
	if e.snap.Load() != snap {
		t.Error("the snapshot pointer changed across 50 queries on an unchanged module")
	}
	ix.mu.Lock()
	entries := len(ix.entries)
	ix.mu.Unlock()
	if entries != 1 {
		t.Errorf("%d cache entries for one module root, want 1", entries)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("heap growth across 50 warm queries: %.1f MB", float64(growth)/(1<<20))
	// Bound is far above per-query allocation noise; an extra retained snapshot would blow it.
	if growth > 64<<20 {
		t.Errorf("heap grew %.1f MB across 50 warm queries — a snapshot is being retained per query", float64(growth)/(1<<20))
	}
}
