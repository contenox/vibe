package gointel

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func copyFixtureInto(t *testing.T, name, dst string) string {
	t.Helper()
	src := filepath.Join("testdata", name)
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if filepath.Base(rel) == "go.mod.txt" {
			target = filepath.Join(filepath.Dir(target), "go.mod")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copy fixture %s: %v", name, err)
	}
	return dst
}

func newFixture(t *testing.T, name string) string {
	t.Helper()
	return copyFixtureInto(t, name, t.TempDir())
}

func newTestIndex(t *testing.T, root string) *index {
	t.Helper()
	ix := NewIndex(Config{AllowedDir: root}).(*index)
	t.Cleanup(ix.Shutdown)
	return ix
}

func (ix *index) testEntry(t *testing.T, root string) *entry {
	t.Helper()
	ix.mu.Lock()
	defer ix.mu.Unlock()
	e, ok := ix.entries[root]
	if !ok {
		t.Fatalf("no cache entry for %s (have %v)", root, ix.order)
	}
	return e
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func TestUnit_Loader_BuildsOnceAndServesFromCache(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := newTestIndex(t, root)
	ctx := context.Background()

	cold := time.Now()
	if _, err := ix.Definition(ctx, Request{Symbol: "shapes.Rect"}); err != nil {
		t.Fatalf("first query: %v", err)
	}
	coldDur := time.Since(cold)

	e := ix.testEntry(t, root)
	first := e.snap.Load()
	if first == nil {
		t.Fatal("no snapshot cached after the first query")
	}
	if got := e.builds.Load(); got != 1 {
		t.Fatalf("builds after first query = %d, want 1", got)
	}

	warm := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := ix.Definition(ctx, Request{Symbol: "shapes.Rect"}); err != nil {
			t.Fatalf("warm query %d: %v", i, err)
		}
	}
	warmDur := time.Since(warm) / 5

	if got := e.builds.Load(); got != 1 {
		t.Fatalf("builds after 5 warm queries = %d, want 1 (cache miss on an unchanged module)", got)
	}
	if e.snap.Load() != first {
		t.Fatal("warm query replaced the snapshot on an unchanged module")
	}
	if first.ModulePath != "example.com/fixture" {
		t.Fatalf("ModulePath = %q, want example.com/fixture", first.ModulePath)
	}
	if len(first.pkgs) != 2 {
		t.Fatalf("loaded %d packages, want 2", len(first.pkgs))
	}
	t.Logf("fixture cold=%v (packages.Load %v) warm=%v/query", coldDur, first.BuildDuration, warmDur)
}

func TestUnit_Loader_ToolchainViewNamesTheBuildContext(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := newTestIndex(t, root)

	res, err := ix.Definition(context.Background(), Request{Symbol: "shapes.Rect"})
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	for _, want := range []string{runtime.GOOS, runtime.GOARCH, "tests excluded", "advisory", "go build"} {
		if !strings.Contains(res.Toolchain, want) {
			t.Errorf("toolchain %q missing %q", res.Toolchain, want)
		}
	}
	if !strings.HasPrefix(res.Toolchain, "go1.") {
		t.Errorf("toolchain %q does not start with the go version", res.Toolchain)
	}
}

func TestUnit_Loader_LRUEvictsBeyondTwoRoots(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"m1", "m2", "m3"} {
		copyFixtureInto(t, "fixture", filepath.Join(base, name))
	}
	ix := newTestIndex(t, base)
	ctx := context.Background()

	for _, name := range []string{"m1", "m2", "m3"} {
		if _, err := ix.Definition(ctx, Request{Dir: name, Symbol: "shapes.Rect"}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	ix.mu.Lock()
	got := len(ix.entries)
	_, hasM1 := ix.entries[filepath.Join(base, "m1")]
	_, hasM3 := ix.entries[filepath.Join(base, "m3")]
	ix.mu.Unlock()

	if got != defaultMaxRoots {
		t.Fatalf("cached %d roots, want %d", got, defaultMaxRoots)
	}
	if hasM1 {
		t.Error("least-recently-used root m1 was not evicted")
	}
	if !hasM3 {
		t.Error("most-recently-used root m3 was evicted")
	}
}

func TestUnit_ModuleRoot_RefusesDirEscapingAllowedDir(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := newTestIndex(t, root)

	_, err := ix.Definition(context.Background(), Request{Dir: "../..", Symbol: "shapes.Rect"})
	if err == nil {
		t.Fatal("a dir outside the allowed directory was accepted")
	}
	if !strings.Contains(err.Error(), "escapes allowed directory") {
		t.Fatalf("error %q does not use the local_fs containment voice", err)
	}
}

func TestUnit_ModuleRoot_RefusesModuleRootAboveAllowedDir(t *testing.T) {
	module := newFixture(t, "fixture")
	ix := newTestIndex(t, filepath.Join(module, "shapes"))

	_, err := ix.Definition(context.Background(), Request{Symbol: "shapes.Rect"})
	if !errors.Is(err, ErrOutsideAllowedDir) {
		t.Fatalf("error = %v, want ErrOutsideAllowedDir", err)
	}
	if !strings.Contains(err.Error(), "outside the allowed directory") {
		t.Fatalf("error %q does not name the boundary", err)
	}
}

func TestUnit_ModuleRoot_NoModuleIsATeachingError(t *testing.T) {
	ix := newTestIndex(t, t.TempDir())

	_, err := ix.Definition(context.Background(), Request{Symbol: "Anything"})
	if !errors.Is(err, ErrNoModule) {
		t.Fatalf("error = %v, want ErrNoModule", err)
	}
	if !strings.Contains(err.Error(), "go.mod") {
		t.Fatalf("error %q does not say what is missing", err)
	}
}

func TestUnit_ModuleRoot_ResolvesFromASubdirectory(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := newTestIndex(t, root)

	if _, err := ix.Definition(context.Background(), Request{Dir: "report", Symbol: "shapes.Rect"}); err != nil {
		t.Fatalf("definition from subdirectory: %v", err)
	}
	ix.mu.Lock()
	_, ok := ix.entries[root]
	ix.mu.Unlock()
	if !ok {
		t.Fatalf("module root was not resolved to %s", root)
	}
}

func TestUnit_ModuleRoot_ResolvesFromAFilePath(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := newTestIndex(t, root)

	if _, err := ix.Definition(context.Background(), Request{Dir: "report/report.go", Symbol: "shapes.Rect"}); err != nil {
		t.Fatalf("definition from file path: %v", err)
	}
}

// TestUnit_Config_RequiresAnAllowedDir pins: a missing workspace root refuses with a fatal marker naming both ways the root can be supplied.
func TestUnit_Config_RequiresAnAllowedDir(t *testing.T) {
	ix := NewIndex(Config{})
	t.Cleanup(ix.Shutdown)

	_, err := ix.Definition(context.Background(), Request{Symbol: "X"})
	if err == nil {
		t.Fatal("an index with no workspace root answered a query")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no workspace root") {
		t.Fatalf("error = %v, want a no-workspace-root refusal", err)
	}
	if !strings.Contains(msg, severityFatalToken) {
		t.Errorf("error %q is not marked fatal, but no retry can fix a wiring gap", msg)
	}
	for _, want := range []string{"--local-exec-allowed-dir", "cwd resolver"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %q as a way to supply the root", msg, want)
		}
	}
}

func TestUnit_Config_CwdResolverSuppliesTheWorkspaceRoot(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := NewIndex(Config{CwdResolver: func(context.Context) string { return root }})
	t.Cleanup(ix.Shutdown)

	if _, err := ix.Definition(context.Background(), Request{Symbol: "shapes.Rect"}); err != nil {
		t.Fatalf("definition via CwdResolver: %v", err)
	}
}

const addedSymbol = `
// Added is a symbol that did not exist when the snapshot was built.
func Added() float64 { return Unit }
`

func TestUnit_Invalidate_QueryAfterInvalidateSeesTheEdit(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := newTestIndex(t, root)
	ctx := context.Background()
	file := filepath.Join(root, "shapes", "shapes.go")

	if _, err := ix.Definition(ctx, Request{Symbol: "shapes.Rect"}); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	if _, err := ix.Definition(ctx, Request{Symbol: "shapes.Added"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pre-edit lookup = %v, want ErrNotFound", err)
	}

	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, file, string(original)+addedSymbol)
	ix.Invalidate(file)

	res, err := ix.Definition(ctx, Request{Symbol: "shapes.Added"})
	if err != nil {
		t.Fatalf("post-Invalidate lookup: %v", err)
	}
	if res.Kind != "func" {
		t.Fatalf("kind = %q, want func", res.Kind)
	}
	if !strings.HasPrefix(res.Location, "shapes/shapes.go:") {
		t.Fatalf("location = %q, want shapes/shapes.go:...", res.Location)
	}
}

func TestUnit_Invalidate_MtimeSweepCatchesAnUnannouncedEdit(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := newTestIndex(t, root)
	ctx := context.Background()
	file := filepath.Join(root, "shapes", "shapes.go")

	if _, err := ix.Definition(ctx, Request{Symbol: "shapes.Rect"}); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	builds := ix.testEntry(t, root).builds.Load()

	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, file, string(original)+addedSymbol)

	if _, err := ix.Definition(ctx, Request{Symbol: "shapes.Added"}); err != nil {
		t.Fatalf("lookup after an unannounced edit: %v", err)
	}
	if got := ix.testEntry(t, root).builds.Load(); got != builds+1 {
		t.Fatalf("builds = %d, want %d (the sweep should have forced exactly one rebuild)", got, builds+1)
	}
}

func TestUnit_Invalidate_MtimeSweepCatchesANewFileInAnExistingPackage(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := newTestIndex(t, root)
	ctx := context.Background()

	if _, err := ix.Definition(ctx, Request{Symbol: "shapes.Rect"}); err != nil {
		t.Fatalf("warm-up: %v", err)
	}

	// A brand-new file only moves the package directory's mtime, not any file's; no Invalidate call needed.
	writeFixtureFile(t, filepath.Join(root, "shapes", "extra.go"),
		"package shapes\n\n// Extra was added after the snapshot was built.\nfunc Extra() float64 { return Unit }\n")

	if _, err := ix.Definition(ctx, Request{Symbol: "shapes.Extra"}); err != nil {
		t.Fatalf("lookup after a new file appeared: %v", err)
	}
}

func TestUnit_Invalidate_RemovedSymbolStopsResolving(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := newTestIndex(t, root)
	ctx := context.Background()
	file := filepath.Join(root, "report", "report.go")

	if _, err := ix.Definition(ctx, Request{Symbol: "report.Doubled"}); err != nil {
		t.Fatalf("pre-edit lookup: %v", err)
	}

	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.Split(string(original), "// Doubled scales Default by two units.")[0]
	writeFixtureFile(t, file, trimmed)
	ix.Invalidate(file)

	if _, err := ix.Definition(ctx, Request{Symbol: "report.Doubled"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("post-delete lookup = %v, want ErrNotFound", err)
	}
}

func TestUnit_Invalidate_ReferencesRebuiltOnAnUnannouncedEdit(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := newTestIndex(t, root)
	ctx := context.Background()

	before, err := ix.References(ctx, Request{Symbol: "shapes.Scale"})
	if err != nil {
		t.Fatalf("references: %v", err)
	}
	if before.Uses != 1 {
		t.Fatalf("baseline uses = %d, want 1", before.Uses)
	}

	file := filepath.Join(root, "report", "report.go")
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, file, string(original)+
		"\n// Tripled is a second call site added after the snapshot was built.\nfunc Tripled() shapes.Rect {\n\treturn shapes.Scale(Default, 3)\n}\n")

	after, err := ix.References(ctx, Request{Symbol: "shapes.Scale"})
	if err != nil {
		t.Fatalf("references after edit: %v", err)
	}
	if after.Uses != 2 {
		t.Fatalf("uses after edit = %d, want 2 (a stale snapshot would still say 1)", after.Uses)
	}
}

func TestUnit_Invalidate_CoalescesAnEditBurstIntoOneRebuild(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := newTestIndex(t, root)
	ctx := context.Background()

	if _, err := ix.Definition(ctx, Request{Symbol: "shapes.Rect"}); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	e := ix.testEntry(t, root)
	base := e.builds.Load()

	for i := 0; i < 10; i++ {
		ix.Invalidate(filepath.Join(root, "shapes", "shapes.go"))
	}
	// Ten dirty marks, then a burst of concurrent queries: exactly one rebuild.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ix.Definition(ctx, Request{Symbol: "shapes.Rect"}); err != nil {
				t.Errorf("concurrent query: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := e.builds.Load(); got != base+1 {
		t.Fatalf("builds = %d, want %d — an edit burst plus a query burst must coalesce into one packages.Load", got, base+1)
	}
}

func TestUnit_Invalidate_IgnoresIrrelevantAndOutOfTreePaths(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := newTestIndex(t, root)
	ctx := context.Background()

	if _, err := ix.Definition(ctx, Request{Symbol: "shapes.Rect"}); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	e := ix.testEntry(t, root)
	snap := e.snap.Load()
	base := e.builds.Load()

	ix.Invalidate(
		filepath.Join(root, "README.md"),
		filepath.Join(root, "docs", "notes.txt"),
		filepath.Join(t.TempDir(), "elsewhere", "other.go"),
	)
	ix.Invalidate()

	if _, err := ix.Definition(ctx, Request{Symbol: "shapes.Rect"}); err != nil {
		t.Fatalf("query: %v", err)
	}
	if got := e.builds.Load(); got != base {
		t.Fatalf("builds = %d, want %d — a docs edit must not evict a warm snapshot", got, base)
	}
	if e.snap.Load() != snap {
		t.Fatal("snapshot replaced by an irrelevant write")
	}
}

func TestUnit_Invalidate_GoModRewriteForcesARebuild(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := newTestIndex(t, root)
	ctx := context.Background()

	if _, err := ix.Definition(ctx, Request{Symbol: "shapes.Rect"}); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	e := ix.testEntry(t, root)
	base := e.builds.Load()

	writeFixtureFile(t, filepath.Join(root, "go.mod"), "module example.com/fixture\n\ngo 1.21\n\n// touched\n")

	if _, err := ix.Definition(ctx, Request{Symbol: "shapes.Rect"}); err != nil {
		t.Fatalf("query after go.mod rewrite: %v", err)
	}
	if got := e.builds.Load(); got != base+1 {
		t.Fatalf("builds = %d, want %d", got, base+1)
	}
}

func TestUnit_Invalidate_ChangedSetIsBounded(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := newTestIndex(t, root)

	if _, err := ix.Definition(context.Background(), Request{Symbol: "shapes.Rect"}); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	e := ix.testEntry(t, root)
	for i := 0; i < maxChangedPaths*2; i++ {
		e.markDirty(filepath.Join(root, "shapes", "gen", "f"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+".go"))
	}
	if got := len(e.changedPaths()); got > maxChangedPaths {
		t.Fatalf("changed set holds %d paths, want at most %d", got, maxChangedPaths)
	}
}

func TestUnit_Reaper_DropsAnIdleSnapshot(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := NewIndex(Config{AllowedDir: root, IdleTimeout: time.Millisecond}).(*index)
	t.Cleanup(ix.Shutdown)

	if _, err := ix.Definition(context.Background(), Request{Symbol: "shapes.Rect"}); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	// The reap interval is clamped to a minimum of one second, so this waits past one tick rather than past IdleTimeout.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ix.mu.Lock()
		n := len(ix.entries)
		ix.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("idle snapshot was never reaped")
}

func TestUnit_Shutdown_JoinsTheReaperGoroutine(t *testing.T) {
	root := newFixture(t, "fixture")

	settle := func() int {
		for i := 0; i < 40; i++ {
			runtime.Gosched()
			time.Sleep(10 * time.Millisecond)
		}
		return runtime.NumGoroutine()
	}
	before := settle()

	const n = 20
	idx := make([]Index, 0, n)
	for i := 0; i < n; i++ {
		idx = append(idx, NewIndex(Config{AllowedDir: root}))
	}
	if got := runtime.NumGoroutine(); got < before+n {
		t.Fatalf("goroutines = %d after %d indexes, want at least %d — the reaper did not start", got, n, before+n)
	}
	for _, ix := range idx {
		ix.Shutdown()
		// Shutdown is idempotent: the stop channel is closed under a sync.Once.
		ix.Shutdown()
	}

	after := settle()
	if after >= before+n {
		t.Fatalf("goroutines = %d after shutting down %d indexes (was %d before) — Shutdown did not join", after, n, before)
	}
}

func TestUnit_Shutdown_DropsEveryCachedSnapshot(t *testing.T) {
	root := newFixture(t, "fixture")
	ix := NewIndex(Config{AllowedDir: root}).(*index)

	if _, err := ix.Definition(context.Background(), Request{Symbol: "shapes.Rect"}); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	ix.Shutdown()

	ix.mu.Lock()
	n := len(ix.entries)
	ix.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d snapshots retained after Shutdown", n)
	}
}
