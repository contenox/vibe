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

// ToolsProviderName is the tools-provider key this package registers under; every tool it exposes is a pure read at allow tier.
const ToolsProviderName = "gointel"

const (
	defaultMaxRoots = 2

	defaultIdleTimeout = 15 * time.Minute

	maxChangedPaths = 512
)

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

const maxEchoRunes = 120

func echoArg(s string) string {
	r := []rune(s)
	if len(r) > maxEchoRunes {
		return strconv.Quote(string(r[:maxEchoRunes])) + fmt.Sprintf("… (+%d more characters)", len(r)-maxEchoRunes)
	}
	return strconv.Quote(s)
}

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

func recoverablef(format string, a ...any) error {
	return errors.New(fmt.Sprintf(format, a...) + " " + severityRecoverable)
}

func wrapRecoverable(sentinel error, format string, a ...any) error {
	msg := fmt.Sprintf(format, a...)
	if strings.Contains(msg, severityRecoverable) || strings.Contains(msg, severityFatalToken) {
		return fmt.Errorf("%w: %s", sentinel, msg)
	}
	return fmt.Errorf("%w: %s %s", sentinel, msg, severityRecoverable)
}

// Config configures an Index; zero values fall back to the documented defaults.
type Config struct {
	// AllowedDir is the workspace root that every query directory and module root must lie within.
	AllowedDir string
	// CwdResolver supplies the workspace root per call context when AllowedDir is empty.
	CwdResolver func(context.Context) string
	// MaxRoots bounds how many module-root snapshots are cached (default 2).
	MaxRoots int
	// IdleTimeout drops a snapshot untouched for this long (default 15m; <=0 disables reaping).
	IdleTimeout time.Duration
}

// Request is the per-query argument bundle; each field is meaningful only to the queries that use it.
type Request struct {
	Dir    string
	Symbol string
	Target string
	Scope  string
	Passes []string
	Max    int
}

// Index owns the per-module-root snapshot cache, answers every query, and is safe for concurrent use.
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

	// Invalidate marks the snapshots owning these paths dirty so the next query rebuilds; it never blocks or rebuilds inline.
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
	order   []string

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

// Shutdown drops every snapshot, stops and joins the reaper, and closes the index to further queries; a racing query gets ErrShutdown rather than a silent rebuild.
func (ix *index) Shutdown() {
	ix.closed.Store(true)
	ix.stopOnce.Do(func() { close(ix.stop) })
	ix.mu.Lock()
	ix.entries = map[string]*entry{}
	ix.order = nil
	ix.mu.Unlock()
	ix.wg.Wait()
}

func (ix *index) checkOpen() error {
	if ix.closed.Load() {
		return fmt.Errorf("%w: the Go index was shut down with the engine; no further query can be answered (fatal: index closed)", ErrShutdown)
	}
	return nil
}

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

func (ix *index) allowedDir(ctx context.Context) (string, error) {
	base := ix.cfg.AllowedDir
	if strings.TrimSpace(base) == "" && ix.cfg.CwdResolver != nil {
		base = ix.cfg.CwdResolver(ctx)
	}
	if strings.TrimSpace(base) == "" {
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

type entry struct {
	root string
	base string

	buildMu sync.Mutex
	snap    atomic.Pointer[Snapshot]

	gen      atomic.Uint64
	builtGen atomic.Uint64
	builds   atomic.Uint64

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

func (e *entry) changedPaths() []string {
	e.changedMu.Lock()
	defer e.changedMu.Unlock()
	return append([]string(nil), e.changed...)
}

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

func (e *entry) get(ctx context.Context) (*Snapshot, error) {
	e.touch()
	if s := e.snap.Load(); s != nil && e.builtGen.Load() == e.gen.Load() && !e.sweepModuleFiles(s) {
		return s, nil
	}
	return e.rebuild(ctx)
}

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

func (e *entry) sweepModuleFiles(s *Snapshot) bool {
	for path, want := range s.moduleFiles {
		if stampOf(path) != want {
			e.markDirty(path)
			return true
		}
	}
	return false
}

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

func lookPathGo() error {
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("%w: the go binary is not on PATH; gointel drives `go list` to read the module graph, so Go must be installed for any gointel tool to answer (fatal: no go toolchain)", ErrNoGoToolchain)
	}
	return nil
}

var _ Index = (*index)(nil)
