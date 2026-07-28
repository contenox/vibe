package libacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// ClientSideConnection is the editor-side mirror of AgentSideConnection
// (conn.go): it dispatches incoming agent->client requests (session/request_
// permission, fs/*, terminal/*) and the session/update notification to a
// Client, and exposes the client->agent methods (initialize, session/new,
// session/prompt, ...) as outbound calls. Wire framing, id correlation, and
// shutdown behavior follow the same design as AgentSideConnection.
type ClientSideConnection struct {
	reader *ndjsonReader
	writer *ndjsonWriter
	closer io.Closer

	client Client

	pendingMu sync.Mutex
	pending   map[int64]chan *Response

	nextID atomic.Int64

	// requestCancels tracks every in-flight incoming (agent->client) request's
	// cancelable context by JSON-RPC id, so "$/cancel_request" can abort it.
	reqCancelMu    sync.Mutex
	requestCancels map[string]context.CancelFunc

	// turnMu guards promptTurns: one entry per session with an outstanding
	// outbound session/prompt call, so CancelPrompt knows which
	// session/request_permission requests (pendingPerms below) belong to a
	// turn it is cancelling.
	turnMu      sync.Mutex
	promptTurns map[SessionID]*clientPromptTurn

	// permsMu guards pendingPerms: every in-flight inbound
	// session/request_permission request by JSON-RPC id, so CancelPrompt can
	// force-resolve the ones belonging to a session it cancels.
	permsMu      sync.Mutex
	pendingPerms map[string]*pendingPerm

	// extRequest/extNotification: optional inbound handlers for extension
	// methods and notifications (IsExtensionMethod), installed via
	// SetExtRequestHandler / SetExtNotificationHandler from the ClientFactory
	// before Run starts reading.
	extRequest      ExtRequestHandler
	extNotification ExtNotificationHandler

	// handlerMu makes admission of a new handler goroutine and "shutdown has
	// begun" a single atomic step, so handlers.Add can never race handlers.Wait.
	handlerMu sync.Mutex
	draining  bool
	handlers  sync.WaitGroup

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

// goHandler spawns fn as a tracked handler goroutine and reports whether it
// was started; refuses once shutdown has begun (see AgentSideConnection.goHandler).
func (c *ClientSideConnection) goHandler(fn func()) bool {
	c.handlerMu.Lock()
	if c.draining {
		c.handlerMu.Unlock()
		return false
	}
	c.handlers.Add(1)
	c.handlerMu.Unlock()
	go func() {
		defer c.handlers.Done()
		fn()
	}()
	return true
}

// waitHandlers blocks until every handler goroutine spawned by goHandler
// returns. Call only after shutdown, so everything a handler could be parked
// on has already been released (see AgentSideConnection.waitHandlers).
func (c *ClientSideConnection) waitHandlers() error {
	c.handlerMu.Lock()
	c.draining = true
	c.handlerMu.Unlock()

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		c.handlers.Wait()
	}()
	timer := time.NewTimer(HandlerDrainTimeout)
	defer timer.Stop()
	select {
	case <-drained:
		return nil
	case <-timer.C:
		return ErrHandlerDrainTimeout
	}
}

// clientPromptTurn tracks whether CancelPrompt has marked a session's
// outstanding session/prompt call for cancellation. Prompt's own cleanup
// removes the entry only if it is still the current one for its session, so
// a later session/prompt call on the same session never observes a stale mark.
type clientPromptTurn struct {
	cancelled atomic.Bool
}

// pendingPerm is one in-flight session/request_permission request.
// CancelPrompt's forced cancellation (forceCancelSessionPermissions) and the
// normal handler-completion path (writeResult) race to resolve it; resolve
// guards that race so exactly one response reaches the wire.
type pendingPerm struct {
	id        RequestID
	sessionID SessionID
	resolve   sync.Once
}

func NewClientSideConnection(rw io.ReadWriteCloser, factory ClientFactory) *ClientSideConnection {
	c := &ClientSideConnection{
		reader:         newNDJSONReader(rw),
		writer:         newNDJSONWriter(rw),
		closer:         rw,
		pending:        make(map[int64]chan *Response),
		requestCancels: make(map[string]context.CancelFunc),
		promptTurns:    make(map[SessionID]*clientPromptTurn),
		pendingPerms:   make(map[string]*pendingPerm),
		closed:         make(chan struct{}),
	}
	c.client = factory(c)
	return c
}

// SetExtRequestHandler installs h to handle inbound extension requests
// (method names starting with ExtensionMethodPrefix). Call it from the
// ClientFactory before Run starts reading. Nil (the default) answers
// extension requests with MethodNotFound.
func (c *ClientSideConnection) SetExtRequestHandler(h ExtRequestHandler) {
	c.extRequest = h
}

// SetExtNotificationHandler installs h to handle inbound extension
// notifications. Call it from the ClientFactory before Run starts reading.
// Nil (the default) silently ignores extension notifications.
func (c *ClientSideConnection) SetExtNotificationHandler(h ExtNotificationHandler) {
	c.extNotification = h
}

func (c *ClientSideConnection) Run(ctx context.Context) (err error) {
	// Deferred first so it runs last: shutdown must unblock everything before
	// the join. By the time Run returns, no handler goroutine is executing.
	defer func() {
		if derr := c.waitHandlers(); derr != nil {
			err = errors.Join(err, derr)
		}
	}()
	defer c.shutdown(nil)

	go func() {
		select {
		case <-ctx.Done():
			c.shutdown(ctx.Err())
		case <-c.closed:
			// Connection ended on its own; exit instead of leaking until ctx dies.
		}
	}()

	for {
		line, err := c.reader.Next()
		if err != nil {
			// A canceled ctx closes the transport out from under the reader, so
			// report the cancellation, not the resulting read error.
			if ctxErr := ctx.Err(); ctxErr != nil {
				c.shutdown(ctxErr)
				return ctxErr
			}
			if errors.Is(err, io.EOF) {
				c.shutdown(nil)
				return nil
			}
			c.shutdown(err)
			return err
		}
		c.dispatch(ctx, line)
	}
}

func (c *ClientSideConnection) Closed() <-chan struct{} { return c.closed }

func (c *ClientSideConnection) CloseErr() error {
	<-c.closed
	return c.closeErr
}

func (c *ClientSideConnection) shutdown(err error) {
	c.closeOnce.Do(func() {
		c.closeErr = err

		// Admission closes before the cancel sweep below, so every handler
		// that will ever run has its cancel func already registered by then.
		c.handlerMu.Lock()
		c.draining = true
		c.handlerMu.Unlock()

		_ = c.closer.Close()

		c.pendingMu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.pendingMu.Unlock()

		c.reqCancelMu.Lock()
		for key, cancel := range c.requestCancels {
			cancel()
			delete(c.requestCancels, key)
		}
		c.reqCancelMu.Unlock()

		close(c.closed)
	})
}

func (c *ClientSideConnection) dispatch(ctx context.Context, line []byte) {
	msg, err := ParseIncoming(line)
	if err != nil {
		c.respondToMalformed(line, err)
		return
	}
	switch msg.Kind {
	case IncomingKindResponse:
		c.deliverResponse(msg.Response)
	case IncomingKindRequest:
		// Registered by JSON-RPC id before the handler goroutine spawns, so a
		// later "$/cancel_request" is guaranteed to observe it. Running on its
		// own goroutine means a slow handler (e.g. a session/request_permission
		// dialog awaiting user input) never blocks the read loop.
		reqCtx, cancelReq := context.WithCancel(ctx)
		key := requestCancelKey(msg.Request.ID)
		c.reqCancelMu.Lock()
		c.requestCancels[key] = cancelReq
		c.reqCancelMu.Unlock()

		var pp *pendingPerm
		if msg.Request.Method == MethodSessionRequestPermission {
			pp = c.registerPendingPerm(msg.Request)
		}

		release := func() {
			c.reqCancelMu.Lock()
			delete(c.requestCancels, key)
			c.reqCancelMu.Unlock()
			cancelReq()
			if pp != nil {
				c.unregisterPendingPerm(pp)
			}
		}
		if !c.goHandler(func() {
			defer release()
			c.handleRequest(reqCtx, msg.Request, pp)
		}) {
			// Shutdown already under way; no response could reach the agent
			// anyway. Drop the request and undo dispatch's registrations.
			release()
		}
	case IncomingKindNotification:
		// "$/cancel_request" is applied inline on the read loop so a cancel
		// always observes its request's registration in wire order.
		if msg.Notification.Method == MethodCancelRequest {
			c.applyCancelRequest(msg.Notification.Params)
		}
		// Notifications are handled inline, not spawned onto their own
		// goroutine: session/update chunks must reach Client.SessionUpdate in
		// the order sent, which only the single-threaded read loop guarantees.
		// A slow handler delays the next line being read; acceptable here.
		c.handleNotification(ctx, msg.Notification)
	}
}

// registerPendingPerm records an in-flight session/request_permission request
// by JSON-RPC id and session, so CancelPrompt can find and force-resolve it
// later. Returns nil when params carry no usable sessionId.
func (c *ClientSideConnection) registerPendingPerm(req Request) *pendingPerm {
	var probe struct {
		SessionID SessionID `json:"sessionId"`
	}
	if len(req.Params) == 0 || json.Unmarshal(req.Params, &probe) != nil || probe.SessionID == "" {
		return nil
	}
	pp := &pendingPerm{id: req.ID, sessionID: probe.SessionID}
	c.permsMu.Lock()
	c.pendingPerms[requestCancelKey(req.ID)] = pp
	c.permsMu.Unlock()
	return pp
}

func (c *ClientSideConnection) unregisterPendingPerm(pp *pendingPerm) {
	c.permsMu.Lock()
	delete(c.pendingPerms, requestCancelKey(pp.id))
	c.permsMu.Unlock()
}

// applyCancelRequest aborts the in-flight request the agent no longer awaits.
// Unknown ids are ignored: the request may simply have completed already.
func (c *ClientSideConnection) applyCancelRequest(params json.RawMessage) {
	var p CancelRequestNotification
	if len(params) == 0 || json.Unmarshal(params, &p) != nil {
		return
	}
	c.reqCancelMu.Lock()
	cancel, ok := c.requestCancels[requestCancelKey(p.RequestID)]
	c.reqCancelMu.Unlock()
	if ok {
		cancel()
	}
}

// forceCancelSessionPermissions resolves every in-flight
// session/request_permission request for sessionID with the "cancelled"
// outcome, per the spec's cancellation contract. Each request's context is
// also cancelled, but the response is written unconditionally, racing (and
// normally winning) against whatever the application eventually returns —
// see pendingPerm and writeResult.
func (c *ClientSideConnection) forceCancelSessionPermissions(sessionID SessionID) {
	c.permsMu.Lock()
	var matches []*pendingPerm
	for key, pp := range c.pendingPerms {
		if pp.sessionID == sessionID {
			matches = append(matches, pp)
			delete(c.pendingPerms, key)
		}
	}
	c.permsMu.Unlock()
	if len(matches) == 0 {
		return
	}

	resp := RequestPermissionResponse{Outcome: RequestPermissionOutcome{Outcome: PermissionOutcomeCancelled}}
	for _, pp := range matches {
		c.reqCancelMu.Lock()
		cancel, ok := c.requestCancels[requestCancelKey(pp.id)]
		c.reqCancelMu.Unlock()
		if ok {
			cancel()
		}
		c.writeResult(pp.id, pp, resp, nil)
	}
}

// promptCancelling reports whether CancelPrompt has marked sessionID's
// outstanding session/prompt call for cancellation.
func (c *ClientSideConnection) promptCancelling(sessionID SessionID) bool {
	c.turnMu.Lock()
	pt, ok := c.promptTurns[sessionID]
	c.turnMu.Unlock()
	return ok && pt.cancelled.Load()
}

// respondToMalformed answers input the dispatcher could not parse, per
// JSON-RPC 2.0: -32700 for invalid JSON, -32600 for a structurally invalid
// message (id null when it cannot be salvaged).
func (c *ClientSideConnection) respondToMalformed(line []byte, parseErr error) {
	if !json.Valid(line) {
		_ = c.writer.Write(NewErrorResponse(NewRequestIDNull(), ParseError(parseErr.Error())))
		return
	}
	id := NewRequestIDNull()
	var probe struct {
		ID *json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(line, &probe); err == nil && probe.ID != nil {
		var rid RequestID
		if rid.UnmarshalJSON(*probe.ID) == nil {
			id = rid
		}
	}
	_ = c.writer.Write(NewErrorResponse(id, InvalidRequest(parseErr.Error())))
}

// handleNotification routes an inbound notification. Unknown methods
// (including any not listed below) are ignored, per the JSON-RPC/ACP
// convention that notifications never produce an error response.
func (c *ClientSideConnection) handleNotification(ctx context.Context, n Notification) {
	switch n.Method {
	case MethodSessionUpdate:
		var p SessionNotification
		if err := json.Unmarshal(n.Params, &p); err != nil {
			return
		}
		_ = c.client.SessionUpdate(ctx, p)
	default:
		// Handled inline like every notification on this connection; a
		// blocking handler delays the next line being read (see dispatch).
		if IsExtensionMethod(n.Method) && c.extNotification != nil {
			c.extNotification(ctx, n.Method, n.Params)
		}
	}
}

// handleRequest runs the request's handler and writes its response. pp is
// non-nil only for a session/request_permission request, whose response then
// goes through writeResult's guard against a racing forced cancellation.
func (c *ClientSideConnection) handleRequest(ctx context.Context, req Request, pp *pendingPerm) {
	result, rpcErr := c.safeCallMethod(ctx, req)
	c.writeResult(req.ID, pp, result, rpcErr)
}

// writeResult writes the JSON-RPC response for id. When pp is non-nil the
// write goes through pp.resolve, so at most one of {this call,
// forceCancelSessionPermissions' forced write} reaches the wire (see pendingPerm).
func (c *ClientSideConnection) writeResult(id RequestID, pp *pendingPerm, result any, rpcErr *Error) {
	write := func() {
		if rpcErr != nil {
			_ = c.writer.Write(NewErrorResponse(id, rpcErr))
			return
		}
		resultRaw, err := json.Marshal(result)
		if err != nil {
			_ = c.writer.Write(NewErrorResponse(id, InternalError(err.Error())))
			return
		}
		_ = c.writer.Write(NewResultResponse(id, resultRaw))
	}
	if pp != nil {
		pp.resolve.Do(write)
		return
	}
	write()
}

// safeCallMethod converts a panicking Client handler into an InternalError
// response instead of tearing down the whole process.
func (c *ClientSideConnection) safeCallMethod(ctx context.Context, req Request) (result any, rpcErr *Error) {
	defer func() {
		if r := recover(); r != nil {
			result = nil
			rpcErr = InternalError(fmt.Sprintf("panic in %s handler: %v", req.Method, r))
		}
	}()
	return c.callMethod(ctx, req)
}

func (c *ClientSideConnection) callMethod(ctx context.Context, req Request) (any, *Error) {
	params := req.Params
	if len(params) == 0 {
		// Treat omitted params as {} so all-optional-params methods still unmarshal.
		params = []byte("{}")
	}
	switch req.Method {
	case MethodSessionRequestPermission:
		var p RequestPermissionRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		if c.promptCancelling(p.SessionID) {
			// Once CancelPrompt has cancelled this session's turn, every
			// session/request_permission request for it resolves as
			// "cancelled" without reaching the application.
			return RequestPermissionResponse{Outcome: RequestPermissionOutcome{Outcome: PermissionOutcomeCancelled}}, nil
		}
		resp, err := c.client.RequestPermission(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodFSReadTextFile:
		var p ReadTextFileRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.client.ReadTextFile(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodFSWriteTextFile:
		var p WriteTextFileRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.client.WriteTextFile(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodTerminalCreate:
		var p CreateTerminalRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.client.CreateTerminal(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodTerminalOutput:
		var p TerminalOutputRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.client.TerminalOutput(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodTerminalWaitForExit:
		var p WaitForTerminalExitRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.client.WaitForTerminalExit(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodTerminalKill:
		var p KillTerminalRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.client.KillTerminal(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodTerminalRelease:
		var p ReleaseTerminalRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.client.ReleaseTerminal(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	default:
		// req.Params, not the {} default above: extension methods own their
		// own params schema and must see exactly what arrived on the wire.
		if IsExtensionMethod(req.Method) && c.extRequest != nil {
			result, extErr := c.extRequest(ctx, req.Method, req.Params)
			if extErr != nil {
				return nil, extErr
			}
			return result, nil
		}
		return nil, MethodNotFound(req.Method)
	}
}

func (c *ClientSideConnection) deliverResponse(resp Response) {
	if resp.ID.Kind != RequestIDKindNumber {
		return
	}
	c.pendingMu.Lock()
	ch, ok := c.pending[resp.ID.Number]
	if ok {
		delete(c.pending, resp.ID.Number)
	}
	c.pendingMu.Unlock()
	if !ok {
		return
	}
	ch <- &resp
	close(ch)
}

func (c *ClientSideConnection) call(ctx context.Context, method string, params any, result any) error {
	id := c.nextID.Add(1)
	rid := NewRequestIDNumber(id)

	var paramsRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("libacp: marshal %s params: %w", method, err)
		}
		paramsRaw = b
	}

	ch := make(chan *Response, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	if err := c.writer.Write(NewRequest(rid, method, paramsRaw)); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return fmt.Errorf("libacp: write %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		// Best-effort: tell the peer this response is no longer awaited.
		_ = c.notify(MethodCancelRequest, CancelRequestNotification{RequestID: rid})
		return ctx.Err()
	case <-c.closed:
		return ErrConnectionClosed
	case resp, ok := <-ch:
		if !ok {
			return ErrConnectionClosed
		}
		if resp.Error != nil {
			return resp.Error
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("libacp: unmarshal %s result: %w", method, err)
		}
		return nil
	}
}

func (c *ClientSideConnection) notify(method string, params any) error {
	var paramsRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("libacp: marshal %s params: %w", method, err)
		}
		paramsRaw = b
	}
	return c.writer.Write(NewNotification(method, paramsRaw))
}

func (c *ClientSideConnection) Initialize(ctx context.Context, req InitializeRequest) (InitializeResponse, error) {
	var resp InitializeResponse
	if err := c.call(ctx, MethodInitialize, req, &resp); err != nil {
		return InitializeResponse{}, err
	}
	return resp, nil
}

func (c *ClientSideConnection) Authenticate(ctx context.Context, req AuthenticateRequest) (AuthenticateResponse, error) {
	var resp AuthenticateResponse
	if err := c.call(ctx, MethodAuthenticate, req, &resp); err != nil {
		return AuthenticateResponse{}, err
	}
	return resp, nil
}

// Logout is only meaningful when the agent advertised
// AgentCapabilities.Auth.Logout during initialize.
func (c *ClientSideConnection) Logout(ctx context.Context, req LogoutRequest) (LogoutResponse, error) {
	var resp LogoutResponse
	if err := c.call(ctx, MethodLogout, req, &resp); err != nil {
		return LogoutResponse{}, err
	}
	return resp, nil
}

func (c *ClientSideConnection) NewSession(ctx context.Context, req NewSessionRequest) (NewSessionResponse, error) {
	var resp NewSessionResponse
	if err := c.call(ctx, MethodSessionNew, req, &resp); err != nil {
		return NewSessionResponse{}, err
	}
	return resp, nil
}

func (c *ClientSideConnection) LoadSession(ctx context.Context, req LoadSessionRequest) (LoadSessionResponse, error) {
	var resp LoadSessionResponse
	if err := c.call(ctx, MethodSessionLoad, req, &resp); err != nil {
		return LoadSessionResponse{}, err
	}
	return resp, nil
}

func (c *ClientSideConnection) ResumeSession(ctx context.Context, req ResumeSessionRequest) (ResumeSessionResponse, error) {
	var resp ResumeSessionResponse
	if err := c.call(ctx, MethodSessionResume, req, &resp); err != nil {
		return ResumeSessionResponse{}, err
	}
	return resp, nil
}

func (c *ClientSideConnection) CloseSession(ctx context.Context, req CloseSessionRequest) (CloseSessionResponse, error) {
	var resp CloseSessionResponse
	if err := c.call(ctx, MethodSessionClose, req, &resp); err != nil {
		return CloseSessionResponse{}, err
	}
	return resp, nil
}

func (c *ClientSideConnection) DeleteSession(ctx context.Context, req DeleteSessionRequest) (DeleteSessionResponse, error) {
	var resp DeleteSessionResponse
	if err := c.call(ctx, MethodSessionDelete, req, &resp); err != nil {
		return DeleteSessionResponse{}, err
	}
	return resp, nil
}

func (c *ClientSideConnection) ListSessions(ctx context.Context, req ListSessionsRequest) (ListSessionsResponse, error) {
	var resp ListSessionsResponse
	if err := c.call(ctx, MethodSessionList, req, &resp); err != nil {
		return ListSessionsResponse{}, err
	}
	return resp, nil
}

// SetSessionMode switches a session to a different SessionMode.ID, one of the
// ids the session's SessionModeState.AvailableModes advertised.
func (c *ClientSideConnection) SetSessionMode(ctx context.Context, req SetSessionModeRequest) (SetSessionModeResponse, error) {
	var resp SetSessionModeResponse
	if err := c.call(ctx, MethodSessionSetMode, req, &resp); err != nil {
		return SetSessionModeResponse{}, err
	}
	return resp, nil
}

// SetSessionModel switches a session to a different ModelInfo.ID, one of the
// ids the session's SessionModelState.AvailableModels advertised. This is the
// unstable Zed model-picker method (session/set_model), not part of the
// stable ACP spec and subject to change. On success the requested model is
// authoritative: no session/update kind reconfirms it.
func (c *ClientSideConnection) SetSessionModel(ctx context.Context, req SetSessionModelRequest) (SetSessionModelResponse, error) {
	var resp SetSessionModelResponse
	if err := c.call(ctx, MethodSessionSetModel, req, &resp); err != nil {
		return SetSessionModelResponse{}, err
	}
	return resp, nil
}

func (c *ClientSideConnection) SetSessionConfigOption(ctx context.Context, req SetSessionConfigOptionRequest) (SetSessionConfigOptionResponse, error) {
	var resp SetSessionConfigOptionResponse
	if err := c.call(ctx, MethodSessionSetConfigOption, req, &resp); err != nil {
		return SetSessionConfigOptionResponse{}, err
	}
	return resp, nil
}

// Prompt registers req.SessionID's turn in promptTurns for the call's
// duration, so CancelPrompt can mark it and promptCancelling can check
// session/request_permission requests against it. Removed on return, only if
// still this call's own entry, so it can't clobber a later overlapping call.
func (c *ClientSideConnection) Prompt(ctx context.Context, req PromptRequest) (PromptResponse, error) {
	pt := &clientPromptTurn{}
	c.turnMu.Lock()
	c.promptTurns[req.SessionID] = pt
	c.turnMu.Unlock()
	defer func() {
		c.turnMu.Lock()
		if existing, ok := c.promptTurns[req.SessionID]; ok && existing == pt {
			delete(c.promptTurns, req.SessionID)
		}
		c.turnMu.Unlock()
	}()

	var resp PromptResponse
	if err := c.call(ctx, MethodSessionPrompt, req, &resp); err != nil {
		return PromptResponse{}, err
	}
	return resp, nil
}

// CancelSession sends "session/cancel" — a notification, not a request, per
// spec: the agent must resolve the in-flight session/prompt call with stop
// reason "cancelled" rather than answering this call itself. Does not apply
// the pending-permission auto-cancel rule; use CancelPrompt for that.
func (c *ClientSideConnection) CancelSession(req CancelNotification) error {
	return c.notify(MethodSessionCancel, req)
}

// CancelPrompt cancels sessionID's in-flight prompt turn: sends
// "session/cancel" and, for as long as this session's Prompt call remains
// outstanding, auto-resolves every session/request_permission request for
// sessionID (new or in-flight) with the "cancelled" outcome, instead of
// invoking or waiting on Client.RequestPermission — the client-side half of
// the spec's prompt-turn cancellation contract. The auto-resolve mark clears
// the moment the Prompt call for sessionID returns. With no outstanding
// Prompt call for sessionID, behaves exactly like CancelSession.
func (c *ClientSideConnection) CancelPrompt(sessionID SessionID) error {
	c.turnMu.Lock()
	pt, ok := c.promptTurns[sessionID]
	if ok {
		pt.cancelled.Store(true)
	}
	c.turnMu.Unlock()

	if ok {
		c.forceCancelSessionPermissions(sessionID)
	}
	return c.CancelSession(CancelNotification{SessionID: sessionID})
}

// CallExtMethod sends a custom extension request (method must satisfy
// IsExtensionMethod) to the agent and returns its raw result. Outbound half
// of the extension-method seam; SetExtRequestHandler installs the inbound half.
func (c *ClientSideConnection) CallExtMethod(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if !IsExtensionMethod(method) {
		return nil, fmt.Errorf("libacp: %q is not an extension method (must start with %q)", method, ExtensionMethodPrefix)
	}
	var paramsAny any
	if len(params) > 0 {
		paramsAny = params
	}
	var result json.RawMessage
	if err := c.call(ctx, method, paramsAny, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// SendExtNotification sends a custom, fire-and-forget extension notification
// (method must satisfy IsExtensionMethod) to the agent.
func (c *ClientSideConnection) SendExtNotification(method string, params json.RawMessage) error {
	if !IsExtensionMethod(method) {
		return fmt.Errorf("libacp: %q is not an extension method (must start with %q)", method, ExtensionMethodPrefix)
	}
	var paramsAny any
	if len(params) > 0 {
		paramsAny = params
	}
	return c.notify(method, paramsAny)
}
