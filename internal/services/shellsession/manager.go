// Package shellsession manages one persistent PTY-backed shell per chat
// session, rooted at the session's workspace root and outliving individual
// commands. Output lives in a bounded scrollback ring with monotonically
// increasing offsets; the agent never receives it streamed and must read
// scrollback explicitly. Run submits exactly one line, gated by HITL.
package shellsession

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/contenox/contenox/internal/services/vfs"
)

// ErrNoSession is returned by Write for a session with no live shell. Distinct
// from a spawn failure: the caller addressed something that is not there.
var ErrNoSession = errors.New("shellsession: no live shell for this session")

// spawnKey carries a per-call spawn override. Unexported so the only way to
// set one is WithSpawn — a context value cannot be forged by a caller that
// does not import this package.
type spawnKey struct{}

// spawnOverride is the per-call half of Config: what THIS shell should be
// rooted at and run, when the manager-wide defaults are not it.
type spawnOverride struct {
	cwd   string
	shell string
}

// WithSpawn attaches a per-shell cwd and/or shell to ctx, honoured by the next
// shell this manager creates for the session named in that call. Empty strings
// fall back to the manager's CwdResolver and Config.Shell. The cwd is still
// validated against the workspace allowlist — an override chooses among
// permitted roots, it never escapes them.
func WithSpawn(ctx context.Context, cwd, shell string) context.Context {
	return context.WithValue(ctx, spawnKey{}, spawnOverride{cwd: cwd, shell: shell})
}

func spawnFrom(ctx context.Context) spawnOverride {
	if ctx == nil {
		return spawnOverride{}
	}
	ov, _ := ctx.Value(spawnKey{}).(spawnOverride)
	return ov
}

const (
	// defaultScrollbackBytes bounds the retained output per shell.
	defaultScrollbackBytes = 64 * 1024
	// defaultIdleTimeout kills a shell that has seen no activity for this long.
	defaultIdleTimeout = 15 * time.Minute
	// flushInterval coalesces PTY output before fanning it out to subscribers so
	// a `yes`-style flood becomes a handful of batched updates per second rather
	// than one wire frame per read.
	flushInterval = 60 * time.Millisecond
	// runCaptureWindow is how long Run waits for a submitted line's initial
	// output before returning a snapshot; it never blocks until process exit.
	runCaptureWindow = 250 * time.Millisecond
	readChunkBytes   = 32 * 1024
	subscriberBuffer = 1024
	// defaultRows/defaultCols size a PTY whose client never reported a
	// geometry; a wrong width wraps or truncates column-aware tool output.
	defaultRows = 24
	defaultCols = 120
)

// Chunk is one batch of terminal output delivered to a subscriber. Offset is the
// absolute scrollback offset where Data begins. Reset marks the initial
// snapshot a fresh subscriber receives (or a stream restart after the PTY was
// recreated), signalling the consumer to replace rather than append.
type Chunk struct {
	Offset int64
	Data   string
	Reset  bool
}

// RunResult is what Run returns after submitting a line: the scrollback end
// marker and a best-effort snapshot of the output captured within the initial
// window (empty when the command is still running silently).
type RunResult struct {
	Offset   int64
	Snapshot string
	Started  bool // a new shell was created for this run
}

// ReadResult is a scrollback slice: the content, the offset it starts at, and
// the current end marker to hand to the next read.
type ReadResult struct {
	Content    string
	FromOffset int64
	NextOffset int64
	Exists     bool
}

// Manager owns the process-global set of per-session shells. All methods are
// safe for concurrent use and key on the internal chat-session id.
type Manager interface {
	// Run ensures a shell exists for sessionID (rooted via the cwd resolver
	// against ctx) and submits one line to it. ctx is used only for cwd
	// resolution at creation time.
	Run(ctx context.Context, sessionID, line string) (RunResult, error)
	// Open ensures a shell exists for sessionID without submitting anything,
	// so an interactive client can attach before the first keystroke.
	// Idempotent: an already-live shell is returned as-is.
	Open(ctx context.Context, sessionID string) error
	// Write feeds raw bytes to sessionID's shell stdin VERBATIM — unlike Run
	// it appends no newline and imposes no line discipline, because the bytes
	// are a human's keystrokes (arrow keys, ^C, partial lines). Never creates
	// a shell: an unknown session is ErrNoSession.
	Write(sessionID string, data []byte) error
	// Read returns scrollback for sessionID: bytes since `since` when since >= 0,
	// otherwise the last `tailBytes`. Never creates a shell.
	Read(sessionID string, since int64, tailBytes int) ReadResult
	// Resize records the terminal geometry for sessionID and applies it to
	// the live shell when there is one. Total: an unknown session, a reaped
	// shell, or a non-positive dimension are no-ops, not errors. The size is
	// remembered even with no live shell, so the next one is born at it.
	Resize(sessionID string, rows, cols int)
	// Subscribe registers fn for live output of sessionID, invoked from a
	// dedicated goroutine so a slow consumer cannot stall the PTY. The
	// current scrollback is delivered immediately as a Reset chunk.
	Subscribe(sessionID string, fn func(Chunk)) (cancel func())
	// Kill terminates and forgets sessionID's shell (session close/delete).
	Kill(sessionID string)
	// Shutdown kills every shell and stops the reaper.
	Shutdown()
}

// Config configures a Manager. Zero values fall back to sane defaults.
type Config struct {
	// CwdResolver returns the workspace root a new shell should be rooted at,
	// given the tool/request context (which carries the session id). Required.
	CwdResolver func(ctx context.Context) string
	// Workspace is the operator's workspace-root allowlist, enforced against
	// whatever CwdResolver returns; the only source of the default root.
	// Nil means no allowlist, and an absolute cwd is taken as given.
	Workspace *vfs.Factory
	// Shell overrides the shell executable; empty picks a platform default.
	Shell string
	// ScrollbackBytes bounds retained output per shell (default 64 KiB).
	ScrollbackBytes int
	// IdleTimeout kills inactive shells (default 15m; <=0 disables reaping).
	IdleTimeout time.Duration
	// ScrubEnv, when set, maps the parent environment to the one a spawned
	// shell inherits, so serve's own secrets never reach an agent-reachable
	// PTY. Nil inherits the full environment.
	ScrubEnv func([]string) []string
	// Interactive spawns shells for a HUMAN at a real terminal: ECHO stays on
	// and the shell draws its own prompt, because the operator must see what
	// they type. The default (false) is the agent-facing posture — echo off,
	// prompt suppressed — where output is scrollback for a model to read and a
	// prompt is noise plus a login/host/cwd leak.
	Interactive bool
	// OnExit, when set, is invoked once per shell when it terminates, from a
	// dedicated goroutine. Fires for every cause — process exit, Kill, idle
	// reap, Shutdown — so a client can report the terminal as gone exactly
	// once. Total: never called twice for the same shell.
	OnExit func(sessionID string)
}

type manager struct {
	cfg      Config
	idle     time.Duration
	capacity int

	mu     sync.Mutex
	shells map[string]*shell
	subs   map[string][]*subscriber
	// sizes is the last geometry each session reported, kept independently
	// of the shell so it survives idle reaping.
	sizes map[string]ptySize

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewManager builds a Manager and starts its idle reaper.
func NewManager(cfg Config) Manager {
	m := &manager{
		cfg:      cfg,
		idle:     cfg.IdleTimeout,
		capacity: cfg.ScrollbackBytes,
		shells:   map[string]*shell{},
		subs:     map[string][]*subscriber{},
		sizes:    map[string]ptySize{},
		stop:     make(chan struct{}),
	}
	if m.idle == 0 {
		m.idle = defaultIdleTimeout
	}
	if m.capacity <= 0 {
		m.capacity = defaultScrollbackBytes
	}
	if m.idle > 0 {
		m.wg.Add(1)
		go m.reap()
	}
	return m
}

// resolveCwd decides which directory a new shell is rooted at. A PTY is a
// live interactive foothold, so the result is validated through
// vfs.ResolveSessionCwd — the same procedure the ACP session paths and
// fleet dispatch use — rather than trusting CwdResolver's answer directly.
// The fallback is "" since Workspace already carries the default root.
func (m *manager) resolveCwd(ctx context.Context) (string, error) {
	cwd := spawnFrom(ctx).cwd
	if cwd == "" && m.cfg.CwdResolver != nil {
		cwd = m.cfg.CwdResolver(ctx)
	}
	return vfs.ResolveSessionCwd(m.cfg.Workspace, cwd, "")
}

func (m *manager) Open(ctx context.Context, sessionID string) error {
	_, _, err := m.ensureShell(ctx, sessionID)
	return err
}

func (m *manager) Write(sessionID string, data []byte) error {
	m.mu.Lock()
	sh, ok := m.shells[sessionID]
	m.mu.Unlock()
	if !ok || sh.closed.Load() {
		return ErrNoSession
	}
	sh.touch()
	_, err := sh.pty.Write(data)
	return err
}

func (m *manager) Run(ctx context.Context, sessionID, line string) (RunResult, error) {
	sh, started, err := m.ensureShell(ctx, sessionID)
	if err != nil {
		return RunResult{}, err
	}
	pre := sh.sb.endOffset()
	if err := sh.submit(line); err != nil {
		return RunResult{}, err
	}
	// Capture a brief window for the snapshot; long-running output keeps streaming to subscribers.
	select {
	case <-time.After(runCaptureWindow):
	case <-ctx.Done():
	}
	res := m.Read(sessionID, pre, 0)
	return RunResult{Offset: res.NextOffset, Snapshot: res.Content, Started: started}, nil
}

func (m *manager) Read(sessionID string, since int64, tailBytes int) ReadResult {
	m.mu.Lock()
	sh, ok := m.shells[sessionID]
	m.mu.Unlock()
	if !ok {
		return ReadResult{Exists: false}
	}
	var data []byte
	var from, to int64
	if since >= 0 {
		data, from, to = sh.sb.since(since)
	} else {
		data, from, to = sh.sb.tail(tailBytes)
	}
	return ReadResult{Content: string(data), FromOffset: from, NextOffset: to, Exists: true}
}

func (m *manager) Subscribe(sessionID string, fn func(Chunk)) func() {
	sub := newSubscriber(fn)
	m.mu.Lock()
	m.subs[sessionID] = append(m.subs[sessionID], sub)
	sh, ok := m.shells[sessionID]
	m.mu.Unlock()

	// Deliver the current scrollback as a Reset so a reconnecting client repopulates cleanly.
	data, from, _ := []byte(nil), int64(0), int64(0)
	if ok {
		data, from, _ = sh.sb.snapshot()
	}
	sub.deliver(Chunk{Offset: from, Data: string(data), Reset: true})

	return func() {
		m.mu.Lock()
		list := m.subs[sessionID]
		for i, s := range list {
			if s == sub {
				m.subs[sessionID] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(m.subs[sessionID]) == 0 {
			delete(m.subs, sessionID)
		}
		m.mu.Unlock()
		sub.stop()
	}
}

func (m *manager) Resize(sessionID string, rows, cols int) {
	if sessionID == "" || rows <= 0 || cols <= 0 {
		return
	}
	m.mu.Lock()
	prev, had := m.sizes[sessionID]
	size := ptySize{rows: rows, cols: cols}
	m.sizes[sessionID] = size
	sh := m.shells[sessionID]
	m.mu.Unlock()
	if had && prev == size {
		// Nothing changed: skip the ioctl so a client reporting geometry every run does not spray SIGWINCH.
		return
	}
	if sh == nil || sh.closed.Load() {
		return
	}
	_ = sh.pty.resize(rows, cols)
}

func (m *manager) Kill(sessionID string) {
	m.killShell(sessionID, true)
}

func (m *manager) Shutdown() {
	m.stopOnce.Do(func() { close(m.stop) })
	m.mu.Lock()
	ids := make([]string, 0, len(m.shells))
	for id := range m.shells {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.killShell(id, true)
	}
	m.wg.Wait()
}

// ensureShell returns the live shell for sessionID, creating one when absent.
func (m *manager) ensureShell(ctx context.Context, sessionID string) (*shell, bool, error) {
	m.mu.Lock()
	if sh, ok := m.shells[sessionID]; ok && !sh.closed.Load() {
		m.mu.Unlock()
		return sh, false, nil
	}
	m.mu.Unlock()

	cwd, err := m.resolveCwd(ctx)
	if err != nil {
		return nil, false, err
	}
	// Born at the session's last known geometry, not the default.
	m.mu.Lock()
	size, ok := m.sizes[sessionID]
	m.mu.Unlock()
	if !ok {
		size = ptySize{rows: defaultRows, cols: defaultCols}
	}
	shellPath := spawnFrom(ctx).shell
	if shellPath == "" {
		shellPath = m.cfg.Shell
	}
	pty, err := startPTY(spawnSpec{
		cwd:         cwd,
		shell:       shellPath,
		scrub:       m.cfg.ScrubEnv,
		rows:        size.rows,
		cols:        size.cols,
		interactive: m.cfg.Interactive,
	})
	if err != nil {
		return nil, false, err
	}
	sh := &shell{
		id:   sessionID,
		pty:  pty,
		sb:   newScrollback(m.capacity),
		mgr:  m,
		done: make(chan struct{}),
	}
	sh.touch()

	m.mu.Lock()
	// Lost a race: another caller created the shell first — discard ours.
	if existing, ok := m.shells[sessionID]; ok && !existing.closed.Load() {
		m.mu.Unlock()
		pty.close()
		return existing, false, nil
	}
	m.shells[sessionID] = sh
	m.mu.Unlock()

	m.wg.Add(2)
	go sh.readLoop()
	go sh.flushLoop()
	return sh, true, nil
}

func (m *manager) fanout(sessionID string, c Chunk) {
	m.mu.Lock()
	subs := append([]*subscriber(nil), m.subs[sessionID]...)
	m.mu.Unlock()
	for _, s := range subs {
		s.deliver(c)
	}
}

func (m *manager) killShell(sessionID string, dropSubs bool) {
	m.mu.Lock()
	sh := m.shells[sessionID]
	delete(m.shells, sessionID)
	var subs []*subscriber
	if dropSubs {
		subs = m.subs[sessionID]
		delete(m.subs, sessionID)
		// Only dropped when the session is going away; idle reap passes false and keeps it.
		delete(m.sizes, sessionID)
	}
	m.mu.Unlock()
	if sh != nil {
		sh.shutdown()
	}
	for _, s := range subs {
		s.stop()
	}
}

func (m *manager) reap() {
	defer m.wg.Done()
	interval := m.idle / 2
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
		case <-m.stop:
			return
		case <-ticker.C:
			now := time.Now()
			m.mu.Lock()
			var idle []string
			for id, sh := range m.shells {
				if now.Sub(sh.lastActivity()) >= m.idle {
					idle = append(idle, id)
				}
			}
			m.mu.Unlock()
			// Idle-reaped shells keep their subscribers; a later run re-streams from a fresh shell.
			for _, id := range idle {
				m.killShell(id, false)
			}
		}
	}
}

// ptySize is a terminal geometry. Comparable on purpose: Resize uses equality to
// skip a no-change ioctl.
type ptySize struct {
	rows int
	cols int
}

// shell is one running PTY plus its scrollback and output pump.
type shell struct {
	id      string
	pty     *ptySession
	sb      *scrollback
	mgr     *manager
	done    chan struct{}
	once    sync.Once
	closed  atomic.Bool
	lastNs  atomic.Int64
	emitted int64 // last offset fanned out (flushLoop-only)
}

func (s *shell) touch()                  { s.lastNs.Store(time.Now().UnixNano()) }
func (s *shell) lastActivity() time.Time { return time.Unix(0, s.lastNs.Load()) }

func (s *shell) submit(line string) error {
	s.touch()
	_, err := s.pty.Write([]byte(line + "\n"))
	return err
}

func (s *shell) readLoop() {
	defer s.mgr.wg.Done()
	buf := make([]byte, readChunkBytes)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			s.sb.append(buf[:n])
			s.touch()
		}
		if err != nil {
			s.shutdown()
			return
		}
	}
}

func (s *shell) flushLoop() {
	defer s.mgr.wg.Done()
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			s.flush()
			return
		case <-ticker.C:
			s.flush()
		}
	}
}

func (s *shell) flush() {
	data, from, to := s.sb.since(s.emitted)
	if len(data) == 0 {
		return
	}
	s.emitted = to
	s.mgr.fanout(s.id, Chunk{Offset: from, Data: string(data)})
}

func (s *shell) shutdown() {
	s.once.Do(func() {
		s.closed.Store(true)
		close(s.done)
		s.pty.close()
		go s.pty.wait()
		// once guarantees exactly one notification per shell, whichever of the
		// four teardown paths got here first.
		if fn := s.mgr.cfg.OnExit; fn != nil {
			go fn(s.id)
		}
	})
}

// spawnSpec is everything startPTY needs for one shell: the already-validated
// cwd, the shell to exec, the environment mapping, the initial geometry, and
// whether this PTY faces a human (see Config.Interactive).
type spawnSpec struct {
	cwd         string
	shell       string
	scrub       func([]string) []string
	rows        int
	cols        int
	interactive bool
}

var _ Manager = (*manager)(nil)
