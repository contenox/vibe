package agentinstance

import (
	"context"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/services/agenthost"
	"github.com/contenox/contenox/libacp"
)

// Instance lifecycle states — the vocabulary of InstanceStatus.State.
//
//   - StateStarting: transient, while a subprocess is (re)spawning.
//   - StateRunning: a live downstream connection.
//   - StateStopped: torn down intentionally (Stop/Close); the watchDog never
//     restarts out of this state.
//   - StateError: the downstream died unexpectedly. Terminal if restart is
//     disabled, else transient (leads back to StateStarting).
//   - StateWarning: restart was enabled but exhausted its limit, or a
//     re-spawn itself failed.
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
	// Sessions is how many downstream sessions are open on the instance —
	// always len(SessionIDs), read from the same snapshot.
	Sessions int `json:"sessions"`
	// Viewers is how many viewers are attached across those sessions,
	// independent of Sessions: an open session with nobody watching still
	// counts toward Sessions but not Viewers.
	Viewers   int       `json:"viewers"`
	StartedAt time.Time `json:"startedAt"`
	// SessionIDs lists every session currently open on the instance, sorted
	// for a deterministic snapshot. A session nobody is watching, or that
	// has emitted no update yet, is still listed.
	SessionIDs []string `json:"sessionIds"`
}

// journalingHarness is the instance's internal libacp.Client, wired into the
// downstream connection. On each session/update it first folds the update
// into the session's captured driving surface (via the driver), then
// journals and fans it out to viewers (via the hub) — that order guarantees
// an accessor read right after a viewer observes an update sees the same
// value. Permission requests and terminal/* route to the session's
// controller viewer (terminal/* only if it implements TerminalServer,
// else MethodNotFound); fs/* is left to UnimplementedClient.
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

// instanceConfig is the fully-resolved input to newInstance. The Manager
// builds it from a declared agent; the instance primitive itself never
// touches the registry.
type instanceConfig struct {
	id        string
	agentID   string
	agentName string
	kind      string

	// rootCtx is the long-lived context the subprocess is bound to (the
	// Manager's root), so the instance outlives the caller ctx that started
	// it. spawner (re)establishes the downstream connection; nil marks a
	// process-less instance.
	rootCtx context.Context
	spawner agenthost.Agent

	journalSize    int
	restartEnabled bool
	restartLimit   int

	onState            func(state string)
	onAttach           func(sessionID libacp.SessionID, viewerID string, controller bool)
	onDetach           func(sessionID libacp.SessionID, viewerID string)
	onUnsupervisedDeny func(sessionID libacp.SessionID)
	// onUnsupervisedRequest is the Manager's injected permission fallback
	// with this instance's identity already closed over (see
	// Manager.WithPermissionFallback). Nil keeps the hub's built-in deny.
	onUnsupervisedRequest func(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error)
}

// instance is one running agent instance. Its lifecycle state is guarded by
// mu; its per-session viewer/journal state lives in hub under its own lock,
// so a status read never contends with the fan-out.
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

// newInstance builds the instance and its internal harness/hub but does not
// spawn — call start for that.
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
		state:          StateStarting,
	}
}

// start brings the instance up: a spawner-less instance transitions straight
// to Running; an external one spawns the subprocess wired to the internal
// journaling harness and arms the watchDog. A spawn failure leaves the
// instance in StateError and returns the error.
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

// setState checks preds under mu and, if all pass, sets the state and fires
// onState outside the lock (so a callback into the Manager cannot deadlock).
// Returns whether the transition happened.
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

// watchDog watches one downstream connection and applies the restart policy
// when it closes. It runs once per live handle and re-arms itself on the
// fresh handle after a restart.
//
// A restart re-spawns a fresh subprocess that must be re-Initialized; the
// downstream agent's conversation/session context is lost. Viewers and the
// journal survive (they belong to the instance, not the process), but now
// describe a conversation the new process has never heard of.
func (i *instance) watchDog(h *agenthost.Handle) {
	<-h.Conn.Closed()

	i.mu.Lock()
	stopped := i.manualStop || i.closed
	current := i.handle == h // ignore a stale handle we have already replaced
	i.mu.Unlock()
	if stopped || !current {
		return
	}

	// Unexpected death: mark errored, unless a Stop raced in and already
	// claimed the terminal Stopped state.
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
		// A Stop raced in while re-spawning: abandon the fresh handle.
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
// external). manualStop is set before the handle is closed so the watchDog,
// woken by the resulting Closed signal, sees an intentional stop and neither
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

// conn returns the instance's live downstream connection, or nil for a
// spawner-less or not-yet-started instance. Internal: drive.go's
// session-driving methods issue their ACP calls on it; the Manager exposes
// no raw-connection accessor. After a watchDog restart this returns a
// different connection, since the downstream's session context was lost.
func (i *instance) conn() *libacp.ClientSideConnection {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.handle == nil {
		return nil
	}
	return i.handle.Conn
}

// status snapshots the instance, reading session facts from the driver
// (authoritative for what is open) and the viewer count from the hub
// (authoritative for who is watching) as two separate snapshots under
// separate locks — a session opening or a viewer attaching concurrently may
// land on either side, which is fine for a point-in-time report.
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

// deliverToSession injects n into sid's fan-out iff this instance currently
// owns sid, per the driver (the authoritative "session is open here" fact,
// not the hub, whose per-session state materializes lazily). Reports whether
// it owned and delivered, so the Manager's scan can find the owning
// instance. Ownership-gating matters because hub.deliver get-or-creates
// session state — delivering to an unowned session would journal it into a
// phantom session on the wrong instance. SessionID is forced to sid so a
// caller cannot misroute it within the owning instance.
func (i *instance) deliverToSession(ctx context.Context, sid libacp.SessionID, n libacp.SessionNotification) bool {
	if !i.driver.owns(sid) {
		return false
	}
	n.SessionID = sid
	i.hub.deliver(ctx, n)
	return true
}

// agentText returns the concatenated agent-message-chunk text retained in
// sid's journal, and whether this instance owns sid. The journal captures
// every downstream update whether or not a viewer is attached, so an
// owned-but-silent session yields ("", true).
func (i *instance) agentText(sid libacp.SessionID) (string, bool) {
	if !i.driver.owns(sid) {
		return "", false
	}
	return i.hub.agentText(sid), true
}

// sessionJournal returns a raw snapshot of sid's replay journal together
// with the session's working directory, and whether this instance owns sid.
// An owned-but-empty session yields (nil, cwd, true).
func (i *instance) sessionJournal(sid libacp.SessionID) ([]libacp.SessionNotification, string, bool) {
	if !i.driver.owns(sid) {
		return nil, "", false
	}
	return i.hub.journalSnapshot(sid), i.driver.cwd(sid), true
}
