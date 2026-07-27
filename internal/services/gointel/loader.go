// Package gointel is the agent's in-process Go code intelligence: a warm,
// type-checked view of the Go module under the workspace root, exposed as
// allow-tier read tools (go_describe, go_definition, go_references,
// go_implementations, go_symbols, go_diagnostics).
//
// See docs/development/blueprints/gointel.md for the ratified design. The three
// decisions that shape every file in this package:
//
//   - NAMES-FIRST. LSP is position-oriented because editors have cursors; an
//     agent has names. Every query takes a qualified symbol ("pkg.Ident",
//     "pkg.Type.Method", or a bare "Ident" resolved across the module), never a
//     byte offset. file:line only ever appears in ANSWERS.
//
//   - SNAPSHOT MODEL, HONESTLY COARSE. One immutable Snapshot per module root
//     (packages.Load "./..." with full syntax and types); queries read it
//     lock-free. There is NO package-granular incrementality — that is gopls's
//     eighty percent, and an agent that edits whole files at tool-call cadence
//     does not need it. A snapshot is replaced wholesale, never patched.
//
//   - ADVISORY, NEVER BLOCKING. The type checker compiled into this binary is
//     not necessarily the one the repo builds with, so a repo on a newer
//     toolchain can see phantom errors. Every result names the toolchain view it
//     was produced under and `go build` stays the arbiter.
//
// # The one way this design can lie
//
// A stale snapshot. Everything else this package returns is a fact derived from
// a real type-check; a stale snapshot returns a fact about source that no longer
// exists. Two mechanisms defend that boundary, and they are deliberately
// belt-and-braces:
//
//  1. Invalidate(paths...) — the PRIMARY path. The write-class tools know
//     exactly what they changed; the V1.1 engine middleware calls Invalidate
//     with those paths on every successful write to a .go/go.mod/go.sum file.
//     Marking is O(1) and never blocks the writer.
//
//  2. An mtime sweep at query time — the BACKSTOP, for edits nobody announced
//     (a human in an editor, a git checkout, a build script). go.mod/go.sum are
//     swept on every query; the files of the package a query actually landed on
//     are swept after resolution, and the query is re-run against a rebuilt
//     snapshot when they moved.
//
// Both mark the entry dirty rather than rebuilding inline, so an edit BURST
// coalesces into exactly one rebuild on the next query (see entry.rebuild).
//
// # Bounded like shellsession
//
// Lazy build, LRU of 2 module roots, 15-minute idle reaper, Shutdown joins its
// goroutines. The loop/clamp discipline mirrors shellsession.manager.reap
// deliberately: one more package with a background timer in this process should
// not mean one more shape of background timer to reason about.
package gointel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/contenox/beam/internal/services/vfs"
)

// ToolsProviderName is the tools-provider key this package registers under (the
// `name` a chain's `tools` task or a runtime allowlist refers to). Every tool it
// exposes is a pure read and is expected to sit at allow tier.
const ToolsProviderName = "gointel"

const (
	// defaultMaxRoots is the LRU bound on cached snapshots. Two is the blueprint's
	// number: a session works in one module and occasionally reaches into a
	// sibling one, and a warm snapshot of this repo retains ~110 MB.
	defaultMaxRoots = 2

	// defaultIdleTimeout drops a snapshot nothing has queried for this long. Same
	// value and same reason as shellsession's shell reaper.
	defaultIdleTimeout = 15 * time.Minute

	// maxChangedPaths bounds the per-root set of files observed to have changed,
	// which go_diagnostics scope=changed reports over. It is a diagnostic aid, not
	// a ledger: past the bound the oldest entries are dropped and the result says
	// so rather than growing without limit.
	maxChangedPaths = 512
)

// --- Errors -----------------------------------------------------------------
//
// Voice follows local_fs (internal/services/localtools/fs.go): a "gointel: "
// prefix, the concrete value that failed, and the next call that would work.
// The severity marker is localtools' fatal-vs-recoverable convention
// (internal/services/localtools/hardening.go) — every gointel failure is
// recoverable by a corrected call EXCEPT a missing Go toolchain, which no
// argument change can fix.

const (
	severityRecoverable = "(recoverable: adjust parameters and retry)"
	severityFatalToken  = "(fatal:"
)

// Sentinel errors, for errors.Is by callers that need to branch (the V1.1
// middleware treats ErrNoModule as "this workspace is not Go, stop asking").
var (
	// ErrNoModule means no go.mod was found at or above the query directory,
	// within the allowed directory.
	ErrNoModule = errors.New("gointel: no Go module")
	// ErrOutsideAllowedDir means the module root that owns the query directory
	// lies outside the allowed directory. Same boundary, same voice as local_fs.
	ErrOutsideAllowedDir = errors.New("gointel: module root outside allowed directory")
	// ErrNoGoToolchain means the `go` binary is not on PATH. gointel drives
	// `go list` through go/packages, so nothing can be answered without it.
	ErrNoGoToolchain = errors.New("gointel: no go toolchain")
	// ErrLoad means the module could not be loaded at all (driver failure).
	ErrLoad = errors.New("gointel: module load failed")
	// ErrNotFound means a named symbol resolved to nothing in this module.
	ErrNotFound = errors.New("gointel: symbol not found")
	// ErrAmbiguous means a name matched more than one declaration. The error text
	// lists the qualified candidates; re-ask with one of them.
	ErrAmbiguous = errors.New("gointel: ambiguous symbol")
	// ErrShutdown means the index has been shut down (engine.Stop ran) and will
	// answer nothing further. It is the typed answer to a query that raced the
	// engine's teardown: without it a late query would quietly rebuild a snapshot
	// — spawning `go list` and retaining ~110 MB — into a cache whose reaper has
	// already exited, so nothing would ever drop it again.
	ErrShutdown = errors.New("gointel: index shut down")
)

// maxEchoRunes bounds how much of a MODEL-SUPPLIED string a teaching error
// quotes back. Every argument on this surface is written by the model, so an
// error that echoes one verbatim is an output channel the model controls the
// length of: a 10 KB symbol name would come back as a 10 KB error, and a burst
// of them is a context window spent on nothing. The quoted form is always %q,
// so control characters, NULs and bidi overrides are escaped rather than
// embedded in the result.
const maxEchoRunes = 120

// echoArg renders a model-supplied argument for an error message: clamped, then
// Go-quoted. Use it EVERYWHERE an argument is quoted back — the cap is only
// worth anything if there is no path around it.
func echoArg(s string) string {
	r := []rune(s)
	if len(r) > maxEchoRunes {
		return strconv.Quote(string(r[:maxEchoRunes])) + fmt.Sprintf("… (+%d more characters)", len(r)-maxEchoRunes)
	}
	return strconv.Quote(s)
}

// echoErr renders a WRAPPED lower-level error inside a teaching message.
//
// It is clamped for the same reason echoArg is, and the reason is easy to miss:
// the wrapped text routinely embeds the very argument that failed. A 10 KB dir
// comes back from the filesystem as "…: file name too long" with all 10 KB of
// the name in it, so clamping the argument and then interpolating %v puts the
// argument straight back into the result through the side door.
func echoErr(err error) string {
	if err == nil {
		return ""
	}
	r := []rune(err.Error())
	if len(r) > maxEchoRunes {
		return string(r[:maxEchoRunes]) + "…"
	}
	return string(r)
}

// echoName renders a model-supplied IDENTIFIER — an argument NAME — for an
// error message. Same clamp as echoArg, and non-printable runes (a NUL, a bidi
// override) are replaced rather than embedded, but no quotes: the
// unknown-argument voice is local_fs's and reads as a bare comma-separated list.
func echoName(s string) string {
	var b strings.Builder
	for i, r := range []rune(s) {
		switch {
		case i >= maxEchoRunes:
			b.WriteString("…")
			return b.String()
		case unicode.IsPrint(r):
			b.WriteRune(r)
		default:
			b.WriteRune('?')
		}
	}
	return b.String()
}

// recoverablef builds a teaching error tagged recoverable-by-correction.
func recoverablef(format string, a ...any) error {
	return errors.New(fmt.Sprintf(format, a...) + " " + severityRecoverable)
}

// wrapRecoverable tags a sentinel-wrapping error recoverable unless it already
// carries a severity marker.
func wrapRecoverable(sentinel error, format string, a ...any) error {
	msg := fmt.Sprintf(format, a...)
	if strings.Contains(msg, severityRecoverable) || strings.Contains(msg, severityFatalToken) {
		return fmt.Errorf("%w: %s", sentinel, msg)
	}
	return fmt.Errorf("%w: %s %s", sentinel, msg, severityRecoverable)
}

// --- Configuration ----------------------------------------------------------

// Config configures an Index. Zero values fall back to the documented defaults.
type Config struct {
	// AllowedDir is the workspace root. Every query directory, and the module
	// root resolved from it, must lie within this directory — the same
	// dir-scoping local_fs applies to every path it touches.
	AllowedDir string
	// CwdResolver supplies the workspace root per call context when AllowedDir is
	// empty, matching localtools.NewLocalFSToolsWith's resolver seam so both
	// toolsets agree on what "the project root" is for a given session.
	CwdResolver func(context.Context) string
	// MaxRoots bounds how many module-root snapshots are cached (default 2).
	MaxRoots int
	// IdleTimeout drops a snapshot untouched for this long (default 15m; <=0
	// disables reaping).
	IdleTimeout time.Duration
}

// Request is the argument bundle every query takes. Fields are per-query:
//
//	Dir     — directory to resolve the module root from, relative to the
//	          workspace root. Empty means the workspace root itself.
//	Symbol  — the qualified symbol for describe/definition/references/
//	          implementations.
//	Target  — the package or file for symbols, and the package for
//	          diagnostics scope=package.
//	Scope   — diagnostics only: "changed", "package", or "all".
//	Passes  — diagnostics only: vet passes to run; empty takes
//	          DefaultVetPasses(), ["all"] takes VetPasses().
//	Max     — result cap for references, symbols and diagnostics; 0 takes the
//	          default.
type Request struct {
	Dir    string
	Symbol string
	Target string
	Scope  string
	Passes []string
	Max    int
}

// Index owns the per-module-root snapshot cache and answers every query. It is
// safe for concurrent use.
//
// The interface is what the tools repo holds and what the V1.1 post-edit
// middleware will hold: queries plus Invalidate. Nothing here exposes the
// snapshot itself, so no caller can hold one past the point where it stops
// being true.
type Index interface {
	// Describe returns kind, type, signature, doc and — for named types —
	// fields and methods.
	Describe(ctx context.Context, req Request) (*DescribeResult, error)
	// Definition returns the declaration site and the declaring source line.
	Definition(ctx context.Context, req Request) (*DefinitionResult, error)
	// References returns uses of the symbol across this module, grouped by file.
	References(ctx context.Context, req Request) (*ReferencesResult, error)
	// Implementations answers in both directions: implementers of an interface,
	// or the module interfaces a concrete type satisfies.
	Implementations(ctx context.Context, req Request) (*ImplementationsResult, error)
	// Symbols outlines a package or a file.
	Symbols(ctx context.Context, req Request) (*SymbolsResult, error)
	// Diagnostics returns type/parse errors plus a curated vet pass set.
	Diagnostics(ctx context.Context, req Request) (*DiagnosticsResult, error)

	// Invalidate marks the snapshots owning these paths dirty so the next query
	// rebuilds. It is the PRIMARY freshness mechanism: the V1.1 engine middleware
	// calls it with the paths of every successful write-class tool result on a
	// .go/go.mod/go.sum file, before the tool result is handed back to the model.
	// It never blocks and never rebuilds inline — a burst of edits collapses into
	// one rebuild at the next query.
	//
	// Paths may be absolute or relative to the process working directory, and may
	// name a file or a directory. Paths under no cached root are ignored.
	Invalidate(paths ...string)

	// Shutdown drops every snapshot and stops the reaper, joining its goroutine.
	Shutdown()
}

type index struct {
	cfg      Config
	idle     time.Duration
	capacity int

	mu      sync.Mutex
	entries map[string]*entry
	// order is the LRU: least-recently-used first, most-recent last.
	order []string

	stop     chan struct{}
	stopOnce sync.Once
	closed   atomic.Bool
	wg       sync.WaitGroup
}

// NewIndex builds an Index and starts its idle reaper.
func NewIndex(cfg Config) Index {
	ix := &index{
		cfg:      cfg,
		idle:     cfg.IdleTimeout,
		capacity: cfg.MaxRoots,
		entries:  map[string]*entry{},
		stop:     make(chan struct{}),
	}
	if ix.idle == 0 {
		ix.idle = defaultIdleTimeout
	}
	if ix.capacity <= 0 {
		ix.capacity = defaultMaxRoots
	}
	if ix.idle > 0 {
		ix.wg.Add(1)
		go ix.reap()
	}
	return ix
}

// Shutdown drops every snapshot, stops the reaper and joins it, and closes the
// index to further queries.
//
// The closed flag is not bookkeeping. Shutdown runs on engine.Stop, and a chain
// goroutine can still be mid-turn when it does: without the flag, that late
// query would find an empty cache, rebuild a snapshot (a `go list` subprocess
// and ~110 MB retained) and file it under a reaper that has already exited —
// a leak that outlives the engine it belonged to. Refusing with a typed error
// is the honest answer: the tool the model called no longer exists.
func (ix *index) Shutdown() {
	ix.closed.Store(true)
	ix.stopOnce.Do(func() { close(ix.stop) })
	ix.mu.Lock()
	ix.entries = map[string]*entry{}
	ix.order = nil
	ix.mu.Unlock()
	ix.wg.Wait()
}

// checkOpen is the guard every query runs before touching the cache.
func (ix *index) checkOpen() error {
	if ix.closed.Load() {
		return fmt.Errorf("%w: the Go index was shut down with the engine; no further query can be answered (fatal: index closed)", ErrShutdown)
	}
	return nil
}

// reap drops snapshots nothing has queried for IdleTimeout. The interval clamp
// (idle/2, floored at a second, ceilinged at a minute) and the
// collect-under-lock / act-outside-lock shape are shellsession.manager.reap's,
// on purpose.
func (ix *index) reap() {
	defer ix.wg.Done()
	interval := ix.idle / 2
	if interval < time.Second {
		interval = time.Second
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ix.stop:
			return
		case <-ticker.C:
			now := time.Now()
			ix.mu.Lock()
			for id, e := range ix.entries {
				if now.Sub(e.lastActivity()) >= ix.idle {
					delete(ix.entries, id)
					ix.dropOrderLocked(id)
				}
			}
			ix.mu.Unlock()
		}
	}
}

// --- Module-root resolution -------------------------------------------------

// allowedDir resolves the workspace root for this call. Mirrors
// LocalFSTools.baseDir/absAllowedDir: an explicit AllowedDir wins, otherwise the
// context resolver supplies it, and an unset root is a configuration error
// rather than an implicit "anywhere".
func (ix *index) allowedDir(ctx context.Context) (string, error) {
	base := ix.cfg.AllowedDir
	if strings.TrimSpace(base) == "" && ix.cfg.CwdResolver != nil {
		base = ix.cfg.CwdResolver(ctx)
	}
	if strings.TrimSpace(base) == "" {
		// FATAL, and it says so, because no argument the model can change will fix
		// it: the workspace root is a WIRING decision made above this package.
		//
		// This is the failure mode that looks like a broken tool and is not one.
		// gointel is registered unconditionally, so it appears in the tool list of
		// every session; a session whose composition root left both AllowedDir and
		// CwdResolver empty therefore advertises six tools that refuse every call.
		// A bare "no allowed directory configured" sends the model hunting for a
		// better symbol spelling forever. Naming the two ways to supply the root —
		// the flag an operator passes and the resolver a composition root wires —
		// is what turns that into something the first reader can act on.
		return "", errors.New("gointel: no workspace root is configured for this session, so there is nothing to index; " +
			"the root comes from the runtime's allowed directory (--local-exec-allowed-dir on the CLI) or from the " +
			"session cwd resolver the composition root supplies — no symbol or dir argument can substitute for it " +
			"(fatal: no workspace root)")
	}
	resolved, err := vfs.ResolveRoot(base)
	if err != nil {
		return "", recoverablef("gointel: workspace root %s cannot be resolved: %s", echoArg(base), echoErr(err))
	}
	return resolved, nil
}

// moduleRoot resolves dir (relative to the workspace root) to the module root
// that owns it.
//
// Containment first, exactly as local_fs does it — vfs.Contain is the single
// symlink-escape guard in the process, so a link inside the workspace pointing
// at /etc is refused here before any I/O. Then a go.mod walk-up bounded BY the
// workspace root: a module whose root sits above the allowed directory is
// refused rather than silently indexed, and it is refused with the containment
// voice, because that is what actually happened.
//
// NESTED MODULES: the walk stops at the FIRST go.mod at or above dir, so the
// INNERMOST module owning the query directory wins. A repository with a nested
// module (a tools/ or examples/ module inside the main one) therefore indexes
// whichever of the two actually contains dir — which is the same module `go
// build` would use standing in that directory, and the only reading under which
// a file:line answer is about the code the caller is looking at. Querying the
// outer module's symbols from inside the inner one is a genuine miss, answered
// by passing dir explicitly.
func (ix *index) moduleRoot(ctx context.Context, dir string) (root, base string, err error) {
	base, err = ix.allowedDir(ctx)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	start, err := vfs.Contain(base, dir)
	if err != nil {
		if errors.Is(err, vfs.ErrEscape) {
			// ONE typed boundary. A "../.." and a symlink pointing at /etc are the
			// same refusal as a module root sitting above the workspace, so a
			// caller branching on errors.Is(err, ErrOutsideAllowedDir) sees all
			// three — otherwise the two most obvious escape attempts would be the
			// two the sentinel misses.
			return "", "", wrapRecoverable(ErrOutsideAllowedDir,
				"path %s escapes allowed directory %s", echoArg(dir), base)
		}
		// Anything else vfs refused (a NUL byte in the path, an unreadable
		// component) is still a caller-correctable argument, so it carries the
		// recoverable marker like every other refusal on this surface.
		return "", "", recoverablef("gointel: cannot resolve dir %s: %s", echoArg(dir), echoErr(err))
	}
	if info, statErr := os.Stat(start); statErr == nil && !info.IsDir() {
		start = filepath.Dir(start)
	}

	for cur := start; ; {
		if hasGoMod(cur) {
			return cur, base, nil
		}
		if cur == base {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}

	// Nothing inside the workspace. If a go.mod exists ABOVE it, the module is
	// real and the refusal is a boundary refusal, not a "there is no Go here".
	for cur := filepath.Dir(base); ; {
		if hasGoMod(cur) {
			return "", "", wrapRecoverable(ErrOutsideAllowedDir,
				"%s is the module root for %s but lies outside the allowed directory %s; gointel only indexes modules rooted inside the workspace",
				cur, echoArg(dir), base)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return "", "", wrapRecoverable(ErrNoModule,
		"no go.mod at or above %s within %s; gointel indexes Go modules, so a directory outside any module has nothing to index",
		echoArg(dir), base)
}

func hasGoMod(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil && !info.IsDir()
}

// displayPath renders an absolute path the way the model addresses files
// everywhere else: relative to the workspace root, forward-slashed, so a
// file:line anchor from gointel can be pasted straight into
// local_fs.read_file. Copied in shape from LocalFSTools.displayPath.
func displayPath(base, abs string) string {
	if base == "" || abs == "" {
		return abs
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return abs
	}
	return filepath.ToSlash(rel)
}

// --- Cache ------------------------------------------------------------------

// entry is one module root's cache slot: at most one Snapshot, a generation
// counter that dirty-marking bumps, and the set of files observed to have
// changed under it.
type entry struct {
	root string
	base string

	// buildMu serializes rebuilds for this root. It is the coalescing point: N
	// queries arriving during one edit burst produce one packages.Load.
	buildMu sync.Mutex
	snap    atomic.Pointer[Snapshot]

	// gen counts dirty marks; builtGen records the gen the live snapshot was
	// built at. gen != builtGen means "something changed since this snapshot".
	// Keeping them as counters rather than a bool closes the window where an
	// Invalidate lands DURING a build: the build stamps the gen it started from,
	// so the mark is not swallowed.
	gen      atomic.Uint64
	builtGen atomic.Uint64
	// builds counts completed packages.Load calls for this root. It is the
	// coalescing evidence: an edit burst plus a query burst must move it by one.
	builds atomic.Uint64

	lastNs atomic.Int64

	changedMu sync.Mutex
	changed   []string
	changedAt map[string]struct{}
}

func (e *entry) touch()                  { e.lastNs.Store(time.Now().UnixNano()) }
func (e *entry) lastActivity() time.Time { return time.Unix(0, e.lastNs.Load()) }

func (e *entry) markDirty(path string) {
	e.gen.Add(1)
	if path == "" {
		return
	}
	e.changedMu.Lock()
	defer e.changedMu.Unlock()
	if e.changedAt == nil {
		e.changedAt = map[string]struct{}{}
	}
	if _, dup := e.changedAt[path]; dup {
		return
	}
	e.changedAt[path] = struct{}{}
	e.changed = append(e.changed, path)
	if len(e.changed) > maxChangedPaths {
		drop := e.changed[0]
		e.changed = e.changed[1:]
		delete(e.changedAt, drop)
	}
}

// changedPaths returns the files observed to have changed under this root since
// the process started, oldest first.
func (e *entry) changedPaths() []string {
	e.changedMu.Lock()
	defer e.changedMu.Unlock()
	return append([]string(nil), e.changed...)
}

// entryFor returns the cache slot for root, creating it and evicting the
// least-recently-used slot when the LRU is full. It returns ErrShutdown rather
// than a slot once the index is closed — the check is INSIDE the same lock
// Shutdown clears the map under, so a query that raced teardown cannot resurrect
// an entry the reaper is no longer alive to drop.
func (ix *index) entryFor(root, base string) (*entry, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.closed.Load() {
		return nil, ix.checkOpen()
	}
	e, ok := ix.entries[root]
	if !ok {
		e = &entry{root: root, base: base}
		ix.entries[root] = e
	}
	ix.dropOrderLocked(root)
	ix.order = append(ix.order, root)
	for len(ix.order) > ix.capacity {
		evict := ix.order[0]
		ix.order = ix.order[1:]
		delete(ix.entries, evict)
	}
	e.touch()
	return e, nil
}

func (ix *index) dropOrderLocked(root string) {
	for i, id := range ix.order {
		if id == root {
			ix.order = append(ix.order[:i], ix.order[i+1:]...)
			return
		}
	}
}

// get returns a snapshot for this entry, building or rebuilding when the live
// one is superseded by a dirty mark or by a moved go.mod/go.sum.
//
// The go.mod/go.sum sweep runs on EVERY query because it is two stat calls and
// it is the one change that invalidates the whole graph. Per-package file
// sweeps are deferred to withSnapshot, after resolution has named the package a
// query actually landed on.
func (e *entry) get(ctx context.Context) (*Snapshot, error) {
	e.touch()
	if s := e.snap.Load(); s != nil && e.builtGen.Load() == e.gen.Load() && !e.sweepModuleFiles(s) {
		return s, nil
	}
	return e.rebuild(ctx)
}

// rebuild replaces the snapshot under buildMu.
//
// COALESCING: every caller that finds the snapshot stale queues here, and the
// first one through does the work; the rest re-check on entry and return the
// snapshot it just built. So an edit burst of ten files followed by a query
// burst of five tools costs exactly one packages.Load, not fifteen. An
// Invalidate landing mid-build bumps gen past the builtGen this build stamps,
// so it is not swallowed — it simply rebuilds again at the next query.
func (e *entry) rebuild(ctx context.Context) (*Snapshot, error) {
	e.buildMu.Lock()
	defer e.buildMu.Unlock()
	if s := e.snap.Load(); s != nil && e.builtGen.Load() == e.gen.Load() && !e.sweepModuleFiles(s) && !e.sweepPackages(s, nil) {
		return s, nil
	}
	gen := e.gen.Load()
	s, err := buildSnapshot(ctx, e.root, e.base)
	if err != nil {
		return nil, err
	}
	e.snap.Store(s)
	e.builtGen.Store(gen)
	e.builds.Add(1)
	return s, nil
}

// sweepModuleFiles reports whether go.mod or go.sum moved under the snapshot.
func (e *entry) sweepModuleFiles(s *Snapshot) bool {
	for path, want := range s.moduleFiles {
		if stampOf(path) != want {
			e.markDirty(path)
			return true
		}
	}
	return false
}

// sweepPackages reports whether any file of the named packages moved since the
// snapshot was built. A nil pkgPaths sweeps EVERY file in the snapshot — the
// deliberately paranoid default for answers whose correctness depends on the
// whole module (references, implementations) and for "not found" answers, where
// the file that would have changed the answer is by definition not one the
// query named.
//
// Directory stamps are swept alongside file stamps so a brand-new .go file in
// an existing package — which no per-file stat can see — is caught too.
func (e *entry) sweepPackages(s *Snapshot, pkgPaths []string) bool {
	files, dirs := s.sweepSet(pkgPaths)
	for path, want := range files {
		if stampOf(path) != want {
			e.markDirty(path)
			return true
		}
	}
	for dir, want := range dirs {
		if stampOf(dir) != want {
			e.markDirty(dir)
			return true
		}
	}
	return false
}

func (ix *index) Invalidate(paths ...string) {
	if len(paths) == 0 || ix.closed.Load() {
		return
	}
	ix.mu.Lock()
	entries := make([]*entry, 0, len(ix.entries))
	for _, e := range ix.entries {
		entries = append(entries, e)
	}
	ix.mu.Unlock()
	if len(entries) == 0 {
		return
	}
	for _, p := range paths {
		abs, err := filepath.Abs(strings.TrimSpace(p))
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if !invalidatingPath(abs) {
			continue
		}
		for _, e := range entries {
			if underRoot(e.root, abs) {
				e.markDirty(abs)
			}
		}
	}
}

// invalidatingPath reports whether a written path can change a snapshot.
//
// Deliberately generous at the edges: .go, go.mod and go.sum are the load-bearing
// cases, and an extension-less path is treated as a directory (a rename or a
// delete of a package directory) because a needless rebuild costs a second while
// a missed one costs a wrong answer. Everything else — .md, .json, .yaml — is
// ignored, because a docs edit must not evict a warm snapshot.
func invalidatingPath(abs string) bool {
	switch base := filepath.Base(abs); base {
	case "go.mod", "go.sum", "go.work", "go.work.sum":
		return true
	}
	ext := filepath.Ext(abs)
	return ext == ".go" || ext == ""
}

func underRoot(root, abs string) bool {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// --- Query plumbing ---------------------------------------------------------

// withSnapshot runs fn against a fresh snapshot for req.Dir.
//
// fn returns the import paths whose source its answer depends on; a nil slice
// means "every file in the module". After fn returns, those files are swept: if
// any moved, the entry is marked dirty, the snapshot is rebuilt and fn runs
// ONCE more against the new one. That second run is the whole point — it is what
// makes "the file changed between the load and the query" impossible to observe
// as a stale answer, rather than merely unlikely.
//
// The sweep runs even when fn failed, because a failure is an answer too: "no
// such symbol" is exactly what a stale snapshot says about a symbol that was
// just added.
func (ix *index) withSnapshot(ctx context.Context, req Request, fn func(*Snapshot) ([]string, error)) error {
	if err := ix.checkOpen(); err != nil {
		return err
	}
	root, base, err := ix.moduleRoot(ctx, req.Dir)
	if err != nil {
		return err
	}
	e, err := ix.entryFor(root, base)
	if err != nil {
		return err
	}
	snap, err := e.get(ctx)
	if err != nil {
		return err
	}
	touched, fnErr := fn(snap)
	if !e.sweepPackages(snap, touched) {
		return fnErr
	}
	fresh, err := e.rebuild(ctx)
	if err != nil {
		return err
	}
	_, fnErr = fn(fresh)
	return fnErr
}

// entryAndSnapshot is withSnapshot's single-shot cousin for diagnostics, which
// needs the entry itself (scope=changed reads the observed-change set).
func (ix *index) entryAndSnapshot(ctx context.Context, req Request) (*entry, *Snapshot, error) {
	if err := ix.checkOpen(); err != nil {
		return nil, nil, err
	}
	root, base, err := ix.moduleRoot(ctx, req.Dir)
	if err != nil {
		return nil, nil, err
	}
	e, err := ix.entryFor(root, base)
	if err != nil {
		return nil, nil, err
	}
	snap, err := e.get(ctx)
	if err != nil {
		return nil, nil, err
	}
	if e.sweepPackages(snap, nil) {
		snap, err = e.rebuild(ctx)
		if err != nil {
			return nil, nil, err
		}
	}
	return e, snap, nil
}

// lookPathGo is exec.LookPath("go"), hoisted so the missing-toolchain error is
// raised as a teaching error before go/packages produces a driver error nobody
// can act on.
func lookPathGo() error {
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("%w: the go binary is not on PATH; gointel drives `go list` to read the module graph, so Go must be installed for any gointel tool to answer (fatal: no go toolchain)", ErrNoGoToolchain)
	}
	return nil
}

var _ Index = (*index)(nil)
