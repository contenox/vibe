package enginebridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/surfaces/beam/dialect"
	libacp "github.com/contenox/contenox/libacp"
)

const extMethodTerminalRun = "_contenox/terminal/run"

const shutdownJoinTimeout = libacp.HandlerDrainTimeout + 2*time.Second

type Deps struct {
	Conn       io.ReadWriteCloser
	ClientInfo *libacp.Implementation

	// Inbox carries raw operatorinbox.Item JSON as published on
	// operatorinbox.AddedSubject; each item reaches Events as
	// InboxItemAdded. Nil means no inbox events.
	Inbox <-chan []byte

	// WorkspaceRoot is the directory this client serves to its agent through
	// the fs and terminal capabilities; every path is contained under it.
	// Empty advertises neither capability, and the agent's client-backed
	// tools are withheld from the session.
	WorkspaceRoot string
}

func (d Deps) validate() error {
	if d.Conn == nil {
		return errors.New("enginebridge: Deps.Conn is required")
	}
	return nil
}

type Bridge struct {
	info *libacp.Implementation
	root string

	terms terminals

	conn   *libacp.ClientSideConnection
	client *routingClient

	runCtx    context.Context
	runCancel context.CancelFunc

	connDone chan error

	events chan Event
	qmu    sync.Mutex
	queue  []Event
	qdone  bool
	notify chan struct{}

	done      chan struct{}
	stopOnce  sync.Once
	closeOnce sync.Once
	closeErr  error

	admitMu sync.RWMutex
	wg      sync.WaitGroup

	promptMu sync.Mutex
	inflight map[libacp.SessionID]struct{}
}

func New(ctx context.Context, deps Deps) (*Bridge, error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}

	runCtx, runCancel := context.WithCancel(ctx)
	b := &Bridge{
		info:      deps.ClientInfo,
		root:      deps.WorkspaceRoot,
		runCtx:    runCtx,
		runCancel: runCancel,
		connDone:  make(chan error, 1),
		events:    make(chan Event),
		notify:    make(chan struct{}, 1),
		done:      make(chan struct{}),
		inflight:  make(map[libacp.SessionID]struct{}),
	}

	b.client = &routingClient{bridgeClient: &bridgeClient{b: b}}
	b.client.setActive("")

	b.conn = libacp.NewClientSideConnection(deps.Conn, func(*libacp.ClientSideConnection) libacp.Client {
		return b.client
	})

	go func() { b.connDone <- b.conn.Run(runCtx) }()

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.pump()
	}()

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		select {
		case <-runCtx.Done():
			b.stopQueue()
		case <-b.done:
		}
	}()

	if deps.Inbox != nil {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.relayInbox(deps.Inbox)
		}()
	}

	return b, nil
}

func (b *Bridge) Events() <-chan Event { return b.events }

func (b *Bridge) admit() bool {
	b.admitMu.RLock()
	defer b.admitMu.RUnlock()
	if b.isClosed() {
		return false
	}
	b.wg.Add(1)
	return true
}

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

func (b *Bridge) Initialize(ctx context.Context) (libacp.InitializeResponse, error) {
	if b.isClosed() {
		return libacp.InitializeResponse{}, ErrClosed
	}
	info := b.info
	if info == nil {
		info = &libacp.Implementation{Name: "beam"}
	}
	// Capabilities follow the workspace: with a root to contain them in, this
	// client serves fs and terminal itself, so the agent's client-backed
	// local_fs and local_shell are advertised into beam sessions.
	caps := libacp.ClientCapabilities{}
	if b.root != "" {
		caps.FS = libacp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true}
		caps.Terminal = true
	}
	return b.conn.Initialize(ctx, libacp.InitializeRequest{
		ProtocolVersion:    libacp.ProtocolVersion,
		ClientCapabilities: caps,
		ClientInfo:         info,
	})
}

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

func (b *Bridge) emitInitialConfigOptions(sid libacp.SessionID, options []libacp.SessionConfigOption) {
	if len(options) == 0 {
		return
	}
	b.emit(ConfigOptionUpdated{SessionID: sid, Options: options})
}

func (b *Bridge) LoadSession(ctx context.Context, req libacp.LoadSessionRequest) (libacp.LoadSessionResponse, error) {
	if b.isClosed() {
		return libacp.LoadSessionResponse{}, ErrClosed
	}
	resp, err := b.conn.LoadSession(ctx, req)
	if err != nil {
		return resp, err
	}
	b.emitInitialConfigOptions(req.SessionID, resp.ConfigOptions)
	b.emit(ReplayEnded{SessionID: req.SessionID})
	return resp, nil
}

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

func (b *Bridge) ListSessions(ctx context.Context, req libacp.ListSessionsRequest) (libacp.ListSessionsResponse, error) {
	if b.isClosed() {
		return libacp.ListSessionsResponse{}, ErrClosed
	}
	return b.conn.ListSessions(ctx, req)
}

func (b *Bridge) CloseSession(ctx context.Context, req libacp.CloseSessionRequest) (libacp.CloseSessionResponse, error) {
	if b.isClosed() {
		return libacp.CloseSessionResponse{}, ErrClosed
	}
	if b.hasInflight(req.SessionID) {
		_ = b.Cancel(req.SessionID)
	}
	return b.conn.CloseSession(ctx, req)
}

func (b *Bridge) DeleteSession(ctx context.Context, req libacp.DeleteSessionRequest) (libacp.DeleteSessionResponse, error) {
	if b.isClosed() {
		return libacp.DeleteSessionResponse{}, ErrClosed
	}
	if b.hasInflight(req.SessionID) {
		_ = b.Cancel(req.SessionID)
	}
	return b.conn.DeleteSession(ctx, req)
}

func (b *Bridge) SetActiveSession(id libacp.SessionID) {
	b.client.setActive(id)
}

func (b *Bridge) ActiveSession() libacp.SessionID { return b.client.active() }

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

func (b *Bridge) Cancel(sessionID libacp.SessionID) error {
	if b.isClosed() {
		return ErrClosed
	}
	return b.conn.CancelPrompt(sessionID)
}

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

func classifyShellError(err error) error {
	var e *libacp.Error
	if errors.As(err, &e) && e != nil && e.Code == libacp.ErrMethodNotFound {
		return ErrShellDisabled
	}
	return err
}

func (b *Bridge) stopQueue() {
	b.stopOnce.Do(func() {
		b.qmu.Lock()
		b.qdone = true
		b.queue = nil
		b.qmu.Unlock()
		close(b.done)

		b.admitBarrier()
	})
}

func (b *Bridge) admitBarrier() {
	b.admitMu.Lock()
	defer b.admitMu.Unlock()
}

func (b *Bridge) Close() error {
	b.closeOnce.Do(func() {
		b.stopQueue()
		b.runCancel()

		joinCtx, cancelJoin := context.WithTimeout(context.Background(), shutdownJoinTimeout)
		defer cancelJoin()

		var errs []error
		if err := joinRun(joinCtx, b.connDone); err != nil {
			errs = append(errs, err)
		}
		if err := joinWait(joinCtx, &b.wg); err != nil {
			errs = append(errs, err)
		}
		if len(errs) > 0 {
			b.closeErr = errors.Join(append([]error{ErrUncleanShutdown}, errs...)...)
		}
	})
	return b.closeErr
}

func joinRun(ctx context.Context, done <-chan error) error {
	select {
	case err := <-done:
		if isShutdownNoise(err) {
			return nil
		}
		return fmt.Errorf("enginebridge: client connection: %w", err)
	case <-ctx.Done():
		return fmt.Errorf("enginebridge: client connection did not shut down within %s", shutdownJoinTimeout)
	}
}

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

type bridgeClient struct {
	libacp.UnimplementedClient
	b *Bridge
}

func (c *bridgeClient) SessionUpdate(_ context.Context, n libacp.SessionNotification) error {
	c.b.emit(translate(n))
	return nil
}

func (c *bridgeClient) RequestPermission(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	b := c.b
	answer := make(chan bool, 1)
	var once sync.Once
	resolve := func(allow bool) {
		once.Do(func() { answer <- allow })
	}

	meta, ok := dialect.ParseMeta(req.ToolCall.Meta)
	if !ok {
		meta, _ = dialect.ParseMeta(req.Meta)
	}

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

	resolved := func(kind libacp.PermissionOutcomeKind) {
		b.emit(PermissionResolved{
			SessionID:  req.SessionID,
			ToolCallID: req.ToolCall.ToolCallID,
			Outcome:    kind,
		})
	}

	select {
	case allow := <-answer:
		optionID := dialect.OptionDeny
		if allow {
			optionID = dialect.OptionAllow
		}
		resolved(libacp.PermissionOutcomeSelected)
		return libacp.RequestPermissionResponse{
			Outcome: libacp.RequestPermissionOutcome{
				Outcome:  libacp.PermissionOutcomeSelected,
				OptionID: optionID,
			},
		}, nil
	case <-ctx.Done():
		resolved(libacp.PermissionOutcomeCancelled)
		return cancelledPermission(), nil
	case <-b.done:
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
