// Package enginebridge is the seam between a beam surface and the contenox runtime:
// an in-process ACP loopback (acpsvc.Transport wired to a libacp.ClientSideConnection)
// turned into a typed Event vocabulary a renderer can consume without knowing ACP
// exists. The composition root builds and owns shutdown of the engine, database,
// bus and mission fleet (see Deps, Close); Events() delivers exactly one Event per
// admitted update, in wire order, off an unbounded queue.
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

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/services/approvalflow"
	"github.com/contenox/contenox/internal/services/shellsession"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	libacp "github.com/contenox/contenox/libacp"
)

// extMethodTerminalRun mirrors acpsvc's unexported constant of the same name:
// the `$`/`!` passthrough that runs one operator line against the session's
// warm shell without an LLM turn or HITL gating.
const extMethodTerminalRun = "_contenox/terminal/run"

// shutdownJoinTimeout bounds loopback teardown: a Run loop not returned by
// then is reported, not waited on forever. It must exceed
// libacp.HandlerDrainTimeout (both Run loops only return after that drain), or
// a legitimate slow drain would read as an unclean shutdown.
const shutdownJoinTimeout = libacp.HandlerDrainTimeout + 2*time.Second

// transportCloseTimeout bounds Transport.Close, which does real work (MCP
// deregistration, driver teardown) against the database.
const transportCloseTimeout = 10 * time.Second

var (
	// ErrClosed reports a call on a Bridge whose Close has already run.
	ErrClosed = errors.New("enginebridge: bridge is closed")

	// ErrPromptInFlight reports a second SubmitPrompt before the session's
	// previous turn ended; wait for TurnEnded/TurnFailed or Cancel first.
	ErrPromptInFlight = errors.New("enginebridge: a prompt is already in flight for this session")

	// ErrEmptyPrompt reports a SubmitPrompt with nothing in it.
	ErrEmptyPrompt = errors.New("enginebridge: empty prompt")

	// ErrShellDisabled reports the runtime was built without shell sessions
	// (Deps.ShellSessions nil): the feature is absent, not broken.
	ErrShellDisabled = errors.New("enginebridge: shell sessions are not enabled")

	// ErrUncleanShutdown reports Close gave up joining a goroutine within
	// shutdownJoinTimeout; the caller must not close the bus or database
	// afterwards (see Close).
	ErrUncleanShutdown = errors.New("enginebridge: unclean shutdown, resources abandoned")
)

// Deps is everything the Bridge needs from the composition root; nothing here
// is constructed by this package.
type Deps struct {
	// Engine is the warm task-chain engine for the process. Required.
	Engine *enginesvc.Engine
	// DB is the runtime database. Required; caller closes it after Bus.
	DB libdb.DBManager
	// Bus is the process event bus. The Bridge subscribes to it once, to
	// relay operator-inbox items that reached no live session (see inbox.go).
	// Nil disables inbox events. Must be the SQLite backend: it drains on
	// Unsubscribe, which the in-memory backend does not guarantee. The caller
	// closes it after the Bridge and before the database.
	Bus libbus.Messenger
	// Tracker reports bridge lifecycle events (turns ending, approval cards);
	// fields pass through libtracker redaction. Nil defaults to a Noop; beam
	// wires its beam.log-backed tracker.
	Tracker libtracker.ActivityTracker
	// ChainRegistry supplies the default ACP chain. Required: a native turn
	// without one fails with "no chain configured".
	ChainRegistry *acpsvc.ChainRegistry
	// Session defaults acpsvc seeds every new session's config options from,
	// and that chain `{{var:model}}` macros resolve against. DefaultModel is
	// effectively required — a chain naming the macro fails otherwise.
	// `/model` and its siblings mutate these per session afterwards.
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
	// WorkspaceRoots allowlists directories a session may root in; nil
	// accepts any absolute cwd.
	WorkspaceRoots *vfs.Factory
	// ShellSessions manages the per-session PTY; nil disables the terminal
	// passthrough (RunShellLine then reports ErrShellDisabled).
	ShellSessions shellsession.Manager
	// KnownPolicies and HITLDefaultPolicyName are display-only /policy
	// strings; a startup default, never re-asked per call.
	KnownPolicies         []string
	HITLDefaultPolicyName string
	// Fleet and Agents together enable /mission: both must be non-nil.
	Fleet  acpsvc.MissionDispatcher
	Agents acpsvc.MissionAgentResolver
	// SessionRouter is the shared session->transport registry HITL approvals
	// route through; nil for a single-transport process.
	SessionRouter *acpsvc.SessionRouter
	// ClientInfo identifies this client in the handshake; defaults to
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
	// Set once in New before any other goroutine can see the Bridge, and read
	// only by Close, so it needs no lock.
	inboxSub libbus.Subscription

	runCtx    context.Context
	runCancel context.CancelFunc

	agentDone  chan error
	clientDone chan error

	// events is the ordered outlet; queue/notify form the unbounded FIFO
	// behind it (see emit/pump: the producer must never block).
	events chan Event
	qmu    sync.Mutex
	queue  []Event
	qdone  bool
	notify chan struct{}

	// done closes first in teardown, releasing permission waiters and the
	// pump. stopOnce guards that half alone (it has two triggers: Close, and
	// the run context being cancelled); closeOnce guards the full Close
	// sequence (join + Transport.Close).
	done      chan struct{}
	stopOnce  sync.Once
	closeOnce sync.Once
	closeErr  error

	// wg tracks goroutines this package starts so Close can join them.
	// admitMu closes the race between "not closed" and wg.Add: submit paths
	// hold RLock across the isClosed re-check and the Add; teardown takes the
	// write lock after marking closed, so a later Wait counts every admitted
	// goroutine and never races an Add.
	admitMu sync.RWMutex
	wg      sync.WaitGroup

	promptMu sync.Mutex
	inflight map[libacp.SessionID]struct{}

	// pendingPerms counts permission requests awaiting an operator; an
	// observability hook only (Close cannot join in-process requests the way
	// it joins wire handlers).
	pendingPerms atomic.Int64
}

// New builds the loopback and starts both Run loops. Cancelling ctx tears the
// connection down as Close does, minus joining and Transport.Close: prompts
// and shell calls fail, permission requests resolve cancelled, the pump stops
// and Events() closes, and isClosed reports true. Callers must still call
// Close on exit; doing so after cancellation is well-defined.
// track reports one bridge lifecycle event through the composition-chosen
// tracker.
func (b *Bridge) track(ctx context.Context, msg string, kv ...any) {
	_, _, end := b.deps.Tracker.Start(ctx, "beam", msg, kv...)
	end()
}

func New(ctx context.Context, deps Deps) (*Bridge, error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}
	if deps.Tracker == nil {
		deps.Tracker = libtracker.NoopTracker{}
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

	// The inbox subscription is taken before the loopback is built, so a bus
	// that cannot be subscribed to fails New with nothing left to tear down.
	if err := b.startInboxWatch(runCtx); err != nil {
		runCancel()
		return nil, err
	}

	// Two io.Pipe pairs crossed into duplex ReadWriteClosers: the agent's
	// reads are the client's writes and vice versa — an in-memory stand-in
	// for the stdio an editor would use.
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
	// concrete Transport: NewAgentSideConnection invokes the factory eagerly,
	// so transport is populated by the time this returns.
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
	// No active session yet: updates pass through unfiltered so a first
	// session's deferred available_commands_update isn't lost before the
	// caller selects it.
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

	// The context watcher makes New's cancellation promise real: without it a
	// cancelled runCtx would end both Run loops while Events() stayed open.
	// Both arms reach stopQueue via the same sync.Once, so this always returns.
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

// Transport returns the acpsvc.Transport this Bridge drives, for the
// in-process mission fleet's report deliverer (fleetboot.Deps.Transport).
// Nil-safe on a nil receiver, since the fleet is built before the Bridge and
// needs a closure over one that doesn't exist yet — publish that closure
// through an atomic.Pointer, not a closed-over variable, to avoid racing New.
func (b *Bridge) Transport() *acpsvc.Transport {
	if b == nil {
		return nil
	}
	return b.transport
}

// Events returns the single ordered outlet, closed once the pump stops (by
// Close, or by cancellation of the context New was given). A consumer should
// range over it and treat closure as "the bridge is gone."
func (b *Bridge) Events() <-chan Event { return b.events }

// admit reserves a WaitGroup slot for a goroutine this package is about to
// start and reports whether the bridge is still open; it is the only place
// wg.Add may be called after New. A false return means don't start it.
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

// Initialize performs the ACP handshake. It declares no filesystem or
// terminal client capabilities, so acpsvc's ACPFileIO falls back to direct OS
// file IO (fileio.go) — correct for an in-process client on the same machine.
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

// NewSession creates a session: a thin pass-through to acpsvc/session.go. It
// does not change the active session — see SetActiveSession for the call
// order that preserves a new session's deferred available_commands_update.
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

// emitInitialConfigOptions replays a session response's opening config
// options (model/provider groups, think levels, HITL presets) as a
// ConfigOptionUpdated event: they ride the session/new, session/load and
// session/resume responses rather than a notification, so Events() alone
// would otherwise show none until the first change.
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
	// The replay marker rides the same FIFO as the replayed notifications, so
	// by queue order it follows every replayed event. Consumers use it to
	// settle the trailing replayed message.
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

// CloseSession drops a session from this connection, cancelling any
// in-flight turn first so a half-running prompt doesn't leak a goroutine.
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

// SetActiveSession points the session/update filter at id via a fresh
// libacp.FilterSessionUpdates wrapper; without it, an abandoned session's
// updates leak into the current transcript. Empty id means unfiltered —
// every update delivered, tagged with its SessionID — the only way to catch
// the available_commands_update acpsvc defers until after session/new:
//
//	b.SetActiveSession("")
//	resp, _ := b.NewSession(ctx, …)
//	b.SetActiveSession(resp.SessionID)
func (b *Bridge) SetActiveSession(id libacp.SessionID) {
	b.client.setActive(id)
}

// ActiveSession reports the session the update filter is pointed at, or "" when
// updates are unfiltered.
func (b *Bridge) ActiveSession() libacp.SessionID { return b.client.active() }

// SubmitPrompt sends text as a turn and returns immediately; the result
// arrives on Events as TurnEnded (cancel is StopReasonCancelled, not an
// error) or TurnFailed, with streamed output arriving first. The returned
// error covers admission only — one turn per session, non-empty text, bridge
// alive. A turn in flight when torn down never yields a terminal event;
// track pending turns against the close of Events(), not per turn.
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
		// Emitted before the in-flight mark is released, both under promptMu:
		// releasing first would let a racing SubmitPrompt for the same session
		// stream chunks ahead of this turn's TurnEnded/TurnFailed.
		b.promptMu.Lock()
		if err != nil {
			// Always logged, not gated behind --trace: a turn must never end
			// silently, and this is the one line that says it didn't — see
			// the HITL package's hitlLog for why Warn is the level that
			// survives beam.log's handler.
			b.track(b.runCtx, "turn failed", "session_id", string(sessionID), "error", err.Error())
			b.emit(TurnFailed{SessionID: sessionID, Err: err})
		} else {
			b.track(b.runCtx, "turn ended", "session_id", string(sessionID), "stop_reason", string(resp.StopReason))
			b.emit(TurnEnded{SessionID: sessionID, StopReason: resp.StopReason})
		}
		delete(b.inflight, sessionID)
		b.promptMu.Unlock()
	}()
	return nil
}

// Cancel interrupts sessionID's turn via CancelPrompt, not CancelSession: it
// also force-resolves in-flight permission requests for the session as
// cancelled. A session with no in-flight prompt just gets a plain
// session/cancel, which the agent ignores.
func (b *Bridge) Cancel(sessionID libacp.SessionID) error {
	if b.isClosed() {
		return ErrClosed
	}
	return b.conn.CancelPrompt(sessionID)
}

// RunShellLine runs one operator line against the session's warm shell
// without an LLM turn, and returns immediately; output streams as
// TerminalChunk. The returned error covers admission only — a runtime with
// no shell sessions surfaces as ErrShellDisabled on ShellRunResult.Err.
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

// classifyShellError turns the agent's method-not-found answer into the
// typed "feature absent" sentinel; only a typed *libacp.Error counts, so
// unrelated failures pass through unchanged.
func classifyShellError(err error) error {
	var e *libacp.Error
	if errors.As(err, &e) && e != nil && e.Code == libacp.ErrMethodNotFound {
		return ErrShellDisabled
	}
	return err
}

// stopQueue is teardown's first half, triggered by Close or by the run
// context being cancelled: it releases everything parked on the bridge
// (permissions resolve cancelled, the pump stops and Events() closes) and
// installs the admission barrier. The sync.Once covers only this half; Close
// still runs its join and Transport.Close exactly once afterwards either way.
func (b *Bridge) stopQueue() {
	b.stopOnce.Do(func() {
		b.qmu.Lock()
		b.qdone = true
		b.queue = nil
		b.qmu.Unlock()
		close(b.done)

		// A barrier: the write lock can't be taken until every admit() already
		// inside releases its read lock, and none arriving later gets past
		// isClosed under that lock, so the WaitGroup counter is final here.
		b.admitMu.Lock()
		b.admitMu.Unlock() //nolint:staticcheck // SA2001: the empty body is the barrier
	})
}

// Close tears the bridge down in order: end the inbox subscription while its
// consumer is live (stopInboxWatch); release everything parked on the bridge
// (stopQueue); cancel the run context; join both Run loops and this
// package's goroutines under one deadline; then close the Transport with a
// fresh context, only if the join succeeded.
//
// nil means fully joined and closed — the caller closes the bus, then the
// database. Any other error means close neither and exit the process
// instead: ErrUncleanShutdown means a join gave up and abandoned goroutines
// that may still touch sessions, drivers or the database (Transport stays
// open deliberately — leaking beats corrupting); otherwise Transport itself
// failed to close. Close is idempotent.
func (b *Bridge) Close() error {
	b.closeOnce.Do(func() {
		// Ahead of stopQueue: the inbox drain only reaches the operator while
		// the consumer and queue are still open.
		b.stopInboxWatch()

		b.stopQueue()

		b.runCancel()

		// One shared deadline for all three joins — a timer channel receives
		// once, so racing three separate timers would leave two blocked forever.
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
			// Left open deliberately — see the doc comment above.
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

// joinWait waits for this package's own goroutines until ctx's deadline. On
// timeout the watchdog goroutine stays parked in wg.Wait forever (there is no
// way to abandon a WaitGroup join); acceptable once per process, since a
// timeout here already means goroutines are abandoned (see
// ErrUncleanShutdown).
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

// routingClient is the libacp.Client the connection holds for its whole
// life. It embeds the real implementation and swaps only the session/update
// path, since FilterSessionUpdates builds an immutable wrapper and the
// active session changes at runtime.
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
// answers, and UnimplementedClient for the rest — fs and terminal stay
// unimplemented because Initialize declares those capabilities false.
type bridgeClient struct {
	libacp.UnimplementedClient
	b *Bridge
}

// SessionUpdate runs inline on libacp's read loop, one line at a time, which
// is what makes wire order observable here; it must not block.
func (c *bridgeClient) SessionUpdate(_ context.Context, n libacp.SessionNotification) error {
	c.b.emit(translate(n))
	return nil
}

// RequestPermission surfaces a HITL gate and blocks until the operator
// answers, the turn is cancelled, or the bridge closes; libacp dispatches
// each request on its own goroutine, so blocking here stalls only this call.
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

	b.track(ctx, "card shown", "session_id", string(req.SessionID), "tool_call_id", string(req.ToolCall.ToolCallID))
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
	// retires on a fact. The teardown arm's emit is a no-op by construction,
	// but Events() closes in the same instant, retiring every card at once.
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
		b.track(ctx, "verdict entered", "session_id", string(req.SessionID), "tool_call_id", string(req.ToolCall.ToolCallID), "approved", allow)
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
		b.track(ctx, "card cancelled", "session_id", string(req.SessionID), "tool_call_id", string(req.ToolCall.ToolCallID), "reason", "turn_cancelled")
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
