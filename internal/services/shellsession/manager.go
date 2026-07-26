// Package shellsession manages one persistent PTY-backed shell per chat session.
//
// It is the backend of the "Shell Sessions" surface (see
// docs/development/blueprints/beam/shell-sessions.md). A shell is created on
// demand, rooted at the session's workspace root, and outlives individual
// commands: cwd, environment, history, and long-running processes persist
// between submitted lines. Output is captured in a bounded scrollback ring with
// monotonically increasing offsets so both the agent (via the read tool) and the
// live UI stream (via subscribers) can consume "everything since a marker"
// cheaply and without races.
//
// Two reference-only ingestion paths exist, matching the files/@-mention
// principle: the agent NEVER receives terminal output streamed into its context;
// it must ask for scrollback explicitly through the read tool, and the human
// watches the live stream through subscribers. Line input is the approval unit:
// Run submits exactly one line, gated upstream by the same HITL machinery that
// wraps every tool.
package shellsession

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/contenox/beam/internal/services/vfs"
)

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
	// Read returns scrollback for sessionID: bytes since `since` when since >= 0,
	// otherwise the last `tailBytes`. Never creates a shell.
	Read(sessionID string, since int64, tailBytes int) ReadResult
	// Subscribe registers fn for live output of sessionID. fn is invoked from a
	// dedicated goroutine (so a slow consumer cannot stall the PTY). The current
	// scrollback is delivered immediately as a Reset chunk. The returned cancel
	// stops delivery.
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
	// whatever CwdResolver returns. It is the ONLY source of the default root:
	// an unspecified cwd resolves to Workspace.Default(). Nil means no allowlist
	// is configured, in which case an absolute cwd is taken as given — the same
	// rule vfs.ResolveSessionCwd applies everywhere else.
	Workspace *vfs.Factory
	// Shell overrides the shell executable; empty picks a platform default.
	Shell string
	// ScrollbackBytes bounds retained output per shell (default 64 KiB).
	ScrollbackBytes int
	// IdleTimeout kills inactive shells (default 15m; <=0 disables reaping).
	IdleTimeout time.Duration
	// ScrubEnv, when set, maps the parent process environment to the one a spawned
	// shell inherits — the credential-leak fix, so serve's own secrets do not ride
	// into an agent-reachable PTY. Nil (the default) inherits the full environment.
	ScrubEnv func([]string) []string
}

type manager struct {
	cfg      Config
	idle     time.Duration
	capacity int

	mu     sync.Mutex
	shells map[string]*shell
	subs   map[string][]*subscriber

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

// resolveCwd decides which directory a new shell is rooted at. A PTY is the
// most consequential consumer of that decision in the process — it is a live
// interactive foothold, not a single contained read — so this does not trust
// CwdResolver's answer just because a resolver was supplied. CwdResolver is a
// pluggable func(ctx) string with no contract that its result was ever checked
// against anything; the check belongs where the consequence is.
//
// The rules are not re-derived here: vfs.ResolveSessionCwd is the ONE decision
// procedure (absolute-path guard, then the ""/"/" sentinel for "unspecified",
// then the allowlist), shared with the ACP session paths and fleet dispatch, so
// the envelope cannot be enforced in one surface and skipped in this one. The
// fallback is "" because Workspace already carries the default root; a manager
// with no allowlist has no default of its own to offer, and an empty cwd then
// leaves the shell in the serving process's working directory as before.
func (m *manager) resolveCwd(ctx context.Context) (string, error) {
	cwd := ""
	if m.cfg.CwdResolver != nil {
		cwd = m.cfg.CwdResolver(ctx)
	}
	return vfs.ResolveSessionCwd(m.cfg.Workspace, cwd, "")
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
	// Capture a brief window of output for the immediate snapshot, then return —
	// long-running commands keep streaming to subscribers and the scrollback.
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

	// Deliver the current scrollback immediately as a Reset so a reconnecting
	// client (or a freshly opened panel) repopulates from a clean slate.
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
	pty, err := startPTY(cwd, m.cfg.Shell, m.cfg.ScrubEnv)
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
			// Idle-reaped shells keep their subscribers: the session may still be
			// open in the UI and a later run re-streams from a fresh shell.
			for _, id := range idle {
				m.killShell(id, false)
			}
		}
	}
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
	})
}

var _ Manager = (*manager)(nil)
