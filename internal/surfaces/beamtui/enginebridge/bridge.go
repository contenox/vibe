// Package enginebridge is the seam between a beam surface and the contenox
// runtime: it owns one in-process ACP loopback — a real acpsvc.Transport on one
// end, a real libacp.ClientSideConnection on the other, wired by two crossed
// io.Pipes — and translates the resulting notification stream into a typed
// event vocabulary a renderer can consume without knowing ACP exists.
//
// # Ownership
//
// The composition root is NOT here. The caller builds the engine, database,
// bus and (optionally) the mission fleet, hands them in via Deps, and owns
// their shutdown: Close tears down the loopback and the Transport ONLY, and the
// caller then closes the bus and, after it, the database — the bus keeps a
// cleanup goroutine that queries the DB, so that order is not negotiable.
// UNLESS Close returned an error, which means it abandoned goroutines and the
// caller must close NEITHER (see Close and ErrUncleanShutdown).
// Nothing here constructs an engine, reads a config file, or touches $HOME.
//
// # Ordering
//
// Every inbound session/update THE ACTIVE-SESSION FILTER ADMITS becomes exactly
// ONE typed Event, delivered on the single channel Events() returns, in the
// order libacp read it off the wire — no coalescing across kinds, no
// reordering, no silent drops within that set (a kind this build does not model
// becomes UnknownUpdate). Updates for any other session are discarded before
// translation, which is what SetActiveSession is for. The queue behind that
// channel is unbounded on purpose: libacp dispatches notifications inline on
// its read loop, so a Bridge that blocked its producer would stall the whole
// connection, response deliveries included. A slow consumer therefore costs
// memory, never order and never facts. Events still queued when Close runs are
// dropped, and the channel is closed once the pump stops.
//
// Out-of-band facts — PermissionRequested (an inbound request, dispatched on
// its own goroutine by libacp), the Shell*/Turn* results of the Bridge's own
// calls, and InboxItemAdded (which arrives on the process bus, not the wire) —
// share that channel but carry no ordering guarantee relative to the
// notification stream; neither the protocol nor the bus gives one. Among
// themselves the inbox items DO keep publish order, which is all the operator
// inbox has ever promised.
//
// # The second source
//
// One fact does not come off the loopback at all. A mission report that reached
// no live supervising session is written to the durable operator inbox and
// announced on the process bus, because there was no session to deliver it into;
// the Bridge subscribes to that subject and relays it as InboxItemAdded so a
// surface still consumes exactly one event stream (see inbox.go, and Deps.Bus).
// A Bridge built without a bus simply never emits one.
//
// # No UI
//
// This package imports nothing from internal/surfaces/beamtui/{frame,term,
// input,style} and nothing from internal/surfaces/contenoxcli. It is a runtime
// client, not a renderer: a CLI verb could depend on it unchanged, and the beam
// command (which lives in contenoxcli) imports THIS — importing back would be a
// cycle.
package enginebridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/contenox/beam/internal/kernel/enginesvc"
	"github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/approvalflow"
	"github.com/contenox/beam/internal/services/shellsession"
	"github.com/contenox/beam/internal/services/vfs"
	"github.com/contenox/beam/internal/surfaces/acpsvc"
	libacp "github.com/contenox/beam/libacp"
)

// extMethodTerminalRun mirrors the unexported acpsvc constant of the same name
// (internal/surfaces/acpsvc/terminal.go): the `$`/`!` passthrough entrypoint
// that runs one operator line against the session's warm shell WITHOUT an LLM
// turn. Operator lines are not HITL-gated; output streams back as
// TerminalChunk events.
const extMethodTerminalRun = "_contenox/terminal/run"

// shutdownJoinTimeout bounds the loopback teardown as a whole. A Run loop that
// has not returned by then is reported rather than waited on forever: a TUI
// exiting must not hang on a wedged connection.
//
// The bound MUST DOMINATE libacp's own drain: AgentSideConnection.Run and
// ClientSideConnection.Run only return after waitHandlers, which is itself
// bounded by libacp.HandlerDrainTimeout (libacp/errors.go). A join timeout
// shorter than that would report "did not shut down" for every connection whose
// handlers merely took their full, legitimate drain budget — turning the normal
// slow path into a false unclean-shutdown verdict. The margin is for the
// bookkeeping either side does around the drain, not for handler work.
const shutdownJoinTimeout = libacp.HandlerDrainTimeout + 2*time.Second

// transportCloseTimeout bounds Transport.Close, which does real work (MCP
// deregistration, driver teardown) against the database.
const transportCloseTimeout = 10 * time.Second

var (
	// ErrClosed reports a call on a Bridge whose Close has already run.
	ErrClosed = errors.New("enginebridge: bridge is closed")

	// ErrPromptInFlight reports a second SubmitPrompt for a session whose
	// previous turn has not ended. One turn per session is the ACP contract;
	// the caller must wait for TurnEnded/TurnFailed or Cancel first.
	ErrPromptInFlight = errors.New("enginebridge: a prompt is already in flight for this session")

	// ErrEmptyPrompt reports a SubmitPrompt with nothing in it. acpsvc rejects
	// it as invalid params; failing locally keeps a stray keystroke off the
	// wire.
	ErrEmptyPrompt = errors.New("enginebridge: empty prompt")

	// ErrShellDisabled reports that the runtime was built without shell
	// sessions (Deps.ShellSessions nil), so the terminal passthrough answers
	// method-not-found. The feature is ABSENT, not broken — surfaces should say
	// so rather than render a failure.
	ErrShellDisabled = errors.New("enginebridge: shell sessions are not enabled")

	// ErrUncleanShutdown reports that Close gave up joining something: a Run
	// loop, or this package's own goroutines, did not finish inside
	// shutdownJoinTimeout. Every error Close returns wraps it.
	//
	// It is a RESOURCE VERDICT, not a diagnostic: goroutines that may still be
	// touching sessions, drivers and database handles were abandoned, so the
	// caller must NOT go on to close the bus or the database. See Close.
	ErrUncleanShutdown = errors.New("enginebridge: unclean shutdown, resources abandoned")
)

// Deps is everything the Bridge needs from the composition root. It mirrors the
// subset of acpsvc.Deps a beam process wires, plus the pieces the Bridge itself
// holds. Nothing here is constructed by this package.
type Deps struct {
	// Engine is the ONE warm task-chain engine for the process. Required.
	Engine *enginesvc.Engine
	// DB is the runtime database. Required. The Bridge does not close it; the
	// caller closes it AFTER Bus.
	DB libdb.DBManager
	// Bus is the process event bus. The Bridge SUBSCRIBES to it — one
	// subscription, on the operator inbox's added-item subject, relayed onto
	// the event stream as InboxItemAdded (see inbox.go). That is the whole of
	// its use: the Bridge does not publish, does not close the bus, and does
	// not pass it to acpsvc (Engine.Bus already carries the same messenger, and
	// that is the one the runtime actually uses).
	//
	// The subscription is why the inbox works at all. An inbox item is by
	// definition a mission report that reached NO live session, so there is no
	// ACP notification for it and nothing on the loopback will ever mention it;
	// the bus is the only carrier it has.
	//
	// Nil is legal and means exactly one thing: no inbox events. Every other
	// Bridge behaviour is unchanged, and nothing here nil-checks the bus twice —
	// startInboxWatch returns immediately and stopInboxWatch is a no-op.
	//
	// Accepting the field also PINS THE SHUTDOWN ORDER, which was its original
	// and still-standing second purpose: whoever hands the Bridge a bus has, by
	// that act, declared the bus outlives the Bridge and gets closed after it
	// (and before the DB, whose cleanup goroutine the bus queries).
	//
	// It MUST be the SQLite backend (libbus.NewSQLiteWithOptions): a turn's
	// trailing events — and the inbox subscription's final drain — are delivered
	// only by a backend that drains on Unsubscribe, which the in-memory backend
	// does not promise.
	Bus libbus.Messenger
	// ChainRegistry supplies the default ACP chain. Required: a native turn
	// without one fails with "no chain configured".
	ChainRegistry *acpsvc.ChainRegistry
	// The session defaults acpsvc seeds every new session's config options
	// from, and that the chain's `{{var:model}}`-family macros resolve
	// against. DefaultModel is effectively REQUIRED for a working turn: a
	// chain whose execute_config names the model macro fails with "template
	// fallback var \"model\" is not set" when it is empty, exactly as it would
	// for `contenox acp` launched with no configured model. `/model` and its
	// siblings mutate these per session afterwards; the Bridge itself never
	// reads them again.
	DefaultModel       string
	DefaultProvider    string
	DefaultAltModel    string
	DefaultAltProvider string
	DefaultMaxTokens   string
	DefaultThink       string
	// WorkspaceID scopes sessions and stored state. Required.
	WorkspaceID string
	// ContenoxDir is the active .contenox directory, used to locate auxiliary
	// chains (/compact). Optional.
	ContenoxDir string
	// WorkspaceRoots is the allowlist of directories a session may root itself
	// in. Nil accepts any absolute cwd — the right default for a local TUI,
	// which owns its own filesystem.
	WorkspaceRoots *vfs.Factory
	// ShellSessions manages the per-session persistent PTY. Nil disables the
	// terminal passthrough; RunShellLine then reports ErrShellDisabled.
	ShellSessions shellsession.Manager
	// KnownPolicies and HITLDefaultPolicyName are display-only strings /policy
	// reports. HITL policy is a startup default, never re-asked per call.
	KnownPolicies         []string
	HITLDefaultPolicyName string
	// Fleet and Agents together are the mission capability: BOTH non-nil is
	// what makes acpsvc advertise and accept /mission. Both nil in a process
	// that is itself a dispatched unit.
	Fleet  acpsvc.MissionDispatcher
	Agents acpsvc.MissionAgentResolver
	// SessionRouter is the process-shared (contenox session -> transport)
	// registry a shared engine routes HITL approvals through. A single-transport
	// process may leave it nil.
	SessionRouter *acpsvc.SessionRouter
	// ClientInfo identifies this client in the ACP handshake. Defaults to
	// {Name: "beam"} when nil.
	ClientInfo *libacp.Implementation
}

func (d Deps) validate() error {
	switch {
	case d.Engine == nil:
		return errors.New("enginebridge: Deps.Engine is required")
	case d.DB == nil:
		return errors.New("enginebridge: Deps.DB is required")
	case d.ChainRegistry == nil:
		return errors.New("enginebridge: Deps.ChainRegistry is required")
	case d.WorkspaceID == "":
		return errors.New("enginebridge: Deps.WorkspaceID is required")
	}
	return nil
}

// Bridge is the live loopback. It is safe for concurrent use; every method may
// be called from any goroutine, including the one draining Events.
type Bridge struct {
	deps Deps

	conn      *libacp.ClientSideConnection
	transport *acpsvc.Transport
	client    *routingClient

	// inboxSub is the operator-inbox bus subscription, nil when Deps.Bus is.
	// It is written once during New, before any other goroutine can observe the
	// Bridge, and read only by Close — so it needs no lock.
	inboxSub libbus.Subscription

	runCtx    context.Context
	runCancel context.CancelFunc

	agentDone  chan error
	clientDone chan error

	// events is the single ordered outlet; queue/notify are the unbounded FIFO
	// behind it (see the package doc on why the producer must never block).
	events chan Event
	qmu    sync.Mutex
	queue  []Event
	qdone  bool
	notify chan struct{}

	// done is closed first in teardown: it releases pending permission waiters
	// and the pump before anything else is torn down. stopOnce guards that half
	// alone, because it has TWO triggers — Close, and the run context being
	// cancelled out from under the bridge — while closeOnce guards the full
	// Close sequence (join + Transport.Close), which only Close performs.
	done      chan struct{}
	stopOnce  sync.Once
	closeOnce sync.Once
	closeErr  error

	// wg tracks goroutines this package starts (pump, ctx watcher, prompts,
	// shell runs) so Close joins them instead of hoping.
	//
	// admitMu closes the window between "not closed" and wg.Add: a submitter
	// that read isClosed()==false and had not yet incremented the counter could
	// otherwise start a goroutine Close will never join — and, worse, call
	// wg.Add concurrently with wg.Wait, which panics. Submit paths hold RLock
	// across the re-check and the Add; teardown takes the write lock AFTER
	// marking the bridge closed, which makes "closed" a barrier every admitted
	// goroutine is already counted behind.
	admitMu sync.RWMutex
	wg      sync.WaitGroup

	promptMu sync.Mutex
	inflight map[libacp.SessionID]struct{}

	// pendingPerms counts permission requests currently waiting on an operator.
	// It is an observability hook (tests assert it returns to zero after
	// teardown), never a gate: Close cannot assert on it, because a request
	// raised in-process rather than over the wire is not covered by libacp's
	// handler drain.
	pendingPerms atomic.Int64
}

// New builds the loopback and starts both Run loops. ctx bounds the whole
// bridge: cancelling it tears the connection down exactly as Close does, minus
// the Transport.Close call — both Run loops end, in-flight prompts and shell
// calls fail, pending permission requests resolve cancelled, the event pump
// stops and Events() CLOSES, and isClosed reports true so further calls answer
// ErrClosed. What cancellation does NOT do is close the Transport or join
// anything, so callers that already have a process context must still call
// Close on exit; doing so after cancellation is well-defined and performs
// exactly the remaining half.
func New(ctx context.Context, deps Deps) (*Bridge, error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}

	runCtx, runCancel := context.WithCancel(ctx)
	b := &Bridge{
		deps:       deps,
		runCtx:     runCtx,
		runCancel:  runCancel,
		agentDone:  make(chan error, 1),
		clientDone: make(chan error, 1),
		events:     make(chan Event),
		notify:     make(chan struct{}, 1),
		done:       make(chan struct{}),
		inflight:   make(map[libacp.SessionID]struct{}),
	}

	// The inbox subscription is taken BEFORE the loopback is built, so a bus
	// that cannot be subscribed to fails New while there is still nothing to
	// tear down but a context.
	if err := b.startInboxWatch(runCtx); err != nil {
		runCancel()
		return nil, err
	}

	// Two io.Pipe pairs crossed into duplex ReadWriteClosers: the agent's reads
	// are the client's writes and vice versa. This is the in-memory stand-in for
	// the stdio an editor would use — real libacp wire types either way.
	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentSide := &duplexPipe{r: agentR, w: agentW}
	clientSide := &duplexPipe{r: clientR, w: clientW}

	factory := acpsvc.New(acpsvc.Deps{
		Engine:                deps.Engine,
		DB:                    deps.DB,
		ChainRegistry:         deps.ChainRegistry,
		DefaultModel:          deps.DefaultModel,
		DefaultProvider:       deps.DefaultProvider,
		DefaultAltModel:       deps.DefaultAltModel,
		DefaultAltProvider:    deps.DefaultAltProvider,
		DefaultMaxTokens:      deps.DefaultMaxTokens,
		DefaultThink:          deps.DefaultThink,
		WorkspaceID:           deps.WorkspaceID,
		ContenoxDir:           deps.ContenoxDir,
		WorkspaceRoots:        deps.WorkspaceRoots,
		ShellSessions:         deps.ShellSessions,
		KnownPolicies:         deps.KnownPolicies,
		HITLDefaultPolicyName: deps.HITLDefaultPolicyName,
		Fleet:                 deps.Fleet,
		Agents:                deps.Agents,
		SessionRouter:         deps.SessionRouter,
	})

	// Type-asserting inside the factory closure is the only way to reach the
	// concrete Transport: acpsvc.New hands back a libacp.AgentFactory, and
	// NewAgentSideConnection invokes it eagerly during construction, so tr is
	// populated by the time this returns.
	var transport *acpsvc.Transport
	agentConn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		agent := factory(c)
		transport, _ = agent.(*acpsvc.Transport)
		return agent
	})
	if transport == nil {
		runCancel()
		b.stopInboxWatch()
		_ = agentSide.Close()
		_ = clientSide.Close()
		return nil, errors.New("enginebridge: acpsvc factory did not return an *acpsvc.Transport")
	}
	b.transport = transport

	b.client = &routingClient{bridgeClient: &bridgeClient{b: b}}
	// No active session yet: until SetActiveSession names one, updates pass
	// through unfiltered so a first session's deferred available_commands_update
	// cannot be lost between session/new returning and the caller selecting it.
	b.client.setActive("")

	b.conn = libacp.NewClientSideConnection(clientSide, func(*libacp.ClientSideConnection) libacp.Client {
		return b.client
	})

	go func() { b.agentDone <- agentConn.Run(runCtx) }()
	go func() { b.clientDone <- b.conn.Run(runCtx) }()

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.pump()
	}()

	// The context watcher makes New's promise real: nothing else observes
	// runCtx on the bridge's own side, so without it a cancelled context would
	// end the two Run loops while the event surface stayed open forever and
	// isClosed kept answering false. Both arms are terminal — Close reaches the
	// same stopQueue through the same sync.Once — so this goroutine always
	// returns and joinWait can count it.
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		select {
		case <-runCtx.Done():
			b.stopQueue()
		case <-b.done:
		}
	}()

	return b, nil
}

// duplexPipe adapts one end of a crossed io.Pipe pair to io.ReadWriteCloser.
type duplexPipe struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (p *duplexPipe) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *duplexPipe) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *duplexPipe) Close() error {
	_ = p.r.Close()
	return p.w.Close()
}

// Transport returns the acpsvc.Transport this Bridge drives. It exists for one
// caller: the in-process mission fleet, whose report deliverer must reach the
// live session a mission was fired from — fleetboot.Deps.Transport late-binds
// exactly this, so a report lands in the firing transcript instead of the
// operator inbox.
//
// The fleet must be built BEFORE the Bridge (acpsvc reads Deps.Fleet at
// construction), so this is nil-safe on a nil receiver: the composition root
// can hand the fleet a closure over a Bridge it has not published yet.
//
// Publish it through an atomic.Pointer, NOT a naked closed-over variable. The
// deliverer runs on the fleet's goroutines, and a mission that reports during
// startup calls the closure while New is still assigning — a plain variable
// read there is a data race, and one that -race will only catch on the run
// where the timing lands:
//
//	var bridgeRef atomic.Pointer[enginebridge.Bridge]
//	fleet, agents, stop, err := fleetboot.BuildInProcessFleet(ctx, fleetboot.Deps{
//		// Load() is nil until Store runs, and Transport() is nil-safe on a nil
//		// receiver, so an early delivery degrades to "no transport yet".
//		Transport: func() *acpsvc.Transport { return bridgeRef.Load().Transport() },
//		// …
//	})
//	b, err := enginebridge.New(ctx, enginebridge.Deps{Fleet: fleet, Agents: agents /* … */})
//	bridgeRef.Store(b)
//
// It is NOT a general escape hatch: every session and turn operation belongs on
// the Bridge, which owns the connection's in-flight state.
func (b *Bridge) Transport() *acpsvc.Transport {
	if b == nil {
		return nil
	}
	return b.transport
}

// Events returns the single ordered outlet. It is closed once the pump has
// stopped — by Close, or by the context New was given being cancelled — and a
// consumer should range over it and treat the close as "the bridge is gone",
// without waiting for a Close it may not be the one to call.
func (b *Bridge) Events() <-chan Event { return b.events }

// admit reserves a slot in the WaitGroup for a goroutine this package is about
// to start, and reports whether the bridge is still open. It is the ONLY place
// wg.Add may be called after New: the read lock is what makes "isClosed() is
// false" and "the counter is incremented" one atomic step with respect to
// teardown, which takes the write lock after flipping closed. A false return
// means the caller must NOT start its goroutine.
func (b *Bridge) admit() bool {
	b.admitMu.RLock()
	defer b.admitMu.RUnlock()
	if b.isClosed() {
		return false
	}
	b.wg.Add(1)
	return true
}

// emit appends to the unbounded FIFO. It never blocks and never fails, so no
// caller — least of all libacp's read loop — can be stalled by a slow renderer.
// After Close it is a no-op.
func (b *Bridge) emit(e Event) {
	b.qmu.Lock()
	if b.qdone {
		b.qmu.Unlock()
		return
	}
	b.queue = append(b.queue, e)
	b.qmu.Unlock()
	select {
	case b.notify <- struct{}{}:
	default:
	}
}

// pump drains the FIFO onto the events channel in order. It exits on Close,
// discarding anything still queued — teardown is not a delivery guarantee.
func (b *Bridge) pump() {
	defer close(b.events)
	for {
		b.qmu.Lock()
		if len(b.queue) == 0 {
			b.qmu.Unlock()
			select {
			case <-b.notify:
				continue
			case <-b.done:
				return
			}
		}
		e := b.queue[0]
		b.queue[0] = nil
		b.queue = b.queue[1:]
		b.qmu.Unlock()

		select {
		case b.events <- e:
		case <-b.done:
			return
		}
	}
}

// Initialize performs the ACP handshake. It declares NO filesystem and NO
// terminal client capabilities on purpose: acpsvc's ACPFileIO then falls back
// to direct OS file IO (fileio.go), which is correct for an in-process client
// living on the very machine the tools run on, and means beam implements none
// of those reverse callbacks.
func (b *Bridge) Initialize(ctx context.Context) (libacp.InitializeResponse, error) {
	if b.isClosed() {
		return libacp.InitializeResponse{}, ErrClosed
	}
	info := b.deps.ClientInfo
	if info == nil {
		info = &libacp.Implementation{Name: "beam"}
	}
	return b.conn.Initialize(ctx, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientCapabilities: libacp.ClientCapabilities{
			FS:       libacp.FileSystemCapabilities{ReadTextFile: false, WriteTextFile: false},
			Terminal: false,
		},
		ClientInfo: info,
	})
}

// NewSession creates a session. It is a thin 1:1 pass-through: cwd resolution,
// MCP registration and session bookkeeping all live in acpsvc/session.go and
// are not re-implemented here.
//
// It does NOT change the active session — see SetActiveSession for the call
// order that keeps a new session's deferred available_commands_update.
func (b *Bridge) NewSession(ctx context.Context, req libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
	if b.isClosed() {
		return libacp.NewSessionResponse{}, ErrClosed
	}
	resp, err := b.conn.NewSession(ctx, req)
	if err != nil {
		return resp, err
	}
	b.emitInitialConfigOptions(resp.SessionID, resp.ConfigOptions)
	return resp, nil
}

// emitInitialConfigOptions replays a session response's OPENING config options
// onto the event stream as an ordinary ConfigOptionUpdated.
//
// The options a session starts with — the model select and its provider groups,
// the think levels, the HITL presets — ride the session/new, session/load and
// session/resume RESPONSES. Only later changes arrive as config_option_update
// notifications. A consumer that folds the runtime in through Events() (which is
// every consumer: that is the seam) therefore saw nothing at all until the first
// /model or set_config_option, which is precisely the state where a surface most
// needs them — a fresh session, operator about to ask what models exist.
//
// Replaying them here rather than making every caller destructure the response
// keeps ONE shape for "these are the session's config options", so the code that
// applies them never has to be written twice. It is emitted after the response
// is in hand, so it lands behind the notifications acpsvc deferred until after
// session/new — the same ordering an editor sees.
func (b *Bridge) emitInitialConfigOptions(sid libacp.SessionID, options []libacp.SessionConfigOption) {
	if len(options) == 0 {
		return
	}
	b.emit(ConfigOptionUpdated{SessionID: sid, Options: options})
}

// LoadSession reopens a persisted session and replays its transcript as
// user/agent chunk events.
func (b *Bridge) LoadSession(ctx context.Context, req libacp.LoadSessionRequest) (libacp.LoadSessionResponse, error) {
	if b.isClosed() {
		return libacp.LoadSessionResponse{}, ErrClosed
	}
	resp, err := b.conn.LoadSession(ctx, req)
	if err != nil {
		return resp, err
	}
	b.emitInitialConfigOptions(req.SessionID, resp.ConfigOptions)
	// The replay marker rides the same FIFO as the replayed notifications,
	// which libacp dispatched inline on the read loop BEFORE this RPC's
	// response was delivered — so by queue order, every replayed event
	// precedes it. Consumers use it to settle the trailing replayed message
	// (nothing on the wire ends a replay otherwise).
	b.emit(ReplayEnded{SessionID: req.SessionID})
	return resp, nil
}

// ResumeSession re-attaches to a session without replaying its transcript.
func (b *Bridge) ResumeSession(ctx context.Context, req libacp.ResumeSessionRequest) (libacp.ResumeSessionResponse, error) {
	if b.isClosed() {
		return libacp.ResumeSessionResponse{}, ErrClosed
	}
	resp, err := b.conn.ResumeSession(ctx, req)
	if err != nil {
		return resp, err
	}
	b.emitInitialConfigOptions(req.SessionID, resp.ConfigOptions)
	return resp, nil
}

// ListSessions returns the session roster the picker renders.
func (b *Bridge) ListSessions(ctx context.Context, req libacp.ListSessionsRequest) (libacp.ListSessionsResponse, error) {
	if b.isClosed() {
		return libacp.ListSessionsResponse{}, ErrClosed
	}
	return b.conn.ListSessions(ctx, req)
}

// CloseSession drops a session from this connection: it first cancels any
// in-flight turn (leaving a half-running prompt behind would leak a goroutine
// waiting on a response that is never coming), then hands off to acpsvc, which
// owns shell kill, MCP cleanup and driver teardown.
func (b *Bridge) CloseSession(ctx context.Context, req libacp.CloseSessionRequest) (libacp.CloseSessionResponse, error) {
	if b.isClosed() {
		return libacp.CloseSessionResponse{}, ErrClosed
	}
	if b.hasInflight(req.SessionID) {
		_ = b.Cancel(req.SessionID)
	}
	return b.conn.CloseSession(ctx, req)
}

// DeleteSession removes a session permanently. Like CloseSession it cancels an
// in-flight turn first.
func (b *Bridge) DeleteSession(ctx context.Context, req libacp.DeleteSessionRequest) (libacp.DeleteSessionResponse, error) {
	if b.isClosed() {
		return libacp.DeleteSessionResponse{}, ErrClosed
	}
	if b.hasInflight(req.SessionID) {
		_ = b.Cancel(req.SessionID)
	}
	return b.conn.DeleteSession(ctx, req)
}

// SetActiveSession points the session/update filter at id, by building a NEW
// libacp.FilterSessionUpdates wrapper: the connection forwards every update
// regardless of session, so without this a just-abandoned session's chunks leak
// into the current transcript.
//
// The empty id means UNFILTERED — every session's updates are delivered, each
// tagged with its SessionID. That is the state a fresh Bridge starts in, and it
// is the only way to keep the available_commands_update acpsvc defers until
// AFTER the session/new response: that notification is written on the wire
// before the caller can possibly learn the new id. The call order that never
// loses it is therefore:
//
//	b.SetActiveSession("")           // unfiltered window
//	resp, _ := b.NewSession(ctx, …)  // menu/usage arrive during it
//	b.SetActiveSession(resp.SessionID)
//
// Switching between two already-open sessions needs no such window.
func (b *Bridge) SetActiveSession(id libacp.SessionID) {
	b.client.setActive(id)
}

// ActiveSession reports the session the update filter is pointed at, or "" when
// updates are unfiltered.
func (b *Bridge) ActiveSession() libacp.SessionID { return b.client.active() }

// SubmitPrompt sends text as a turn and RETURNS IMMEDIATELY. The turn's result
// arrives on Events as TurnEnded (including a genuine cancel, which is
// StopReasonCancelled and not an error) or TurnFailed; everything it streams
// arrives as ordinary events before that.
//
// Slash commands are ordinary text here: acpsvc's parseCommand intercepts them
// server-side, so /help and /mission take exactly the path an editor's do. The
// error returned is only about ADMISSION — one turn per session, non-empty
// text, bridge alive.
//
// A turn still in flight when the bridge is torn down NEVER yields a terminal
// event: teardown stops the pump before the prompt's goroutine can report, so
// TurnEnded/TurnFailed are dropped along with everything else still queued. A
// caller that tracks pending turns must treat the close of Events() — not a
// terminal event per turn — as the resolution for all of them.
func (b *Bridge) SubmitPrompt(sessionID libacp.SessionID, text string) error {
	if b.isClosed() {
		return ErrClosed
	}
	if text == "" {
		return ErrEmptyPrompt
	}
	if sessionID == "" {
		return fmt.Errorf("enginebridge: session id is required")
	}

	b.promptMu.Lock()
	if _, busy := b.inflight[sessionID]; busy {
		b.promptMu.Unlock()
		return ErrPromptInFlight
	}
	b.inflight[sessionID] = struct{}{}
	b.promptMu.Unlock()

	if !b.admit() {
		b.promptMu.Lock()
		delete(b.inflight, sessionID)
		b.promptMu.Unlock()
		return ErrClosed
	}
	go func() {
		defer b.wg.Done()
		resp, err := b.conn.Prompt(b.runCtx, libacp.PromptRequest{
			SessionID: sessionID,
			Prompt:    []libacp.ContentBlock{libacp.NewTextContent(text)},
		})
		// The terminal event is emitted BEFORE the in-flight mark is released,
		// both under promptMu, because releasing first opens a window where a
		// racing SubmitPrompt for the same session is admitted and starts
		// streaming chunks that reach the queue ahead of this turn's
		// TurnEnded/TurnFailed. A consumer would then see turn 2's output
		// attributed to a turn it still believes is running. Holding the lock
		// across the emit makes "this turn ended" strictly precede "the next
		// turn may begin" in the one order that matters, the queue's.
		b.promptMu.Lock()
		if err != nil {
			b.emit(TurnFailed{SessionID: sessionID, Err: err})
		} else {
			b.emit(TurnEnded{SessionID: sessionID, StopReason: resp.StopReason})
		}
		delete(b.inflight, sessionID)
		b.promptMu.Unlock()
	}()
	return nil
}

// Cancel interrupts sessionID's turn. It uses CancelPrompt, not CancelSession:
// besides sending session/cancel, that force-resolves every in-flight
// permission request for the session as cancelled and auto-cancels new ones
// until the Prompt call returns — the client half of the spec's cancellation
// contract. Without it a turn cancelled while an approval card was open would
// leave the card's goroutine waiting on a keystroke forever.
//
// Cancelling a session with no in-flight prompt is not an error: it degrades to
// a plain session/cancel notification, which the agent ignores.
func (b *Bridge) Cancel(sessionID libacp.SessionID) error {
	if b.isClosed() {
		return ErrClosed
	}
	return b.conn.CancelPrompt(sessionID)
}

// RunShellLine runs one operator line against the session's warm shell WITHOUT
// an LLM turn, and returns immediately. ShellRunStarted is emitted before the
// call and ShellRunResult after it; the output itself streams as TerminalChunk
// events, so a UI renders the shell the same way whether the line came from the
// operator or from the agent's own tool.
//
// The returned error covers admission only. A runtime with no shell sessions
// answers method-not-found, which surfaces as ErrShellDisabled on
// ShellRunResult.Err.
func (b *Bridge) RunShellLine(sessionID libacp.SessionID, line string) error {
	if b.isClosed() {
		return ErrClosed
	}
	if sessionID == "" {
		return fmt.Errorf("enginebridge: session id is required")
	}
	if line == "" {
		return fmt.Errorf("enginebridge: empty shell line")
	}

	params, err := json.Marshal(struct {
		SessionID string `json:"sessionId"`
		Command   string `json:"command"`
	}{SessionID: string(sessionID), Command: line})
	if err != nil {
		return fmt.Errorf("enginebridge: marshal terminal run params: %w", err)
	}

	if !b.admit() {
		return ErrClosed
	}
	go func() {
		defer b.wg.Done()
		b.emit(ShellRunStarted{SessionID: sessionID, Command: line})

		raw, callErr := b.conn.CallExtMethod(b.runCtx, extMethodTerminalRun, params)
		if callErr != nil {
			b.emit(ShellRunResult{SessionID: sessionID, Err: classifyShellError(callErr)})
			return
		}
		var res struct {
			Offset  int64  `json:"offset"`
			Started bool   `json:"started"`
			Output  string `json:"output"`
		}
		if len(raw) > 0 {
			if uErr := json.Unmarshal(raw, &res); uErr != nil {
				b.emit(ShellRunResult{SessionID: sessionID, Err: fmt.Errorf("enginebridge: decode terminal run result: %w", uErr)})
				return
			}
		}
		b.emit(ShellRunResult{
			SessionID: sessionID,
			Offset:    res.Offset,
			Started:   res.Started,
			Snapshot:  res.Output,
		})
	}()
	return nil
}

// classifyShellError turns the agent's method-not-found answer into the typed
// "the feature is absent" sentinel. Only a TYPED *libacp.Error counts — string
// matching would swallow unrelated failures.
func classifyShellError(err error) error {
	var e *libacp.Error
	if errors.As(err, &e) && e != nil && e.Code == libacp.ErrMethodNotFound {
		return ErrShellDisabled
	}
	return err
}

// stopQueue performs the half of teardown that has two triggers: Close, and the
// context New was given being cancelled. It releases everything parked on the
// bridge — pending permission requests resolve cancelled, the pump stops and
// closes Events() — and installs the admission barrier: once done is closed,
// the write lock cannot be taken until every submitter that already passed the
// isClosed check has finished its wg.Add, so a subsequent Wait counts them all
// and can never race an Add.
//
// The sync.Once covers ONLY this half. Close still runs its join and
// Transport.Close exactly once afterwards, whether or not cancellation got here
// first.
func (b *Bridge) stopQueue() {
	b.stopOnce.Do(func() {
		b.qmu.Lock()
		b.qdone = true
		b.queue = nil
		b.qmu.Unlock()
		close(b.done)

		// A barrier, not a critical section: the write lock cannot be taken
		// until every admit() already inside has released its read lock, and
		// none that arrives later can get past the isClosed check it now makes
		// under that lock. After this line the WaitGroup counter is final, so
		// joinWait's Wait can neither miss a goroutine nor race an Add.
		b.admitMu.Lock()
		b.admitMu.Unlock() //nolint:staticcheck // SA2001: the empty body IS the barrier
	})
}

// Close tears the bridge down in the one order that works:
//
//  0. end the operator-inbox subscription, while its consumer is still live so
//     the bus's final drain has somewhere to land (stopInboxWatch);
//  1. release everything parked on the bridge — pending permission requests
//     resolve cancelled, the event pump stops, admission closes (stopQueue,
//     which cancelling New's context may already have run);
//  2. cancel the run context, which ends both Run loops and any in-flight
//     prompt or shell call;
//  3. join both Run loops and this package's goroutines, BOUNDED and
//     CONCURRENTLY under ONE deadline — a wedged connection is reported, not
//     waited on, and two wedged halves cost one timeout, not two;
//  4. Transport.Close with a FRESH context, because the run context is dead by
//     then and Transport.Close does real database work — but ONLY if step 3
//     joined everything.
//
// # The returned error is a verdict on the caller's next move
//
// nil — and ONLY nil — means the loopback is fully joined and the Transport is
// closed: the caller proceeds to close the bus and, after it, the database.
//
// Any non-nil error means the teardown did not complete, and the caller MUST
// NOT close the bus or the database. Two shapes reach it:
//
//   - wrapping ErrUncleanShutdown: a join gave up, so goroutines that may still
//     be running against sessions, drivers and database handles were ABANDONED.
//     The Transport was deliberately left open — a leaked goroutine holding a
//     live handle is survivable; the same goroutine writing into a closed one
//     corrupts. Leaking beats corrupting.
//   - a transport close failure: everything joined, but acpsvc's own teardown
//     (MCP deregistration, driver shutdown) failed part-way, so what it owns
//     against the database is in an unknown state.
//
// Either way the right response is the same: report it and exit the process,
// letting the OS reclaim what this package would not. That is why the rule is
// stated on the error's presence, not on its identity — a caller never has to
// classify.
//
// Close is idempotent and returns the same error on every call.
func (b *Bridge) Close() error {
	b.closeOnce.Do(func() {
		// Ahead of stopQueue on purpose: the inbox subscription's final drain
		// only reaches the operator if the consumer is still running and the
		// queue still open (see stopInboxWatch).
		b.stopInboxWatch()

		b.stopQueue()

		b.runCancel()

		// One deadline for all three joins, shared as a context because a
		// timer channel can only be received once: three joins racing a single
		// time.After would leave two of them blocked forever. Worst-case Close
		// is therefore shutdownJoinTimeout + transportCloseTimeout, not 3x the
		// join bound.
		joinCtx, cancelJoin := context.WithTimeout(context.Background(), shutdownJoinTimeout)
		defer cancelJoin()

		agentErr := make(chan error, 1)
		go func() { agentErr <- joinRun(joinCtx, "agent", b.agentDone) }()
		clientJoin := joinRun(joinCtx, "client", b.clientDone)

		var errs []error
		if err := <-agentErr; err != nil {
			errs = append(errs, err)
		}
		if err := clientJoin; err != nil {
			errs = append(errs, err)
		}
		if err := joinWait(joinCtx, &b.wg); err != nil {
			errs = append(errs, err)
		}

		if len(errs) > 0 {
			// Deliberately NOT closing the Transport: it tears down drivers and
			// runs database work, which is precisely what an abandoned handler
			// may still be touching.
			b.closeErr = errors.Join(append([]error{ErrUncleanShutdown}, errs...)...)
			return
		}

		closeCtx, cancel := context.WithTimeout(context.Background(), transportCloseTimeout)
		defer cancel()
		if err := b.transport.Close(closeCtx); err != nil {
			b.closeErr = fmt.Errorf("enginebridge: transport close: %w", err)
		}
	})
	return b.closeErr
}

// joinRun waits for one Run loop until ctx's deadline. Shutdown-shaped errors
// are the expected result of cancelling the context out from under a reader and
// are not reported.
func joinRun(ctx context.Context, name string, done <-chan error) error {
	select {
	case err := <-done:
		if isShutdownNoise(err) {
			return nil
		}
		return fmt.Errorf("enginebridge: %s connection: %w", name, err)
	case <-ctx.Done():
		return fmt.Errorf("enginebridge: %s connection did not shut down within %s", name, shutdownJoinTimeout)
	}
}

// joinWait waits for this package's own goroutines until ctx's deadline.
//
// The watchdog goroutine is a deliberate, bounded leak: on timeout it stays
// parked in wg.Wait forever, because there is no way to abandon a WaitGroup
// join. That is acceptable exactly once per process — Close is terminal, and a
// timeout here already means the process is exiting with goroutines abandoned
// (see ErrUncleanShutdown).
func joinWait(ctx context.Context, wg *sync.WaitGroup) error {
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		wg.Wait()
	}()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("enginebridge: bridge goroutines did not finish within %s", shutdownJoinTimeout)
	}
}

func isShutdownNoise(err error) bool {
	switch {
	case err == nil,
		errors.Is(err, context.Canceled),
		errors.Is(err, io.EOF),
		errors.Is(err, io.ErrClosedPipe),
		errors.Is(err, libacp.ErrConnectionClosed):
		return true
	}
	return false
}

func (b *Bridge) isClosed() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

func (b *Bridge) hasInflight(id libacp.SessionID) bool {
	b.promptMu.Lock()
	defer b.promptMu.Unlock()
	_, ok := b.inflight[id]
	return ok
}

// routingClient is the libacp.Client the connection holds for its whole life.
// It embeds the real implementation (so permission, fs and terminal methods
// forward unchanged) and swaps only the session/update path, because
// FilterSessionUpdates builds an immutable wrapper and the active session
// changes at runtime.
type routingClient struct {
	*bridgeClient

	mu       sync.RWMutex
	live     libacp.SessionID
	filtered libacp.Client
}

func (r *routingClient) SessionUpdate(ctx context.Context, n libacp.SessionNotification) error {
	r.mu.RLock()
	f := r.filtered
	r.mu.RUnlock()
	if f == nil {
		return r.bridgeClient.SessionUpdate(ctx, n)
	}
	return f.SessionUpdate(ctx, n)
}

// setActive installs a fresh filter for id, or removes filtering entirely when
// id is empty.
func (r *routingClient) setActive(id libacp.SessionID) {
	var f libacp.Client
	if id != "" {
		f = libacp.FilterSessionUpdates(id, r.bridgeClient)
	}
	r.mu.Lock()
	r.live = id
	r.filtered = f
	r.mu.Unlock()
}

func (r *routingClient) active() libacp.SessionID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.live
}

// bridgeClient is the Bridge's libacp.Client: the two reverse calls beam
// actually answers, and UnimplementedClient for the rest. The fs and terminal
// methods stay unimplemented deliberately — Initialize declares those
// capabilities false, so acpsvc never calls them (fileio.go falls back to
// direct OS IO).
type bridgeClient struct {
	libacp.UnimplementedClient
	b *Bridge
}

// SessionUpdate is called inline on libacp's read loop, one line at a time,
// which is exactly what makes wire order observable here. It must not block:
// emit appends to an unbounded queue and returns.
func (c *bridgeClient) SessionUpdate(_ context.Context, n libacp.SessionNotification) error {
	c.b.emit(translate(n))
	return nil
}

// RequestPermission surfaces a HITL gate and BLOCKS until the operator answers,
// the turn is cancelled, or the bridge closes. libacp dispatches each inbound
// request on its own goroutine, so blocking here stalls only this tool call.
func (c *bridgeClient) RequestPermission(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	b := c.b
	answer := make(chan bool, 1)
	var once sync.Once
	resolve := func(allow bool) {
		once.Do(func() { answer <- allow })
	}

	meta, _ := approvalflow.ParseMeta(req.ToolCall.Meta)
	if meta.IsZero() {
		meta, _ = approvalflow.ParseMeta(req.Meta)
	}

	b.pendingPerms.Add(1)
	defer b.pendingPerms.Add(-1)

	b.emit(PermissionRequested{
		SessionID:  req.SessionID,
		ToolCallID: req.ToolCall.ToolCallID,
		Title:      req.ToolCall.Title,
		Kind:       req.ToolCall.Kind,
		Status:     req.ToolCall.Status,
		Meta:       meta,
		Contents:   req.ToolCall.Content,
		Locations:  req.ToolCall.Locations,
		RawInput:   req.ToolCall.RawInput,
		Options:    req.Options,
		Resolve:    resolve,
	})

	// Every arm below emits PermissionResolved before returning, so a card
	// retires on a fact rather than on the consumer inferring one. The teardown
	// arm's emit is a no-op by construction (the queue is already stopped when
	// done closes) — that is not a gap: the consumer's Events() channel is
	// closing in the same instant, which retires every card at once.
	resolved := func(kind libacp.PermissionOutcomeKind) {
		b.emit(PermissionResolved{
			SessionID:  req.SessionID,
			ToolCallID: req.ToolCall.ToolCallID,
			Outcome:    kind,
		})
	}

	select {
	case allow := <-answer:
		optionID := approvalflow.OptionDeny
		if allow {
			optionID = approvalflow.OptionAllow
		}
		resolved(libacp.PermissionOutcomeSelected)
		return libacp.RequestPermissionResponse{
			Outcome: libacp.RequestPermissionOutcome{
				Outcome:  libacp.PermissionOutcomeSelected,
				OptionID: optionID,
			},
		}, nil
	case <-ctx.Done():
		// The turn was cancelled (libacp cancels the request context) — answer
		// cancelled, which acpsvc maps to context.Canceled for the tool call.
		resolved(libacp.PermissionOutcomeCancelled)
		return cancelledPermission(), nil
	case <-b.done:
		// Teardown: resolve rather than leak. No card outlives the bridge.
		resolved(libacp.PermissionOutcomeCancelled)
		return cancelledPermission(), nil
	}
}

func cancelledPermission() libacp.RequestPermissionResponse {
	return libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeCancelled},
	}
}

var (
	_ libacp.Client = (*bridgeClient)(nil)
	_ libacp.Client = (*routingClient)(nil)
)
