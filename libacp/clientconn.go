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

// ClientSideConnection is the editor-side mirror of AgentSideConnection: it
// dispatches agent->client requests to a Client and exposes the client->agent
// methods as outbound calls.
type ClientSideConnection struct {
	reader *ndjsonReader
	writer *ndjsonWriter
	closer io.Closer

	client Client

	pendingMu sync.Mutex
	pending   map[int64]chan *Response

	nextID atomic.Int64

	reqCancelMu    sync.Mutex
	requestCancels map[string]context.CancelFunc

	turnMu      sync.Mutex
	promptTurns map[SessionID]*clientPromptTurn

	permsMu      sync.Mutex
	pendingPerms map[string]*pendingPerm

	extRequest      ExtRequestHandler
	extNotification ExtNotificationHandler

	handlerMu sync.Mutex
	draining  bool
	handlers  sync.WaitGroup

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

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

type clientPromptTurn struct {
	cancelled atomic.Bool
}

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

// SetExtRequestHandler installs h to handle inbound extension requests; nil
// answers them with MethodNotFound. Call it from the ClientFactory.
func (c *ClientSideConnection) SetExtRequestHandler(h ExtRequestHandler) {
	c.extRequest = h
}

// SetExtNotificationHandler installs h to handle inbound extension
// notifications; nil ignores them. Call it from the ClientFactory.
func (c *ClientSideConnection) SetExtNotificationHandler(h ExtNotificationHandler) {
	c.extNotification = h
}

func (c *ClientSideConnection) Run(ctx context.Context) (err error) {
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
		}
	}()

	for {
		line, err := c.reader.Next()
		if err != nil {
			// A canceled ctx closes the transport under the reader; report the
			// cancellation, not the read error.
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
			release()
		}
	case IncomingKindNotification:
		// Applied inline on the read loop so a cancel observes its request's
		// registration in wire order.
		if msg.Notification.Method == MethodCancelRequest {
			c.applyCancelRequest(msg.Notification.Params)
		}
		// Handled inline: session/update chunks must reach Client.SessionUpdate
		// in the order sent.
		c.handleNotification(ctx, msg.Notification)
	}
}

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

func (c *ClientSideConnection) promptCancelling(sessionID SessionID) bool {
	c.turnMu.Lock()
	pt, ok := c.promptTurns[sessionID]
	c.turnMu.Unlock()
	return ok && pt.cancelled.Load()
}

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

func (c *ClientSideConnection) handleNotification(ctx context.Context, n Notification) {
	switch n.Method {
	case MethodSessionUpdate:
		var p SessionNotification
		if err := json.Unmarshal(n.Params, &p); err != nil {
			return
		}
		_ = c.client.SessionUpdate(ctx, p)
	default:
		if IsExtensionMethod(n.Method) && c.extNotification != nil {
			c.extNotification(ctx, n.Method, n.Params)
		}
	}
}

func (c *ClientSideConnection) handleRequest(ctx context.Context, req Request, pp *pendingPerm) {
	result, rpcErr := c.safeCallMethod(ctx, req)
	c.writeResult(req.ID, pp, result, rpcErr)
}

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
		// params schema.
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

// SetSessionModel switches a session to a different ModelInfo.ID via the
// unstable Zed model-picker method (session/set_model).
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

// Prompt sends "session/prompt" and registers the session's turn for the call's
// duration so CancelPrompt can find it.
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

// CancelSession sends the "session/cancel" notification; the agent resolves the
// in-flight session/prompt call itself with stop reason "cancelled". Use
// CancelPrompt for the pending-permission auto-cancel rule.
func (c *ClientSideConnection) CancelSession(req CancelNotification) error {
	return c.notify(MethodSessionCancel, req)
}

// CancelPrompt cancels sessionID's in-flight prompt turn: it sends
// "session/cancel" and auto-resolves every pending session/request_permission
// for that session as "cancelled". With no outstanding Prompt call it behaves
// like CancelSession.
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
// IsExtensionMethod) to the agent and returns its raw result.
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
