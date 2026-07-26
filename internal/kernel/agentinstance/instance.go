package agentinstance

import (
	"context"
	"sync"
	"time"

	"github.com/contenox/beam/internal/services/agenthost"
	"github.com/contenox/beam/libacp"
)

// Instance lifecycle states — the vocabulary of InstanceStatus.State.
//
//   - StateStarting: transient, while a subprocess is (re)spawning.
//   - StateRunning: the instance is up — a live downstream connection.
//     (A spawner-less instance, which no supported kind produces, would report
//     running with no connection; see start.)
//   - StateStopped: torn down intentionally (Stop/Close). The watchDog never
//     restarts out of this state.
//   - StateError: the downstream died UNEXPECTEDLY. Terminal when restart is
//     disabled; transient (a way-station to StateStarting) when it is enabled.
//   - StateWarning: restart was enabled but exhausted its limit (or a restart
//     re-spawn itself failed) — the "needs attention, gave up restarting" state.
const (
	StateStarting = "starting"
	StateRunning  = "running"
	StateStopped  = "stopped"
	StateError    = "error"
	StateWarning  = "warning"
)

// InstanceStatus is a point-in-time snapshot of one instance. It is a value
// copy: mutating it never affects the live instance.
type InstanceStatus struct {
	ID        string `json:"id"`
	AgentID   string `json:"agentId"`
	AgentName string `json:"agentName"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	// Sessions is how many downstream sessions are OPEN on the instance — always
	// len(SessionIDs), read from the same snapshot so the two can never disagree.
	Sessions int `json:"sessions"`
	// Viewers is how many viewers are attached across all of those sessions. It is
	// an independent fact from Sessions: an open session with nobody watching
	// contributes to Sessions and not to Viewers (that is precisely the dispatched,
	// unsupervised session adopt exists to repair).
	Viewers   int       `json:"viewers"`
	StartedAt time.Time `json:"startedAt"`
	// SessionIDs lists every session currently OPEN on the instance — opened by
	// OpenSession and not yet ended by CloseSession — sorted for a deterministic
	// snapshot (the driver's session map has no natural order). Attachment is NOT a
	// condition: a session nobody is watching, and one that has emitted no update
	// yet, are both listed.
	//
	// It is the fleet surface's substrate for session-scoped operations — a CLI or
	// REST caller resolves "cancel everything on this instance" by fanning out over
	// these ids (see fleetservice.Cancel), and acpsvc's adopt validates a requested
	// session against them. Both need the quiet sessions: a cold-loading or
	// long-reasoning session has emitted nothing yet and is exactly the one a
	// caller most wants to cancel or take control of.
	SessionIDs []string `json:"sessionIds"`
}

// journalingHarness is the instance's INTERNAL libacp.Client — the harness wired
// into the downstream connection. It replaces the K1 design where a caller
// supplied the harness: now the instance owns it, and callers ATTACH as viewers.
//
// On every downstream session/update it FIRST folds the update into the session's
// captured driving surface (config options / modes / commands — via the driver),
// THEN journals + fans it out to viewers (via the hub); the capture-before-fanout
// order means an accessor read right after a viewer observed an update sees the same
// value. It routes every session/request_permission to the session's controller
// viewer, and every terminal/* to that controller IFF it implements TerminalServer
// (else MethodNotFound). fs/* is left to UnimplementedClient (MethodNotFound).
type journalingHarness struct {
	libacp.UnimplementedClient
	hub    *viewerHub
	driver *sessionDriver
}

func (h *journalingHarness) SessionUpdate(ctx context.Context, n libacp.SessionNotification) error {
	h.driver.capture(n)
	h.hub.deliver(ctx, n)
	return nil
}

func (h *journalingHarness) RequestPermission(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	return h.hub.requestPermission(ctx, req)
}

// The terminal/* family is ROUTED to the session's controller viewer when it implements
// TerminalServer, else refused with MethodNotFound — the same routing shape as
// RequestPermission, for the second inbound-callback surface (see TerminalServer).

func (h *journalingHarness) CreateTerminal(ctx context.Context, req libacp.CreateTerminalRequest) (libacp.CreateTerminalResponse, error) {
	if ts := h.hub.terminalServer(req.SessionID); ts != nil {
		return ts.CreateTerminal(ctx, req)
	}
	return libacp.CreateTerminalResponse{}, libacp.MethodNotFound(libacp.MethodTerminalCreate)
}

func (h *journalingHarness) TerminalOutput(ctx context.Context, req libacp.TerminalOutputRequest) (libacp.TerminalOutputResponse, error) {
	if ts := h.hub.terminalServer(req.SessionID); ts != nil {
		return ts.TerminalOutput(ctx, req)
	}
	return libacp.TerminalOutputResponse{}, libacp.MethodNotFound(libacp.MethodTerminalOutput)
}

func (h *journalingHarness) WaitForTerminalExit(ctx context.Context, req libacp.WaitForTerminalExitRequest) (libacp.WaitForTerminalExitResponse, error) {
	if ts := h.hub.terminalServer(req.SessionID); ts != nil {
		return ts.WaitForTerminalExit(ctx, req)
	}
	return libacp.WaitForTerminalExitResponse{}, libacp.MethodNotFound(libacp.MethodTerminalWaitForExit)
}

func (h *journalingHarness) KillTerminal(ctx context.Context, req libacp.KillTerminalRequest) (libacp.KillTerminalResponse, error) {
	if ts := h.hub.terminalServer(req.SessionID); ts != nil {
		return ts.KillTerminal(ctx, req)
	}
	return libacp.KillTerminalResponse{}, libacp.MethodNotFound(libacp.MethodTerminalKill)
}

func (h *journalingHarness) ReleaseTerminal(ctx context.Context, req libacp.ReleaseTerminalRequest) (libacp.ReleaseTerminalResponse, error) {
	if ts := h.hub.terminalServer(req.SessionID); ts != nil {
		return ts.ReleaseTerminal(ctx, req)
	}
	return libacp.ReleaseTerminalResponse{}, libacp.MethodNotFound(libacp.MethodTerminalRelease)
}

// instanceConfig is the fully-resolved input to newInstance. The Manager builds
// it from a declared agent; the instance primitive itself never touches the
// registry — it depends only on libacp + agenthost.
type instanceConfig struct {
	id        string
	agentID   string
	agentName string
	kind      string

	// rootCtx is the long-lived context the subprocess is bound to (the Manager's
	// root), so the instance outlives the caller ctx that started it. spawner is
	// the agenthost primitive that (re)establishes the downstream connection; nil
	// marks a process-less instance (no supported agent kind produces one).
	rootCtx context.Context
	spawner agenthost.Agent

	journalSize    int
	restartEnabled bool
	restartLimit   int

	onState            func(state string)
	onAttach           func(sessionID libacp.SessionID, viewerID string, controller bool)
	onDetach           func(sessionID libacp.SessionID, viewerID string)
	onUnsupervisedDeny func(sessionID libacp.SessionID)
	// onUnsupervisedRequest is the Manager's injected permission fallback with this
	// instance's identity already closed over (see Manager.WithPermissionFallback).
	// Nil keeps the hub's built-in headless deny.
	onUnsupervisedRequest func(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error)
}

// instance is one running agent instance: the pure primitive of Layer A. Its own
// mutable lifecycle state is guarded by mu; its per-session viewer/journal state
// lives in hub (separately locked), so a status read never contends with the
// fan-out.
type instance struct {
	id        string
	agentID   string
	agentName string
	kind      string
	startedAt time.Time

	rootCtx        context.Context
	spawner        agenthost.Agent
	harness        *journalingHarness
	hub            *viewerHub
	driver         *sessionDriver
	restartEnabled bool
	restartLimit   int

	onState func(state string)

	mu           sync.Mutex
	state        string
	handle       *agenthost.Handle // nil until connected; reassigned on restart
	manualStop   bool              // set by stop(): the watchDog must never restart
	restartCount int
	closed       bool
}

// newInstance builds the instance and its internal harness/hub, but does NOT
// spawn — call start for that. The hub's attach/detach hooks are wired to the
// Manager-supplied callbacks here so viewer lifecycle reaches the event sink.
func newInstance(cfg instanceConfig) *instance {
	hub := newViewerHub(cfg.id, cfg.journalSize)
	hub.onAttach = cfg.onAttach
	hub.onDetach = cfg.onDetach
	hub.onUnsupervisedDeny = cfg.onUnsupervisedDeny
	hub.onUnsupervisedRequest = cfg.onUnsupervisedRequest
	driver := newSessionDriver()
	return &instance{
		id:             cfg.id,
		agentID:        cfg.agentID,
		agentName:      cfg.agentName,
		kind:           cfg.kind,
		startedAt:      time.Now().UTC(),
		rootCtx:        cfg.rootCtx,
		spawner:        cfg.spawner,
		harness:        &journalingHarness{hub: hub, driver: driver},
		hub:            hub,
		driver:         driver,
		restartEnabled: cfg.restartEnabled,
		restartLimit:   cfg.restartLimit,
		onState:        cfg.onState,
		// StateStarting until start() transitions it — never observed externally
		// (registration follows a successful start), but keeps a status snapshot
		// non-empty if one is ever taken during the spawn window.
		state: StateStarting,
	}
}

// start brings the instance up. For a spawner-less instance it simply
// transitions to Running. For an external one it spawns the subprocess wired to
// the internal journaling harness and arms the watchDog. A spawn failure leaves
// the instance StateError and returns the error, so the Manager can decline to
// register it (nothing leaks).
func (i *instance) start() error {
	if i.spawner == nil {
		i.setState(StateRunning)
		return nil
	}
	handle, err := i.spawner.Connect(i.rootCtx, i.harness)
	if err != nil {
		i.setState(StateError)
		return err
	}
	i.mu.Lock()
	i.handle = handle
	i.mu.Unlock()
	i.setState(StateRunning)
	go i.watchDog(handle)
	return nil
}

// setState atomically checks the predicates (each evaluated with mu held, so it
// may read i.state directly) and, if all pass, sets the state and fires the
// onState hook OUTSIDE the lock — a sink that calls back into the Manager cannot
// deadlock. Returns whether the transition happened.
func (i *instance) setState(s string, preds ...func() bool) bool {
	i.mu.Lock()
	for _, p := range preds {
		if !p() {
			i.mu.Unlock()
			return false
		}
	}
	i.state = s
	fn := i.onState
	i.mu.Unlock()
	if fn != nil {
		fn(s)
	}
	return true
}

// watchDog watches one downstream connection and applies the restart policy when
// it closes — the ACP equivalent of a supervisor waiting on
// os.Process.Wait. It runs once per live handle and re-arms itself on the fresh
// handle after a restart, so it never leaks past teardown.
//
// ACP caveat: a restart re-spawns a FRESH subprocess that must be re-Initialized
// by a driver; the downstream agent's conversation/session context is LOST.
// Viewers and the journal survive (they are the instance's, not the process's),
// but they now describe a conversation the new process has never heard of.
func (i *instance) watchDog(h *agenthost.Handle) {
	<-h.Conn.Closed()

	i.mu.Lock()
	stopped := i.manualStop || i.closed
	current := i.handle == h // ignore a stale handle we have already replaced
	i.mu.Unlock()
	if stopped || !current {
		return
	}

	// Unexpected death: mark errored, unless a Stop raced in and already claimed
	// the terminal Stopped state.
	i.setState(StateError, func() bool {
		return i.state == StateRunning || i.state == StateStarting
	})

	i.mu.Lock()
	raced := i.manualStop || i.closed
	enabled := i.restartEnabled
	count := i.restartCount
	limit := i.restartLimit
	i.mu.Unlock()
	if raced || !enabled {
		return // crash-terminal: stays StateError when restart is disabled
	}
	if count >= limit {
		i.setState(StateWarning) // restart budget exhausted
		return
	}

	i.setState(StateStarting, func() bool { return i.state == StateError })
	newHandle, err := i.spawner.Connect(i.rootCtx, i.harness)
	if err != nil {
		i.setState(StateWarning) // could not re-spawn
		return
	}

	i.mu.Lock()
	if i.manualStop || i.closed {
		// A Stop raced in while we were re-spawning: abandon the fresh handle.
		i.mu.Unlock()
		_ = newHandle.Close()
		return
	}
	i.handle = newHandle
	i.restartCount = count + 1
	i.mu.Unlock()

	i.setState(StateRunning, func() bool { return i.state == StateStarting })
	go i.watchDog(newHandle)
}

// stop transitions the instance to Stopped and tears down its subprocess (if
// external). manualStop is set BEFORE the handle is closed so the watchDog, which
// wakes on the resulting Closed signal, sees an intentional stop and neither
// mislabels it Error nor restarts it. Idempotent.
func (i *instance) stop() error {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return nil
	}
	i.closed = true
	i.manualStop = true
	i.state = StateStopped
	h := i.handle
	fn := i.onState
	i.mu.Unlock()

	if fn != nil {
		fn(StateStopped)
	}
	if h != nil {
		return h.Close()
	}
	return nil
}

// conn returns the instance's live downstream connection, or nil for a spawner-less or
// not-yet-started instance. It is INTERNAL: the instance's own session-driving methods
// (openSession/promptSession/... in drive.go) issue their ACP calls on it, and white-box
// tests use it directly. The Manager exposes NO raw-connection accessor — driving goes
// through the kernel's session-driving API so no consumer holds the connection.
//
// NOTE: after a watchDog restart this returns a DIFFERENT connection (the fresh
// subprocess); the initialize-once handshake (ensureInitialized) re-arms on the new
// connection because the downstream's session context was lost — see the package doc.
func (i *instance) conn() *libacp.ClientSideConnection {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.handle == nil {
		return nil
	}
	return i.handle.Conn
}

// status snapshots the instance, reading each fact from whichever half OWNS it: the
// SESSION facts from the driver, which is authoritative for what is open (its entry is
// seeded by OpenSession and dropped by CloseSession), and the VIEWER fact from the hub,
// which is authoritative for who is watching. Sourcing sessions from the hub — whose
// per-session state materializes lazily on a first update or first attach — used to hide
// every open-but-silent session from the fleet surface.
//
// The two are read as separate snapshots under separate locks, so a session opening or a
// viewer attaching concurrently may land on either side of the boundary. That is fine: a
// status is a point-in-time report, not a transaction, and taking one lock across both
// would couple the fan-out path to the status path for no gain.
func (i *instance) status() InstanceStatus {
	i.mu.Lock()
	state := i.state
	started := i.startedAt
	i.mu.Unlock()
	sessionIDs := i.driver.sessionIDs()
	return InstanceStatus{
		ID:         i.id,
		AgentID:    i.agentID,
		AgentName:  i.agentName,
		Kind:       i.kind,
		State:      state,
		Sessions:   len(sessionIDs),
		Viewers:    i.hub.viewerCount(),
		StartedAt:  started,
		SessionIDs: sessionIDs,
	}
}

func (i *instance) attach(ctx context.Context, sessionID libacp.SessionID, viewer Viewer) (bool, error) {
	return i.hub.attach(ctx, sessionID, viewer)
}

func (i *instance) detach(sessionID libacp.SessionID, viewerID string) error {
	return i.hub.detach(sessionID, viewerID)
}

// deliverToSession injects n into sid's fan-out IFF this instance currently OWNS
// sid — i.e. the driver holds it, the authoritative "this session is open here"
// fact (sessionDriver.owns / .sessionIDs), NOT the hub, whose per-session state
// materializes only lazily on a first update or attach. It reports whether it
// owned and delivered, so the Manager's scan can find the one instance that does.
//
// Ownership-gating is load-bearing: hub.deliver get-or-creates session state, so
// delivering to a session this instance does NOT own would journal the update
// into a phantom session on the wrong instance. Gated, an injected update goes
// only to the instance that actually hosts the session, where it is journaled
// (a viewer attaching later replays it) and fanned out to current viewers
// exactly like a downstream update — the SessionID is forced to sid so a caller
// cannot misroute it within the owning instance.
func (i *instance) deliverToSession(ctx context.Context, sid libacp.SessionID, n libacp.SessionNotification) bool {
	if !i.driver.owns(sid) {
		return false
	}
	n.SessionID = sid
	i.hub.deliver(ctx, n)
	return true
}

// agentText returns the concatenated agent-message-chunk text retained in sid's
// journal, and whether this instance OWNS sid. Ownership is the driver's
// authoritative fact (as everywhere in this package — sessionIDs/owns/
// deliverToSession), while the TEXT comes from the hub's journal, which captures
// every downstream session/update whether or not a viewer is attached: a
// dispatched, unwatched unit's words are journaled all the same, which is why
// they are recoverable here without anyone having attached. An owned-but-silent
// session yields ("", true) — recoverable, but nothing said.
func (i *instance) agentText(sid libacp.SessionID) (string, bool) {
	if !i.driver.owns(sid) {
		return "", false
	}
	return i.hub.agentText(sid), true
}

// sessionJournal returns a raw snapshot of sid's replay journal together with
// the session's working directory, and whether this instance OWNS sid. Same
// ownership-from-driver, data-from-hub split as agentText: ownership is the
// driver's authoritative fact, while the journal comes from the hub (which
// captures every downstream update whether or not a viewer is attached, so a
// dispatched, unwatched unit's diffs are journaled all the same). The cwd is the
// driver's recorded SessionSpec.Cwd. An owned-but-empty session yields
// (nil, cwd, true).
func (i *instance) sessionJournal(sid libacp.SessionID) ([]libacp.SessionNotification, string, bool) {
	if !i.driver.owns(sid) {
		return nil, "", false
	}
	return i.hub.journalSnapshot(sid), i.driver.cwd(sid), true
}
