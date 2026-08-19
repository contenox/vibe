package agentinstance

import (
	"context"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/services/agenthost"
	"github.com/contenox/contenox/libacp"
)

// Instance lifecycle states, the vocabulary of InstanceStatus.State.
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
	// Sessions is how many downstream sessions are open on the instance, always
	// len(SessionIDs).
	Sessions int `json:"sessions"`
	// Viewers is how many viewers are attached across those sessions, independent of
	// Sessions.
	Viewers   int       `json:"viewers"`
	StartedAt time.Time `json:"startedAt"`
	// SessionIDs lists every session currently open on the instance, sorted for a
	// deterministic snapshot; an unwatched or silent session is still listed.
	SessionIDs []string `json:"sessionIds"`
}

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

func (h *journalingHarness) ReadTextFile(ctx context.Context, req libacp.ReadTextFileRequest) (libacp.ReadTextFileResponse, error) {
	if fs := h.hub.fileSystemServer(req.SessionID); fs != nil {
		return fs.ReadTextFile(ctx, req)
	}
	return libacp.ReadTextFileResponse{}, libacp.MethodNotFound(libacp.MethodFSReadTextFile)
}

func (h *journalingHarness) WriteTextFile(ctx context.Context, req libacp.WriteTextFileRequest) (libacp.WriteTextFileResponse, error) {
	if fs := h.hub.fileSystemServer(req.SessionID); fs != nil {
		return fs.WriteTextFile(ctx, req)
	}
	return libacp.WriteTextFileResponse{}, libacp.MethodNotFound(libacp.MethodFSWriteTextFile)
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

type instanceConfig struct {
	id        string
	agentID   string
	agentName string
	kind      string

	rootCtx context.Context
	spawner agenthost.Agent

	journalSize    int
	restartEnabled bool
	restartLimit   int

	onState               func(state string)
	onAttach              func(sessionID libacp.SessionID, viewerID string, controller bool)
	onDetach              func(sessionID libacp.SessionID, viewerID string)
	onUnsupervisedDeny    func(sessionID libacp.SessionID)
	onUnsupervisedRequest func(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error)

	fileSystem     InstanceFileSystem
	terminalServer TerminalServer
}

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
	handle       *agenthost.Handle
	manualStop   bool
	restartCount int
	closed       bool
}

func newInstance(cfg instanceConfig) *instance {
	hub := newViewerHub(cfg.id, cfg.journalSize)
	hub.onAttach = cfg.onAttach
	hub.onDetach = cfg.onDetach
	hub.onUnsupervisedDeny = cfg.onUnsupervisedDeny
	hub.onUnsupervisedRequest = cfg.onUnsupervisedRequest
	hub.fileSystem = cfg.fileSystem
	hub.terminal = cfg.terminalServer
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
		return
	}
	if count >= limit {
		i.setState(StateWarning)
		return
	}

	i.setState(StateStarting, func() bool { return i.state == StateError })
	newHandle, err := i.spawner.Connect(i.rootCtx, i.harness)
	if err != nil {
		i.setState(StateWarning)
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

func (i *instance) conn() *libacp.ClientSideConnection {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.handle == nil {
		return nil
	}
	return i.handle.Conn
}

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

func (i *instance) deliverToSession(ctx context.Context, sid libacp.SessionID, n libacp.SessionNotification) bool {
	if !i.driver.owns(sid) {
		return false
	}
	n.SessionID = sid
	i.hub.deliver(ctx, n)
	return true
}

func (i *instance) agentText(sid libacp.SessionID) (string, bool) {
	if !i.driver.owns(sid) {
		return "", false
	}
	return i.hub.agentText(sid), true
}

func (i *instance) sessionJournal(sid libacp.SessionID) ([]libacp.SessionNotification, string, bool) {
	if !i.driver.owns(sid) {
		return nil, "", false
	}
	return i.hub.journalSnapshot(sid), i.driver.cwd(sid), true
}
