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

// ErrNoSession is returned by Write for a session with no live shell, distinct from a spawn failure: the caller addressed something that is not there.
var ErrNoSession = errors.New("shellsession: no live shell for this session")

type spawnKey struct{}

type spawnOverride struct {
	cwd   string
	shell string
}

// WithSpawn attaches a per-shell cwd and/or shell to ctx, honoured by the next shell created for that session; empty strings fall back to CwdResolver/Config.Shell, and the cwd is still validated against the workspace allowlist.
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
	defaultScrollbackBytes = 64 * 1024
	defaultIdleTimeout     = 15 * time.Minute
	flushInterval          = 60 * time.Millisecond
	runCaptureWindow       = 250 * time.Millisecond
	readChunkBytes         = 32 * 1024
	subscriberBuffer       = 1024
	defaultRows            = 24
	defaultCols            = 120
)

// Chunk is one batch of terminal output delivered to a subscriber: Offset is where Data begins in scrollback, and Reset marks a snapshot (initial or after PTY restart) the consumer should replace rather than append.
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

// Manager owns the process-global set of per-session shells; all methods are safe for concurrent use and key on the internal chat-session id.
type Manager interface {
	// Run ensures a shell exists for sessionID (rooted via the cwd resolver against ctx, used only at creation time) and submits one line to it.
	Run(ctx context.Context, sessionID, line string) (RunResult, error)
	// Open ensures a shell exists for sessionID without submitting anything, so an interactive client can attach before the first keystroke; idempotent, an already-live shell is returned as-is.
	Open(ctx context.Context, sessionID string) error
	// Write feeds raw bytes to sessionID's shell stdin VERBATIM (unlike Run, no newline or line discipline, since the bytes are a human's keystrokes); never creates a shell — an unknown session is ErrNoSession.
	Write(sessionID string, data []byte) error
	// Read returns scrollback for sessionID (bytes since `since` when since >= 0, otherwise the last `tailBytes`); never creates a shell.
	Read(sessionID string, since int64, tailBytes int) ReadResult
	// Resize records the terminal geometry for sessionID and applies it to the live shell if any; total (unknown session, reaped shell, or non-positive dimension are no-ops), and remembered even with no live shell so the next one is born at it.
	Resize(sessionID string, rows, cols int)
	// Subscribe registers fn for live output of sessionID, invoked from a dedicated goroutine so a slow consumer cannot stall the PTY; the current scrollback is delivered immediately as a Reset chunk.
	Subscribe(sessionID string, fn func(Chunk)) (cancel func())
	// Kill terminates and forgets sessionID's shell (session close/delete).
	Kill(sessionID string)
	// Shutdown kills every shell and stops the reaper.
	Shutdown()
}

// Config configures a Manager; zero values fall back to sane defaults.
type Config struct {
	// CwdResolver returns the workspace root a new shell should be rooted at, given the tool/request context (which carries the session id); required.
	CwdResolver func(ctx context.Context) string
	// Workspace is the operator's workspace-root allowlist enforced against CwdResolver's output and the only source of the default root; nil means no allowlist, so an absolute cwd is taken as given.
	Workspace *vfs.Factory
	// Shell overrides the shell executable; empty picks a platform default.
	Shell string
	// ScrollbackBytes bounds retained output per shell (default 64 KiB).
	ScrollbackBytes int
	// IdleTimeout kills inactive shells (default 15m; <=0 disables reaping).
	IdleTimeout time.Duration
	// ScrubEnv, when set, maps the parent environment to the one a spawned shell inherits, so serve's own secrets never reach an agent-reachable PTY; nil inherits the full environment.
	ScrubEnv func([]string) []string
	// Interactive spawns shells for a HUMAN at a real terminal (ECHO on, shell draws its own prompt); the default (false) is the agent-facing posture — echo off, prompt suppressed — since output is scrollback for a model to read.
	Interactive bool
	// OnExit, when set, is invoked once per shell when it terminates, from a dedicated goroutine, for every cause (process exit, Kill, idle reap, Shutdown); total, never called twice for the same shell.
	OnExit func(sessionID string)
}

type manager struct {
	cfg      Config
	idle     time.Duration
	capacity int

	mu     sync.Mutex
	shells map[string]*shell
	subs   map[string][]*subscriber
	sizes  map[string]ptySize

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

func (m *manager) resolveCwd(ctx context.Context) (string, error) {
	cwd := spawnFrom(ctx).cwd
	if cwd == "" && m.cfg.CwdResolver != nil {
		cwd = m.cfg.CwdResolver(ctx)
	}
	// A PTY is a live interactive foothold: validated through vfs.ResolveSessionCwd rather than trusted directly.
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

type ptySize struct {
	rows int
	cols int
}

type shell struct {
	id      string
	pty     *ptySession
	sb      *scrollback
	mgr     *manager
	done    chan struct{}
	once    sync.Once
	closed  atomic.Bool
	lastNs  atomic.Int64
	emitted int64
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
		// once guarantees exactly one notification per shell, whichever of the four teardown paths gets here first.
		if fn := s.mgr.cfg.OnExit; fn != nil {
			go fn(s.id)
		}
	})
}

type spawnSpec struct {
	cwd         string
	shell       string
	scrub       func([]string) []string
	rows        int
	cols        int
	interactive bool
}

var _ Manager = (*manager)(nil)
