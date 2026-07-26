package agentinstance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/contenox/beam/internal/services/agenthost"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
	"github.com/google/uuid"
)

// defaultKillGrace bounds how long an external instance's teardown waits for its
// subprocess to exit on stdin-close before killing it. Persistent ACP agents
// (testy, claude-code-acp) never exit on stdin-close, so a short grace keeps
// Stop/Close from stalling the full acpexec default per instance.
const defaultKillGrace = 2 * time.Second

// The two facts a chain-kind spawn needs about this binary's own ACP server:
// ChainACPSubcommand serves ACP over stdio, and ChainPathEnvVar tells it which
// chain file to execute.
//
// They are DECLARED here rather than imported because the packages that own
// them (the CLI's acp command and the ACP service's chain registry) both sit
// ABOVE this kernel and one of them imports it — importing back would be a
// cycle, and reaching into either would put transport knowledge in a kernel
// that is deliberately ACP-generic. They are EXPORTED so the owning packages
// can assert the definitions still agree, which is what keeps the duplication
// from drifting silently.
const (
	ChainACPSubcommand = "acp"
	ChainPathEnvVar    = "CONTENOX_ACP_CHAIN_PATH"
)

// ErrNotFound is returned for an unknown instance id. It is a sentinel so callers
// can branch on errors.Is(err, ErrNotFound).
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
	// EventUnsupervisedDeny fires when a downstream permission request arrives at a
	// session with no attached controller and is REFUSED — by the kernel's built-in
	// headless deny (returned as a graceful "cancelled"), or by an injected
	// PermissionFallback that selected a reject option or failed to decide. It is
	// passive audit: a permission-gated action was refused with no one watching.
	//
	// It deliberately does NOT fire when a fallback PERMITS the request. Once the
	// decision can go either way (see WithPermissionFallback), an event that fired
	// on every unattended request would claim refusals that never happened, and the
	// audit trail's one job is to be true.
	EventUnsupervisedDeny EventKind = "unsupervised_permission"
)

// Event is one instance-lifecycle event. It is SELF-CONTAINED — every field a
// consumer needs is on the Event, so a sink reacts to it without calling back
// into the Manager (and thus cannot deadlock or race registration). This is the
// substrate a future scheduler (cron/bus → Start) and beam's live fleet view
// both hang off: the Manager fires an Event on every state change / attach /
// detach, and those consumers subscribe via WithEventSink.
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

// UnattendedPermission is the SELF-CONTAINED input to a PermissionFallback: one
// downstream session/request_permission that reached a session with NO controller
// viewer, plus the identity of the unit that raised it. Like Event it carries
// everything a consumer needs, so an answerer never calls back into the Manager to
// find out who is asking (and thus cannot deadlock the session it is answering for).
//
// The identity is the same one the lifecycle Events carry, closed over at bring-up:
// the kernel does no lookup of its own for it.
type UnattendedPermission struct {
	InstanceID string
	AgentID    string
	AgentName  string
	// SessionID is the DOWNSTREAM session the request arrived on — the same id
	// Request.SessionID carries, lifted out so an answerer need not reach into the
	// wire type for the one field it always wants.
	SessionID libacp.SessionID
	Request   libacp.RequestPermissionRequest
}

// PermissionFallback answers a permission request that arrived at an unattended
// session — the headless case a dispatched unit runs in BY DESIGN, where there is
// no controller viewer to ask. It is installed via WithPermissionFallback and is
// the kernel's ONLY concession to human-in-the-loop: the kernel calls it and
// returns whatever it produces, knowing nothing about how the answer was reached
// (a policy envelope, a durable ask, an operator's inbox — all service-layer
// concerns; see the package doc's policy-free invariant).
//
// It MAY block: like Viewer.RequestPermission it runs on the request's own
// dispatched goroutine, so waiting for a human is legitimate. ctx is the
// downstream request's context, so a cancelled turn releases the wait.
//
// Returning an error is NOT a way to fault the downstream: the kernel falls back
// to its built-in headless deny (a graceful "cancelled" outcome), so an answerer
// that cannot decide is never more disruptive than having no answerer at all.
type PermissionFallback func(ctx context.Context, req UnattendedPermission) (libacp.RequestPermissionResponse, error)

// EventSink receives every lifecycle Event the Manager fires. It is called
// synchronously on the goroutine that produced the event (a state transition, an
// attach, a detach), so it MUST NOT block or call back into the Manager; enqueue
// and return. A future scheduler/fleet-view adapts its own subscription on top of
// this one seam.
type EventSink func(Event)

// FleetEntry is one row of the config+runtime join List returns: a DECLARED
// agent, annotated with its live instances (empty when the agent is declared but
// not running). It is the fleet surface's substrate — it shows declared-but-idle
// agents, not only live ones.
type FleetEntry struct {
	AgentID   string           `json:"agentId"`
	AgentName string           `json:"agentName"`
	Kind      string           `json:"kind"`
	Instances []InstanceStatus `json:"instances"`
}

// Running reports whether this declared agent has at least one live instance.
func (e FleetEntry) Running() bool { return len(e.Instances) > 0 }

// Manager owns the lifecycle of running agent instances. Every method is safe for
// concurrent use.
type Manager interface {
	// Start resolves the declared agent named agentName via the registry service
	// and brings up an instance for it. For an external_acp agent it spawns the
	// subprocess (agenthost) bound to the MANAGER's root context — not ctx — so
	// the instance outlives the request that started it; ctx governs only the
	// registry lookup. The instance is wired to its OWN internal journaling
	// harness (callers ATTACH as viewers, they no longer supply the harness). For
	// a chain agent it spawns THIS binary's own ACP server bound to the declared
	// chain file — the same subprocess mechanism, pointed at ourselves. Returns
	// the generated instance id, or an error if the agent cannot be resolved or
	// spawned (in which case nothing is registered and no process is leaked).
	//
	// It is Start-by-NAME: a convenience over StartResolved for a caller that has
	// only a name and no policy to apply. A caller that ALREADY resolved the agent —
	// because it had a policy decision to make about the record, as every spawn path
	// with an Enabled check does — must call StartResolved instead; see there.
	//
	// cwd is the sandbox workspace the spawned agent is confined to; it is passed
	// through to StartResolved. See there.
	Start(ctx context.Context, agentName, cwd string) (instanceID string, err error)

	// StartResolved spawns an instance from an ALREADY-RESOLVED declared agent and
	// performs NO registry read of its own. Start is implemented as resolve +
	// StartResolved, so there is exactly one spawn implementation and this is it.
	//
	// It exists because the service layer above resolves the record anyway in order
	// to make a policy decision about it (agentregistryservice.ResolveForSpawn — the
	// one shared "refuse a disabled agent" judgement). Having the kernel then re-read
	// the same row was both a wasted query per spawn and a TOCTOU window: the Enabled
	// check was made against the first read while the spawn proceeded from the second,
	// so an agent disabled in between still spawned. Handing the kernel the record the
	// decision was made about closes the window by construction — the bytes that were
	// judged are the bytes that are spawned.
	//
	// This is also the correct shape for the layering: the kernel is policy-free and
	// spawns what it is handed; it has no business re-deriving what the service layer
	// already established. Returns an error for a nil agent or an unsupported kind.
	//
	// cwd is the sandbox workspace root the agent is confined to (its one writable
	// root — agenthost fail-closes on an empty one). It is the caller's already
	// resolved session/mission cwd; an EXTERNAL agent's own declared Config.Cwd wins
	// when set, and a CHAIN agent (which has no declared cwd) always takes this one.
	// This mirrors the defaulting the one-shot DriveTurn path does (agenthost/drive.go),
	// so both spawn shapes confine to the workspace the caller resolved rather than
	// leaving the wall without one.
	StartResolved(ctx context.Context, agent *runtimetypes.Agent, cwd string) (instanceID string, err error)

	// Attach registers viewer against (instanceID, sessionID): it replays that
	// session's journal to the viewer, then joins it to the live fan-out. The
	// first viewer of a session with no controller becomes the controller
	// (controllerGranted true) and thereby answers the downstream agent's
	// permission requests; later viewers are observers (false). Returns
	// ErrNotFound for an unknown instance, or an error if the viewer id is empty
	// or already attached to that session.
	Attach(ctx context.Context, instanceID string, sessionID libacp.SessionID, viewer Viewer) (controllerGranted bool, err error)

	// Detach removes viewerID from (instanceID, sessionID)'s fan-out. If it was
	// the controller and viewers remain, the earliest-attached survivor is
	// promoted. Returns ErrNotFound for an unknown instance, or an error if the
	// viewer/session is not attached.
	Detach(instanceID string, sessionID libacp.SessionID, viewerID string) error

	// List returns the config+runtime join: every declared agent, annotated with
	// its live instances (empty = not running). ctx governs the registry read (it
	// is DB-backed, unlike the purely in-memory live side), so unlike the terse
	// primitive it takes a context and can error.
	List(ctx context.Context) ([]FleetEntry, error)

	// Get returns the status of one instance, or ErrNotFound if instanceID is
	// unknown (including one already Stopped).
	Get(instanceID string) (InstanceStatus, error)

	// OpenSession drives the downstream ACP handshake on instanceID's connection: it
	// initializes the connection once (negotiating capabilities per spec — see
	// SessionSpec.Terminal for the terminal-capability advertisement rule), then drives
	// session/new with spec, capturing the response's config options / modes / models into
	// per-session kernel state. It returns the DOWNSTREAM session id — the id the instance
	// journals and fans out under, and the id a viewer Attaches to. This is the outbound
	// counterpart of Attach: viewers OBSERVE a session's stream and answer its permissions
	// via Attach; a consumer DRIVES the downstream via these methods, holding no connection.
	// Returns ErrNotFound for an unknown instance, or an error for a no-connection
	// instance or a failed handshake.
	OpenSession(ctx context.Context, instanceID string, spec SessionSpec) (libacp.SessionID, error)

	// Prompt drives one downstream session/prompt turn for sessionID and returns its stop
	// reason. Every downstream session/update during the turn is journaled and fanned out to
	// the session's viewers by the instance's harness. It is cancellation-aware: a ctx
	// cancellation (or a concurrent Cancel) resolves cleanly as StopReasonCancelled with a nil
	// error. Returns ErrNotFound for an unknown instance.
	Prompt(ctx context.Context, instanceID string, sessionID libacp.SessionID, prompt []libacp.ContentBlock) (libacp.StopReason, error)

	// DeliverToSession injects n into sessionID's fan-out — its replay journal and
	// every attached viewer — on whichever live instance currently OWNS that
	// session, exactly as a downstream session/update would arrive. It is the
	// WRITE counterpart of Attach: Attach lets a consumer OBSERVE a session's
	// stream, this lets one INJECT an out-of-band update into it. The motivating
	// consumer is the supervision edge — a sub-mission's report surfaced in the
	// transcript of the session that fired the mission — but the kernel neither
	// knows nor cares what n is: it adds nothing of its own to n and makes no
	// judgement about it. WHAT to inject and WHY is a service-layer concern, per
	// this package's policy-free invariant; this is only the mechanism.
	//
	// It resolves sessionID to its instance by scanning (session ids are
	// downstream-unique), the same shape GetByInstance uses for missions: this is
	// off any hot path (one report, not one token) and buys no index-consistency
	// problem. Returns ErrNotFound when NO live instance owns sessionID — the
	// supervisor may have ended, or the id may belong to a session this Manager
	// never hosted — which the caller treats as "deliver elsewhere" (the operator
	// inbox), never as a fault.
	DeliverToSession(ctx context.Context, sessionID libacp.SessionID, n libacp.SessionNotification) error

	// Cancel cancels sessionID's in-flight prompt turn on the downstream (session/cancel plus
	// the prompt-turn permission auto-resolve). Safe with no turn in flight. Returns
	// ErrNotFound for an unknown instance.
	Cancel(instanceID string, sessionID libacp.SessionID) error

	// CloseSession ends sessionID: it best-effort cancels any in-flight downstream turn, then
	// drops the session's kernel state (its captured surface and its journal + viewers). It
	// does NOT stop the instance — an instance multiplexes many sessions over one connection.
	// Returns ErrNotFound for an unknown instance.
	//
	// WHO CALLS IT: whoever OPENED the session, and nobody else. No transport does today, and
	// acpsvc's session/close deliberately must not (see acpsvc.Transport.CloseSession): an ACP
	// client closing a session is a VIEWER detaching, and a viewer may not end a session it did
	// not open — a supervisor closing their tab on an adopted fleet dispatch would otherwise
	// make the still-running unit's session invisible to Cancel and to adopt. The verb belongs
	// to the consumer that called OpenSession; the only one that does today (fleetservice
	// Dispatch) ends its units with Stop, which subsumes this. It stays on the interface as the
	// kernel's counterpart to OpenSession — a kernel that can open a session and not end one is
	// an incomplete kernel — and is exercised by the fleetservice cancel-contract tests.
	CloseSession(instanceID string, sessionID libacp.SessionID) error

	// SetConfigOption forwards an upstream config-option change to the downstream and adopts
	// the confirmed value into kernel state. The synthetic mode/model option ids map to
	// session/set_mode and session/set_model; every other id forwards to
	// session/set_config_option. Returns ErrNotFound for an unknown instance.
	SetConfigOption(ctx context.Context, instanceID string, sessionID libacp.SessionID, configID string, value libacp.SessionConfigOptionValue) error

	// SessionConfigOptions returns sessionID's captured config-option surface (synthetic mode
	// select + synthetic model select + the downstream agent's own options), for a transport
	// to advertise upstream. Nil for an unknown session; ErrNotFound for an unknown instance.
	SessionConfigOptions(instanceID string, sessionID libacp.SessionID) ([]libacp.SessionConfigOption, error)

	// AvailableCommands returns sessionID's captured downstream slash-command menu. Nil for an
	// unknown session; ErrNotFound for an unknown instance.
	AvailableCommands(instanceID string, sessionID libacp.SessionID) ([]libacp.AvailableCommand, error)

	// Stop tears an instance down and removes it from the registry. It sets the
	// instance's manualStop flag so the watchDog never restarts it. Idempotent:
	// stopping an unknown or already-stopped id is a no-op returning nil.
	Stop(instanceID string) error

	// Close stops every instance (tearing down all subprocesses) and cancels the
	// Manager's root context. It is the runtime-shutdown hook; after Close, Start
	// returns an error. Idempotent.
	Close() error
}

// Option configures a Manager.
type Option func(*manager)

// WithStderr forwards each spawned external instance's subprocess stderr to w.
// Defaults to io.Discard.
func WithStderr(w io.Writer) Option { return func(m *manager) { m.stderr = w } }

// WithKillGrace overrides how long an external instance's teardown waits for its
// subprocess to exit on stdin-close before killing it (see defaultKillGrace).
func WithKillGrace(d time.Duration) Option { return func(m *manager) { m.killGrace = d } }

// WithJournalSize overrides the per-session replay journal size (see
// defaultJournalSize).
func WithJournalSize(n int) Option { return func(m *manager) { m.journalSize = n } }

// WithRestart enables the watchDog restart policy: an external instance whose
// subprocess dies unexpectedly is re-spawned up to limit times before the
// instance is parked in StateWarning. Default: disabled (an unexpected death is
// terminal StateError). NOTE the ACP caveat — a restart re-spawns a fresh
// subprocess that LOSES the downstream agent's conversation context; it keeps the
// fleet alive, not the conversation.
func WithRestart(limit int) Option {
	return func(m *manager) {
		m.restartEnabled = true
		m.restartLimit = limit
	}
}

// WithEventSink installs sink as the lifecycle event sink (see EventSink).
func WithEventSink(sink EventSink) Option { return func(m *manager) { m.sink = sink } }

// WithPermissionFallback installs fn as the answerer for permission requests that
// reach a session with NO controller viewer (see PermissionFallback). Default:
// unset, which keeps the built-in headless deny — so a Manager nobody wires this
// on behaves exactly as it did before the seam existed.
//
// It is deliberately shaped like WithEventSink: one injected function, called by
// the kernel, whose behavior the kernel neither knows nor constrains. That is what
// lets `contenox serve` route an unattended ask into a mission's HITL envelope
// without teaching this package what an approval is.
func WithPermissionFallback(fn PermissionFallback) Option {
	return func(m *manager) { m.permissionFallback = fn }
}

// WithSelfExecutable overrides the program a CHAIN-kind agent is spawned from
// (see the chain branch of StartResolved). It defaults to os.Executable() —
// the running binary — which is what makes a chain unit "this runtime, bound
// to that chain" rather than a second product.
//
// It exists so a test can point the spawn at a freshly BUILT fixture binary:
// under `go test` os.Executable() is the compiled test binary, which has no
// ACP server in it, so without this seam a chain agent could only ever be
// proven to spawn — never to answer.
func WithSelfExecutable(path string) Option {
	return func(m *manager) { m.selfExecutable = path }
}

type manager struct {
	agents      agentregistryservice.Service
	stderr      io.Writer
	killGrace   time.Duration
	journalSize int

	// selfExecutable is the program a chain-kind agent re-executes. Empty means
	// "resolve os.Executable() at spawn time"; see WithSelfExecutable.
	selfExecutable string

	restartEnabled bool
	restartLimit   int
	sink           EventSink
	// permissionFallback answers unattended permission requests; nil keeps the
	// kernel's built-in headless deny. See WithPermissionFallback.
	permissionFallback PermissionFallback

	// rootCtx is the long-lived context every external instance's subprocess is
	// bound to, so instances outlive the caller ctx passed to Start. Close cancels
	// it, relocating ownership off any client connection onto the runtime.
	rootCtx    context.Context
	rootCancel context.CancelFunc

	mu        sync.Mutex
	instances map[string]*instance
	closed    bool
}

// New returns a Manager that resolves declared agents via agents. The Manager
// owns a fresh root context; call Close to tear everything down at shutdown.
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
	// Resolve the declared agent. ctx governs only this lookup — deliberately NOT
	// the instance that follows.
	agent, err := m.agents.GetByName(ctx, agentName)
	if err != nil {
		return "", fmt.Errorf("agentinstance: resolve agent %q: %w", agentName, err)
	}
	return m.StartResolved(ctx, agent, cwd)
}

func (m *manager) StartResolved(ctx context.Context, agent *runtimetypes.Agent, cwd string) (string, error) {
	_ = ctx // no registry read happens here; ctx is kept for interface uniformity.
	if agent == nil {
		return "", fmt.Errorf("agentinstance: agent is required")
	}
	switch agent.Kind {
	case runtimetypes.AgentKindExternalACP:
		cfg, err := agent.ExternalACPConfig()
		if err != nil {
			return "", fmt.Errorf("agentinstance: agent %q: %w", agent.Name, err)
		}
		// Seed the sandbox workspace from the caller's resolved cwd when the agent
		// declares none of its own — the same defaulting agenthost/drive.go does for
		// the one-shot path, so a mission/session-launched agent is confined to the
		// workspace the caller resolved instead of hitting the wall's fail-closed
		// empty-cwd refusal. A declared cwd wins.
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
		spawner, err := m.chainSpawner(agent, cwd)
		if err != nil {
			return "", err
		}
		return m.bringUp(agent, spawner)
	default:
		return "", fmt.Errorf("agentinstance: agent %q has unsupported kind %q", agent.Name, agent.Kind)
	}
}

// chainSpawner builds the agenthost primitive for a CHAIN-kind agent: this
// binary's own ACP server, bound to the declared chain file.
//
// The kernel does not gain a second connection mechanism for this, and it does
// not learn what a task chain is. It builds an ordinary ExternalACPAgent whose
// command happens to be US — the runtime's `acp` subcommand is a conforming ACP
// agent over stdio, so "contenox driving contenox" is just the external path
// with the loopback wired in. Everything downstream of bringUp (journal,
// viewers, controller promotion, permission routing, adopt, cancel, stop,
// restart policy) is therefore identical for both kinds by construction rather
// than by parallel implementation.
//
// The child INHERITS this process's environment and only ChainPathEnvVar is
// added on top. That is deliberate and load-bearing: a chain unit is meant to
// share the one global runtime state (HOME resolves the database, the seeded
// presets, the workspace id), which is the entire difference between a chain
// unit and an external agent that brings its own everything.
//
// HITL is NOT disabled: the child runs its gated tool calls through ACP
// session/request_permission, which arrives back here as the instance harness's
// RequestPermission and routes to the session's controller viewer — the same
// human-in-the-loop path an external agent's requests take, including the
// unsupervised auto-deny when nobody is attached.
func (m *manager) chainSpawner(agent *runtimetypes.Agent, cwd string) (agenthost.Agent, error) {
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
	return &agenthost.ExternalACPAgent{
		Config: runtimetypes.ExternalACPConfig{
			Transport: runtimetypes.ExternalACPTransportStdio,
			Command:   self,
			Args:      []string{ChainACPSubcommand},
			// A chain unit declares no cwd of its own, so it runs in the caller's
			// resolved cwd (the mission/session root).
			Cwd: cwd,
			Env: map[string]string{ChainPathEnvVar: cfg.Path},
		},
		// This is contenox spawning contenox, not a foreign agent, so it runs
		// OUTSIDE the sandbox — see agenthost.ExternalACPAgent.SelfSpawn. The wall
		// would deny the unit the very state it is defined by sharing (~/.contenox
		// is not carved, the env is scrubbed); what governs it is the in-process
		// capability grants and the HITL gate its tool calls already run through.
		SelfSpawn: true,
		Stderr:    m.stderr,
		KillGrace: m.killGrace,
	}, nil
}

// bringUp builds and starts an instance for agent, then registers it. It is the
// ONE path both kinds take — external and chain differ only in which
// agenthost.Agent they hand it — so journal, viewers, controller promotion,
// adopt, cancel, stop and restart semantics are identical for both by
// construction. start() spawns OUTSIDE the registry lock, so a slow subprocess
// startup never blocks List/Get/Stop of other instances; only on success is the
// instance registered. A spawn failure registers nothing and leaks no process.
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
		// The injected answerer for an unattended permission request, with the SAME
		// identity the hooks above close over. Nil when no fallback is wired, which
		// is what keeps the hub on its built-in deny.
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

// unattendedPermissionAnswerer adapts the Manager's configured PermissionFallback
// to the hub's request-only hook by closing this instance's identity over it —
// exactly how the onState/onAttach/onDetach hooks above are built, and for the same
// reason: the hub is per-instance state that must not know the Manager, and the
// answerer must not have to ask who is calling it.
//
// Returns nil when no fallback is configured, so the hub keeps its built-in deny
// rather than routing through a wrapper that would only re-implement it.
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

// instance resolves instanceID to its live instance, or ErrNotFound. It is the shared
// lookup the session-driving methods use before delegating to the instance primitive.
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

// DeliverToSession implements Manager.DeliverToSession by scanning for the
// instance that owns sessionID and injecting n into its fan-out. It snapshots
// the instance set under mu (never holding mu across the per-instance delivery,
// which takes the hub lock), then asks each whether it owns the session; the
// first owner delivers and the scan stops. No owner is ErrNotFound.
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

// SessionAgentText returns the concatenated text of the agent-message chunks
// retained in sessionID's replay journal on instanceID — the unit's own words,
// recoverable WITHOUT attaching a viewer, from the journal the kernel already
// keeps for replay. ok is false for an unknown instance or a session this
// instance does not own; text is "" when the session is owned but its journal
// holds no agent text yet.
//
// It is deliberately NOT part of the Manager interface. It is an OPTIONAL,
// policy-free READ a consumer reaches by type assertion (fleetservice quotes a
// silent unit's last words when it files a blocker on the unit's behalf), so a
// Manager double need not grow a method just to satisfy the interface — the same
// reason DeliverToSession's scan lives here rather than being pushed onto every
// caller. The kernel can answer it because it already holds the bytes; nothing
// about it is a lifecycle verb, so it stays off the interface that IS the
// lifecycle contract.
func (m *manager) SessionAgentText(instanceID string, sessionID libacp.SessionID) (string, bool) {
	inst, err := m.instance(instanceID)
	if err != nil {
		return "", false
	}
	return inst.agentText(sessionID)
}

// SessionJournal returns a RAW snapshot of sessionID's replay journal on
// instanceID — every downstream session/update the kernel captured, whether or
// not a viewer was attached — together with the session's working directory
// (its SessionSpec.Cwd). ok is false for an unknown instance or a session this
// instance does not own; the journal is nil for an owned-but-silent session and
// cwd is "" when none was set.
//
// It is the SessionAgentText precedent applied to a richer read: an OPTIONAL,
// policy-free accessor a consumer reaches by type assertion, deliberately NOT on
// the Manager interface, so a Manager double need not grow a method and sibling
// mocks stay untouched. Where SessionAgentText interprets the journal down to the
// unit's words for fleetservice's silent-turn blocker, this returns it
// uninterpreted for the attention layer (runtime/missionchanges), which folds the
// tool-call updates into a changed-files list and a scope summary. The kernel
// still interprets nothing: it already holds these bytes for replay, and nothing
// here is a lifecycle verb, so it stays off the interface that IS the lifecycle
// contract.
func (m *manager) SessionJournal(instanceID string, sessionID libacp.SessionID) ([]libacp.SessionNotification, string, bool) {
	inst, err := m.instance(instanceID)
	if err != nil {
		return nil, "", false
	}
	return inst.sessionJournal(sessionID)
}

func (m *manager) List(ctx context.Context) ([]FleetEntry, error) {
	// Snapshot the live side, grouped by declared-agent id.
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

	// Join with the declared (config) side.
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
			Instances: byAgent[a.ID], // nil (not running) or the live set
		})
	}
	// Orphan instances (agent deleted after start) still surface, so the fleet
	// view never silently hides a running subprocess.
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

// listDeclared pages through every declared agent via the registry service. The
// store filters created_at < cursor (DESC), so each page's oldest row seeds the
// next cursor. The strictly-decreasing-cursor guard defends against an
// identical-timestamp storm looping forever (at the cost of truncating such a
// tie), a pre-existing store pagination limit, not one this join introduces.
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
		return nil // idempotent
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

	// Stop every instance, then cancel the root context as a backstop. Aggregate
	// teardown errors rather than stopping at the first, so one wedged subprocess
	// cannot hide the rest.
	var errs []error
	for _, inst := range insts {
		if err := inst.stop(); err != nil {
			errs = append(errs, err)
		}
	}
	m.rootCancel()
	return errors.Join(errs...)
}
