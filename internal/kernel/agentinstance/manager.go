package agentinstance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/services/agenthost"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	"github.com/google/uuid"
)

const defaultKillGrace = 2 * time.Second

// ChainACPSubcommand, ChainPathEnvVar, ChainHopEnvVar, and ChainWorkspaceFlag describe
// this binary's own ACP server for a chain-kind spawn: its ACP subcommand, the env var
// naming the chain file, the env var carrying the dispatch hop, and the flag carrying the
// dispatching host's workspace id.
const (
	ChainACPSubcommand = "acp"
	ChainPathEnvVar    = "CONTENOX_ACP_CHAIN_PATH"
	ChainHopEnvVar     = "CONTENOX_EVENT_HOP"
	ChainWorkspaceFlag = "workspace-id"
)

// ErrNotFound is returned for an unknown instance id; a sentinel for
// errors.Is(err, ErrNotFound).
var ErrNotFound = errors.New("agentinstance: instance not found")

// EventKind classifies a lifecycle Event.
type EventKind string

const (
	// EventStateChange fires on every instance state transition (Event.State
	// carries the new state).
	EventStateChange EventKind = "state_change"
	// EventAttach fires when a viewer attaches to a session (Event.Controller
	// reports whether it became the controller).
	EventAttach EventKind = "attach"
	// EventDetach fires when a viewer detaches from a session.
	EventDetach EventKind = "detach"
	// EventUnsupervisedDeny fires when a downstream permission request reaches a
	// session with no controller and is refused (built-in deny or an injected
	// PermissionFallback); it never fires for a permitted request.
	EventUnsupervisedDeny EventKind = "unsupervised_permission"
)

// Event is one instance-lifecycle event, self-contained so a sink can react without
// calling back into the Manager; subscribe via WithEventSink.
type Event struct {
	Kind       EventKind        `json:"kind"`
	InstanceID string           `json:"instanceId"`
	AgentID    string           `json:"agentId"`
	AgentName  string           `json:"agentName"`
	State      string           `json:"state,omitempty"`      // EventStateChange
	SessionID  libacp.SessionID `json:"sessionId,omitempty"`  // EventAttach / EventDetach / EventUnsupervisedDeny
	ViewerID   string           `json:"viewerId,omitempty"`   // EventAttach / EventDetach
	Controller bool             `json:"controller,omitempty"` // EventAttach
	Time       time.Time        `json:"time"`
}

// UnattendedPermission is the self-contained input to a PermissionFallback:
// one downstream permission request that reached a session with no
// controller viewer, plus the identity of the instance that raised it.
type UnattendedPermission struct {
	InstanceID string
	AgentID    string
	AgentName  string
	// SessionID is the downstream session the request arrived on.
	SessionID libacp.SessionID
	Request   libacp.RequestPermissionRequest
}

// PermissionFallback answers a permission request that arrived at an unattended
// session; it may block (runs on the request's own goroutine), and an error falls
// back to the built-in deny.
type PermissionFallback func(ctx context.Context, req UnattendedPermission) (libacp.RequestPermissionResponse, error)

// EventSink receives every lifecycle Event synchronously on the producing goroutine;
// it must not block or call back into the Manager.
type EventSink func(Event)

// FleetEntry joins one declared agent with its live instances (empty when
// declared but not running).
type FleetEntry struct {
	AgentID   string           `json:"agentId"`
	AgentName string           `json:"agentName"`
	Kind      string           `json:"kind"`
	Instances []InstanceStatus `json:"instances"`
}

// Running reports whether this declared agent has at least one live instance.
func (e FleetEntry) Running() bool { return len(e.Instances) > 0 }

// Manager owns the lifecycle of running agent instances; every method is safe for
// concurrent use and returns ErrNotFound for an unknown instanceID.
type Manager interface {
	// Start resolves agentName via the registry and brings up an instance bound to the
	// Manager's root context (cwd is the sandbox workspace); prefer StartResolved if
	// already resolved.
	Start(ctx context.Context, agentName, cwd string) (instanceID string, err error)

	// StartResolved spawns an instance from an already-resolved agent with no registry
	// read of its own; cwd is the sandbox workspace root, overridden by the agent's own
	// declared Cwd if set.
	StartResolved(ctx context.Context, agent *runtimetypes.Agent, cwd string) (instanceID string, err error)

	// Attach registers viewer against (instanceID, sessionID), replaying the journal
	// then joining the live fan-out; the first viewer of a session becomes its controller.
	Attach(ctx context.Context, instanceID string, sessionID libacp.SessionID, viewer Viewer) (controllerGranted bool, err error)

	// Detach removes viewerID from (instanceID, sessionID)'s fan-out,
	// promoting the earliest-attached survivor if it was the controller.
	Detach(instanceID string, sessionID libacp.SessionID, viewerID string) error

	// List returns every declared agent joined with its live instances
	// (empty = not running).
	List(ctx context.Context) ([]FleetEntry, error)

	// Get returns the status of one instance.
	Get(instanceID string) (InstanceStatus, error)

	// OpenSession drives the downstream ACP handshake on instanceID
	// (initialize once, then session/new) and returns the downstream
	// session id that Attach and the other session methods use.
	OpenSession(ctx context.Context, instanceID string, spec SessionSpec) (libacp.SessionID, error)

	// Prompt drives one downstream session/prompt turn and returns its stop reason; a
	// ctx cancellation or concurrent Cancel resolves as StopReasonCancelled with a nil
	// error.
	Prompt(ctx context.Context, instanceID string, sessionID libacp.SessionID, prompt []libacp.ContentBlock) (libacp.StopReason, error)

	// DeliverToSession injects n into sessionID's fan-out on whichever live instance
	// owns that session, as if it were a downstream update; ErrNotFound when no instance
	// owns it.
	DeliverToSession(ctx context.Context, sessionID libacp.SessionID, n libacp.SessionNotification) error

	// Cancel cancels sessionID's in-flight prompt turn. Safe with no turn in flight.
	Cancel(instanceID string, sessionID libacp.SessionID) error

	// CloseSession ends sessionID and drops its kernel state without stopping the
	// instance; only the consumer that called OpenSession should call this.
	CloseSession(instanceID string, sessionID libacp.SessionID) error

	// SetConfigOption forwards a config-option change downstream and adopts the
	// confirmed value; the synthetic mode/model ids map to session/set_mode and
	// session/set_model, every other id forwards to session/set_config_option.
	SetConfigOption(ctx context.Context, instanceID string, sessionID libacp.SessionID, configID string, value libacp.SessionConfigOptionValue) error

	// SessionConfigOptions returns sessionID's captured config-option
	// surface (synthetic mode + model selects, then the downstream's own),
	// or nil for an unknown session.
	SessionConfigOptions(instanceID string, sessionID libacp.SessionID) ([]libacp.SessionConfigOption, error)

	// AvailableCommands returns sessionID's captured slash-command menu, or nil for an unknown session.
	AvailableCommands(instanceID string, sessionID libacp.SessionID) ([]libacp.AvailableCommand, error)

	// Stop tears an instance down and removes it from the registry, preventing any
	// watchDog restart. Idempotent.
	Stop(instanceID string) error

	// Close stops every instance and cancels the Manager's root context, after which
	// Start returns an error. Idempotent.
	Close() error
}

// Option configures a Manager.
type Option func(*manager)

// WithStderr forwards each spawned external instance's subprocess stderr to w; defaults
// to io.Discard.
func WithStderr(w io.Writer) Option { return func(m *manager) { m.stderr = w } }

// WithKillGrace overrides how long an external instance's teardown waits for its
// subprocess to exit on stdin-close before killing it (see defaultKillGrace).
func WithKillGrace(d time.Duration) Option { return func(m *manager) { m.killGrace = d } }

// WithJournalSize overrides the per-session replay journal size (see
// defaultJournalSize).
func WithJournalSize(n int) Option { return func(m *manager) { m.journalSize = n } }

// WithRestart enables the watchDog restart policy: a dying external instance is
// re-spawned up to limit times before parking in StateWarning (default: disabled,
// terminal StateError); a restart loses the downstream conversation context.
func WithRestart(limit int) Option {
	return func(m *manager) {
		m.restartEnabled = true
		m.restartLimit = limit
	}
}

// WithEventSink installs sink as the lifecycle event sink (see EventSink).
func WithEventSink(sink EventSink) Option { return func(m *manager) { m.sink = sink } }

// WithPermissionFallback installs fn as the answerer for permission requests reaching a
// session with no controller viewer; default unset keeps the built-in deny.
func WithPermissionFallback(fn PermissionFallback) Option {
	return func(m *manager) { m.permissionFallback = fn }
}

// WithSelfExecutable overrides the program a chain-kind agent is spawned from (default
// os.Executable()), letting a test point the spawn at a built fixture binary.
func WithSelfExecutable(path string) Option {
	return func(m *manager) { m.selfExecutable = path }
}

// WithWorkspaceID passes id to every chain-kind spawn via ChainWorkspaceFlag so the
// child's mission-event publisher stamps the dispatching host's workspace; empty
// (default) adds no flag.
func WithWorkspaceID(id string) Option {
	return func(m *manager) { m.workspaceID = id }
}

type manager struct {
	agents      agentregistryservice.Service
	stderr      io.Writer
	killGrace   time.Duration
	journalSize int

	selfExecutable string

	workspaceID string

	restartEnabled     bool
	restartLimit       int
	sink               EventSink
	permissionFallback PermissionFallback

	rootCtx    context.Context
	rootCancel context.CancelFunc

	mu        sync.Mutex
	instances map[string]*instance
	closed    bool
}

// New returns a Manager that resolves declared agents via agents, owning a fresh root
// context; call Close to tear everything down at shutdown.
func New(agents agentregistryservice.Service, opts ...Option) Manager {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	m := &manager{
		agents:      agents,
		stderr:      io.Discard,
		killGrace:   defaultKillGrace,
		journalSize: defaultJournalSize,
		rootCtx:     rootCtx,
		rootCancel:  rootCancel,
		instances:   make(map[string]*instance),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

var _ Manager = (*manager)(nil)

func (m *manager) emit(ev Event) {
	if m.sink != nil {
		m.sink(ev)
	}
}

func (m *manager) Start(ctx context.Context, agentName, cwd string) (string, error) {
	if agentName == "" {
		return "", fmt.Errorf("agentinstance: agentName is required")
	}
	agent, err := m.agents.GetByName(ctx, agentName)
	if err != nil {
		return "", fmt.Errorf("agentinstance: resolve agent %q: %w", agentName, err)
	}
	return m.StartResolved(ctx, agent, cwd)
}

func (m *manager) StartResolved(ctx context.Context, agent *runtimetypes.Agent, cwd string) (string, error) {
	if agent == nil {
		return "", fmt.Errorf("agentinstance: agent is required")
	}
	switch agent.Kind {
	case runtimetypes.AgentKindExternalACP:
		cfg, err := agent.ExternalACPConfig()
		if err != nil {
			return "", fmt.Errorf("agentinstance: agent %q: %w", agent.Name, err)
		}
		if cfg.Cwd == "" {
			cfg.Cwd = cwd
		}
		spawner := &agenthost.ExternalACPAgent{
			Config:    *cfg,
			Stderr:    m.stderr,
			KillGrace: m.killGrace,
		}
		return m.bringUp(agent, spawner)
	case runtimetypes.AgentKindChain:
		// ctx, not the Manager's root: the hop rides with the dispatch that asked for
		// this spawn.
		spawner, err := m.chainSpawner(ctx, agent, cwd)
		if err != nil {
			return "", err
		}
		return m.bringUp(agent, spawner)
	default:
		return "", fmt.Errorf("agentinstance: agent %q has unsupported kind %q", agent.Name, agent.Kind)
	}
}

func (m *manager) chainSpawner(ctx context.Context, agent *runtimetypes.Agent, cwd string) (agenthost.Agent, error) {
	cfg, err := agent.ChainConfig()
	if err != nil {
		return nil, fmt.Errorf("agentinstance: agent %q: %w", agent.Name, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("agentinstance: agent %q: %w", agent.Name, err)
	}
	self := m.selfExecutable
	if self == "" {
		self, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("agentinstance: agent %q: resolve this executable to run its chain: %w", agent.Name, err)
		}
	}
	args := []string{ChainACPSubcommand}
	if m.workspaceID != "" {
		args = append(args, "--"+ChainWorkspaceFlag, m.workspaceID)
	}
	env := map[string]string{ChainPathEnvVar: cfg.Path}
	// Verbatim, not incremented: this child's events belong at the same hop the
	// dispatcher already stamped.
	if hop := runtimetypes.EventHopFromContext(ctx); hop > 0 {
		env[ChainHopEnvVar] = strconv.Itoa(hop)
	}
	return &agenthost.ExternalACPAgent{
		Config: runtimetypes.ExternalACPConfig{
			Transport: runtimetypes.ExternalACPTransportStdio,
			Command:   self,
			Args:      args,
			Cwd:       cwd,
			Env:       env,
		},
		// Contenox spawning contenox runs outside the sandbox (SelfSpawn); the in-process
		// capability grants and the HITL gate still govern it.
		SelfSpawn: true,
		Stderr:    m.stderr,
		KillGrace: m.killGrace,
	}, nil
}

func (m *manager) bringUp(agent *runtimetypes.Agent, spawner agenthost.Agent) (string, error) {
	id := uuid.NewString()
	inst := newInstance(instanceConfig{
		id:             id,
		agentID:        agent.ID,
		agentName:      agent.Name,
		kind:           agent.Kind,
		rootCtx:        m.rootCtx,
		spawner:        spawner,
		journalSize:    m.journalSize,
		restartEnabled: m.restartEnabled,
		restartLimit:   m.restartLimit,
		onState: func(state string) {
			m.emit(Event{Kind: EventStateChange, InstanceID: id, AgentID: agent.ID, AgentName: agent.Name, State: state, Time: time.Now().UTC()})
		},
		onAttach: func(sessionID libacp.SessionID, viewerID string, controller bool) {
			m.emit(Event{Kind: EventAttach, InstanceID: id, AgentID: agent.ID, AgentName: agent.Name, SessionID: sessionID, ViewerID: viewerID, Controller: controller, Time: time.Now().UTC()})
		},
		onDetach: func(sessionID libacp.SessionID, viewerID string) {
			m.emit(Event{Kind: EventDetach, InstanceID: id, AgentID: agent.ID, AgentName: agent.Name, SessionID: sessionID, ViewerID: viewerID, Time: time.Now().UTC()})
		},
		onUnsupervisedDeny: func(sessionID libacp.SessionID) {
			m.emit(Event{Kind: EventUnsupervisedDeny, InstanceID: id, AgentID: agent.ID, AgentName: agent.Name, SessionID: sessionID, Time: time.Now().UTC()})
		},
		onUnsupervisedRequest: m.unattendedPermissionAnswerer(id, agent),
	})

	if err := inst.start(); err != nil {
		return "", fmt.Errorf("agentinstance: start agent %q: %w", agent.Name, err)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = inst.stop()
		return "", fmt.Errorf("agentinstance: manager is closed")
	}
	m.instances[id] = inst
	m.mu.Unlock()
	return id, nil
}

func (m *manager) unattendedPermissionAnswerer(instanceID string, agent *runtimetypes.Agent) func(context.Context, libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	fn := m.permissionFallback
	if fn == nil {
		return nil
	}
	agentID, agentName := agent.ID, agent.Name
	return func(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
		return fn(ctx, UnattendedPermission{
			InstanceID: instanceID,
			AgentID:    agentID,
			AgentName:  agentName,
			SessionID:  req.SessionID,
			Request:    req,
		})
	}
}

func (m *manager) Attach(ctx context.Context, instanceID string, sessionID libacp.SessionID, viewer Viewer) (bool, error) {
	m.mu.Lock()
	inst, ok := m.instances[instanceID]
	m.mu.Unlock()
	if !ok {
		return false, fmt.Errorf("agentinstance: %q: %w", instanceID, ErrNotFound)
	}
	return inst.attach(ctx, sessionID, viewer)
}

func (m *manager) Detach(instanceID string, sessionID libacp.SessionID, viewerID string) error {
	m.mu.Lock()
	inst, ok := m.instances[instanceID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("agentinstance: %q: %w", instanceID, ErrNotFound)
	}
	return inst.detach(sessionID, viewerID)
}

func (m *manager) Get(instanceID string) (InstanceStatus, error) {
	m.mu.Lock()
	inst, ok := m.instances[instanceID]
	m.mu.Unlock()
	if !ok {
		return InstanceStatus{}, fmt.Errorf("agentinstance: %q: %w", instanceID, ErrNotFound)
	}
	return inst.status(), nil
}

func (m *manager) instance(instanceID string) (*instance, error) {
	m.mu.Lock()
	inst, ok := m.instances[instanceID]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("agentinstance: %q: %w", instanceID, ErrNotFound)
	}
	return inst, nil
}

func (m *manager) OpenSession(ctx context.Context, instanceID string, spec SessionSpec) (libacp.SessionID, error) {
	inst, err := m.instance(instanceID)
	if err != nil {
		return "", err
	}
	return inst.openSession(ctx, spec)
}

func (m *manager) Prompt(ctx context.Context, instanceID string, sessionID libacp.SessionID, prompt []libacp.ContentBlock) (libacp.StopReason, error) {
	inst, err := m.instance(instanceID)
	if err != nil {
		return "", err
	}
	return inst.promptSession(ctx, sessionID, prompt)
}

// DeliverToSession scans for the instance that owns sessionID and injects n into its
// fan-out; ErrNotFound if none does.
func (m *manager) DeliverToSession(ctx context.Context, sessionID libacp.SessionID, n libacp.SessionNotification) error {
	if sessionID == "" {
		return fmt.Errorf("agentinstance: sessionID is required")
	}
	m.mu.Lock()
	insts := make([]*instance, 0, len(m.instances))
	for _, inst := range m.instances {
		insts = append(insts, inst)
	}
	m.mu.Unlock()

	for _, inst := range insts {
		if inst.deliverToSession(ctx, sessionID, n) {
			return nil
		}
	}
	return fmt.Errorf("agentinstance: session %q: %w", sessionID, ErrNotFound)
}

func (m *manager) Cancel(instanceID string, sessionID libacp.SessionID) error {
	inst, err := m.instance(instanceID)
	if err != nil {
		return err
	}
	return inst.cancelSession(sessionID)
}

func (m *manager) CloseSession(instanceID string, sessionID libacp.SessionID) error {
	inst, err := m.instance(instanceID)
	if err != nil {
		return err
	}
	return inst.closeSession(sessionID)
}

func (m *manager) SetConfigOption(ctx context.Context, instanceID string, sessionID libacp.SessionID, configID string, value libacp.SessionConfigOptionValue) error {
	inst, err := m.instance(instanceID)
	if err != nil {
		return err
	}
	return inst.setConfigOption(ctx, sessionID, configID, value)
}

func (m *manager) SessionConfigOptions(instanceID string, sessionID libacp.SessionID) ([]libacp.SessionConfigOption, error) {
	inst, err := m.instance(instanceID)
	if err != nil {
		return nil, err
	}
	return inst.sessionConfigOptions(sessionID), nil
}

func (m *manager) AvailableCommands(instanceID string, sessionID libacp.SessionID) ([]libacp.AvailableCommand, error) {
	inst, err := m.instance(instanceID)
	if err != nil {
		return nil, err
	}
	return inst.availableCommands(sessionID), nil
}

// SessionAgentText returns the concatenated agent-message text retained in sessionID's
// replay journal on instanceID; ok is false for an unknown instance or unowned session.
func (m *manager) SessionAgentText(instanceID string, sessionID libacp.SessionID) (string, bool) {
	inst, err := m.instance(instanceID)
	if err != nil {
		return "", false
	}
	return inst.agentText(sessionID)
}

// SessionJournal returns a raw snapshot of sessionID's replay journal on instanceID
// together with its working directory; ok is false for an unknown instance or unowned
// session.
func (m *manager) SessionJournal(instanceID string, sessionID libacp.SessionID) ([]libacp.SessionNotification, string, bool) {
	inst, err := m.instance(instanceID)
	if err != nil {
		return nil, "", false
	}
	return inst.sessionJournal(sessionID)
}

func (m *manager) List(ctx context.Context) ([]FleetEntry, error) {
	m.mu.Lock()
	live := make([]*instance, 0, len(m.instances))
	for _, inst := range m.instances {
		live = append(live, inst)
	}
	m.mu.Unlock()

	byAgent := make(map[string][]InstanceStatus)
	for _, inst := range live {
		st := inst.status()
		byAgent[st.AgentID] = append(byAgent[st.AgentID], st)
	}

	declared, err := m.listDeclared(ctx)
	if err != nil {
		return nil, fmt.Errorf("agentinstance: list declared agents: %w", err)
	}

	entries := make([]FleetEntry, 0, len(declared))
	seen := make(map[string]bool, len(declared))
	for _, a := range declared {
		seen[a.ID] = true
		entries = append(entries, FleetEntry{
			AgentID:   a.ID,
			AgentName: a.Name,
			Kind:      a.Kind,
			Instances: byAgent[a.ID],
		})
	}
	for agentID, insts := range byAgent {
		if seen[agentID] {
			continue
		}
		entries = append(entries, FleetEntry{
			AgentID:   agentID,
			AgentName: insts[0].AgentName,
			Kind:      insts[0].Kind,
			Instances: insts,
		})
	}
	return entries, nil
}

func (m *manager) listDeclared(ctx context.Context) ([]*runtimetypes.Agent, error) {
	const page = 200
	var all []*runtimetypes.Agent
	var cursor *time.Time
	for {
		batch, err := m.agents.List(ctx, cursor, page)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < page {
			break
		}
		last := batch[len(batch)-1].CreatedAt
		if cursor != nil && !last.Before(*cursor) {
			break
		}
		cursor = &last
	}
	return all, nil
}

func (m *manager) Stop(instanceID string) error {
	m.mu.Lock()
	inst, ok := m.instances[instanceID]
	if ok {
		delete(m.instances, instanceID)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return inst.stop()
}

func (m *manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	insts := make([]*instance, 0, len(m.instances))
	for id, inst := range m.instances {
		insts = append(insts, inst)
		delete(m.instances, id)
	}
	m.mu.Unlock()

	var errs []error
	for _, inst := range insts {
		if err := inst.stop(); err != nil {
			errs = append(errs, err)
		}
	}
	m.rootCancel()
	return errors.Join(errs...)
}
