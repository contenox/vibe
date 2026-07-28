// Package gointel is the agent's in-process Go code intelligence: a warm,
// type-checked view of the Go module under the workspace root, exposed as
// read-only tools (go_describe, go_definition, go_references,
// go_implementations, go_symbols, go_diagnostics). Queries resolve against an
// immutable per-module Snapshot.
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

	"github.com/contenox/contenox/internal/services/vfs"
)

// ToolsProviderName is the tools-provider key this package registers under. Every tool it exposes is a pure read at allow tier.
const ToolsProviderName = "gointel"

const (
	// defaultMaxRoots is the LRU bound on cached module-root snapshots.
	defaultMaxRoots = 2

	// defaultIdleTimeout drops a snapshot nothing has queried for this long.
	defaultIdleTimeout = 15 * time.Minute

	// maxChangedPaths bounds the per-root changed-file set go_diagnostics scope=changed reports over; oldest entries are dropped past the bound.
	maxChangedPaths = 512
)

// Errors carry a "gointel: " prefix, the value that failed, and a severity marker: every failure is recoverable by a corrected call except a missing Go toolchain.

const (
	severityRecoverable = "(recoverable: adjust parameters and retry)"
	severityFatalToken  = "(fatal:"
)

// Sentinel errors, for errors.Is by callers that need to branch.
var (
	// ErrNoModule means no go.mod was found at or above the query directory, within the allowed directory.
	ErrNoModule = errors.New("gointel: no Go module")
	// ErrOutsideAllowedDir means the module root that owns the query directory lies outside the allowed directory.
	ErrOutsideAllowedDir = errors.New("gointel: module root outside allowed directory")
	// ErrNoGoToolchain means the `go` binary is not on PATH.
	ErrNoGoToolchain = errors.New("gointel: no go toolchain")
	// ErrLoad means the module could not be loaded at all (driver failure).
	ErrLoad = errors.New("gointel: module load failed")
	// ErrNotFound means a named symbol resolved to nothing in this module.
	ErrNotFound = errors.New("gointel: symbol not found")
	// ErrAmbiguous means a name matched more than one declaration; the error text lists the qualified candidates.
	ErrAmbiguous = errors.New("gointel: ambiguous symbol")
	// ErrShutdown means the index has been shut down and answers nothing further, rather than silently rebuilding a snapshot no reaper will drop.
	ErrShutdown = errors.New("gointel: index shut down")
)

// maxEchoRunes bounds how much of a model-supplied string a teaching error quotes back, since the argument length is otherwise attacker-controlled.
const maxEchoRunes = 120

// echoArg renders a model-supplied argument for an error message: clamped, then Go-quoted. Use it everywhere an argument is quoted back.
func echoArg(s string) string {
	r := []rune(s)
	if len(r) > maxEchoRunes {
		return strconv.Quote(string(r[:maxEchoRunes])) + fmt.Sprintf("… (+%d more characters)", len(r)-maxEchoRunes)
	}
	return strconv.Quote(s)
}

// echoErr renders a wrapped lower-level error inside a teaching message, clamped because the wrapped text often embeds the failed argument itself.
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

// echoName renders a model-supplied identifier for an error message: same clamp as echoArg, non-printable runes replaced, but unquoted.
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

// wrapRecoverable tags a sentinel-wrapping error recoverable unless it already carries a severity marker.
func wrapRecoverable(sentinel error, format string, a ...any) error {
	msg := fmt.Sprintf(format, a...)
	if strings.Contains(msg, severityRecoverable) || strings.Contains(msg, severityFatalToken) {
		return fmt.Errorf("%w: %s", sentinel, msg)
	}
	return fmt.Errorf("%w: %s %s", sentinel, msg, severityRecoverable)
}

// Config configures an Index. Zero values fall back to the documented defaults.
type Config struct {
	// AllowedDir is the workspace root. Every query directory, and the module root resolved from it, must lie within this directory.
	AllowedDir string
	// CwdResolver supplies the workspace root per call context when AllowedDir is empty.
	CwdResolver func(context.Context) string
	// MaxRoots bounds how many module-root snapshots are cached (default 2).
	MaxRoots int
	// IdleTimeout drops a snapshot untouched for this long (default 15m; <=0 disables reaping).
	IdleTimeout time.Duration
}

// Request is the argument bundle every query takes. Fields are per-query:
//
//	Dir     — directory to resolve the module root from; empty means the workspace root.
//	Symbol  — the qualified symbol for describe/definition/references/implementations.
//	Target  — the package or file for symbols, and the package for diagnostics scope=package.
//	Scope   — diagnostics only: "changed", "package", or "all".
//	Passes  — diagnostics only: vet passes to run; empty takes DefaultVetPasses(), ["all"] takes VetPasses().
//	Max     — result cap for references, symbols and diagnostics; 0 takes the default.
type Request struct {
	Dir    string
	Symbol string
	Target string
	Scope  string
	Passes []string
	Max    int
}

// Index owns the per-module-root snapshot cache and answers every query. It is safe for concurrent use. Nothing here exposes the snapshot itself, so no caller can hold one past the point where it stops being true.
type Index interface {
	// Describe returns kind, type, signature, doc and — for named types — fields and methods.
	Describe(ctx context.Context, req Request) (*DescribeResult, error)
	// Definition returns the declaration site and the declaring source line.
	Definition(ctx context.Context, req Request) (*DefinitionResult, error)
	// References returns uses of the symbol across this module, grouped by file.
	References(ctx context.Context, req Request) (*ReferencesResult, error)
	// Implementations answers in both directions: implementers of an interface, or the module interfaces a concrete type satisfies.
	Implementations(ctx context.Context, req Request) (*ImplementationsResult, error)
	// Symbols outlines a package or a file.
	Symbols(ctx context.Context, req Request) (*SymbolsResult, error)
	// Diagnostics returns type/parse errors plus a curated vet pass set.
	Diagnostics(ctx context.Context, req Request) (*DiagnosticsResult, error)

	// Invalidate marks the snapshots owning these paths dirty so the next query rebuilds; it never blocks and never rebuilds inline. Paths may be absolute or relative to the process working directory, and may name a file or a directory; paths under no cached root are ignored.
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

// Shutdown drops every snapshot, stops the reaper and joins it, and closes the index to further queries. A query racing teardown gets ErrShutdown rather than silently rebuilding into a cache no reaper will ever drop.
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

// reap drops snapshots nothing has queried for IdleTimeout. The tick interval is idle/2, clamped to [1s, 1m].
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

// allowedDir resolves the workspace root for this call: an explicit AllowedDir wins, otherwise the context resolver supplies it, and an unset root is a configuration error rather than an implicit "anywhere".
func (ix *index) allowedDir(ctx context.Context) (string, error) {
	base := ix.cfg.AllowedDir
	if strings.TrimSpace(base) == "" && ix.cfg.CwdResolver != nil {
		base = ix.cfg.CwdResolver(ctx)
	}
	if strings.TrimSpace(base) == "" {
		// Fatal: the workspace root is a wiring decision made above this package, so no query argument can fix it.
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

// moduleRoot resolves dir (relative to the workspace root) to the module root that owns it: vfs.Contain guards symlink escapes first, then a go.mod walk-up bounded by the workspace root. The walk stops at the first go.mod at or above dir, so the innermost module owning dir wins — the same module `go build` would use standing there. Querying an outer module's symbols from inside a nested one requires passing dir explicitly.
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
			// Same sentinel as a module root sitting above the workspace, so callers branching on errors.Is(err, ErrOutsideAllowedDir) see both.
			return "", "", wrapRecoverable(ErrOutsideAllowedDir,
				"path %s escapes allowed directory %s", echoArg(dir), base)
		}
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

	// Nothing inside the workspace. If a go.mod exists above it, the module is real and the refusal is a boundary refusal, not "there is no Go here".
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

// displayPath renders an absolute path relative to the workspace root, forward-slashed, so a file:line anchor can be pasted into local_fs.read_file.
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

// entry is one module root's cache slot: at most one Snapshot, a generation counter that dirty-marking bumps, and the set of files observed to have changed under it.
type entry struct {
	root string
	base string

	// buildMu serializes rebuilds for this root: concurrent queries during one edit burst produce a single packages.Load.
	buildMu sync.Mutex
	snap    atomic.Pointer[Snapshot]

	// gen counts dirty marks; builtGen records the gen the live snapshot was built at. gen != builtGen means "something changed since this snapshot". Counters rather than a bool so an Invalidate landing during a build is not swallowed: the build stamps the gen it started from.
	gen      atomic.Uint64
	builtGen atomic.Uint64
	// builds counts completed packages.Load calls for this root.
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

// changedPaths returns the files observed to have changed under this root since the process started, oldest first.
func (e *entry) changedPaths() []string {
	e.changedMu.Lock()
	defer e.changedMu.Unlock()
	return append([]string(nil), e.changed...)
}

// entryFor returns the cache slot for root, creating it and evicting the least-recently-used slot when the LRU is full. Returns ErrShutdown, under the same lock Shutdown clears the map under, once the index is closed.
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

// get returns a snapshot for this entry, building or rebuilding when the live one is superseded by a dirty mark or a moved go.mod/go.sum. The go.mod/go.sum sweep runs on every query; per-package file sweeps are deferred to withSnapshot, after resolution names the package a query landed on.
func (e *entry) get(ctx context.Context) (*Snapshot, error) {
	e.touch()
	if s := e.snap.Load(); s != nil && e.builtGen.Load() == e.gen.Load() && !e.sweepModuleFiles(s) {
		return s, nil
	}
	return e.rebuild(ctx)
}

// rebuild replaces the snapshot under buildMu. Every caller that finds the snapshot stale queues here; the first through does the work and the rest re-check on entry and return the snapshot it just built, so one packages.Load serves the whole burst. An Invalidate landing mid-build bumps gen past the builtGen this build stamps, so it rebuilds again at the next query.
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

// sweepPackages reports whether any file of the named packages moved since the snapshot was built. A nil pkgPaths sweeps every file in the snapshot, for answers whose correctness depends on the whole module (references, implementations) and for "not found" answers. Directory stamps are swept alongside file stamps so a brand-new .go file in an existing package is caught too.
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

// invalidatingPath reports whether a written path can change a snapshot: .go, go.mod/go.sum/go.work(.sum), and extension-less paths (treated as a directory rename/delete). Everything else, e.g. .md/.json/.yaml, is ignored.
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

// withSnapshot runs fn against a fresh snapshot for req.Dir. fn returns the import paths its answer depends on; nil means every file in the module. After fn returns, those files are swept; if any moved, the entry is marked dirty, the snapshot rebuilt, and fn runs once more against the new one, so a file changing between load and query cannot surface as a stale answer. The sweep runs even when fn failed, since "no such symbol" is exactly what a stale snapshot says about a symbol just added.
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

// entryAndSnapshot is withSnapshot's single-shot cousin for diagnostics, which needs the entry itself (scope=changed reads the observed-change set).
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

// lookPathGo is exec.LookPath("go"), hoisted so the missing-toolchain error is raised as a teaching error before go/packages produces a driver error nobody can act on.
func lookPathGo() error {
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("%w: the go binary is not on PATH; gointel drives `go list` to read the module graph, so Go must be installed for any gointel tool to answer (fatal: no go toolchain)", ErrNoGoToolchain)
	}
	return nil
}

var _ Index = (*index)(nil)
