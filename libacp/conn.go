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

var (
	ErrConnectionClosed = errors.New("libacp: connection closed")
)

type afterResponseSink struct {
	mu      sync.Mutex
	flushed bool
	fns     []func()
}

func (s *afterResponseSink) add(fn func()) {
	s.mu.Lock()
	if s.flushed {
		s.mu.Unlock()
		fn()
		return
	}
	s.fns = append(s.fns, fn)
	s.mu.Unlock()
}

func (s *afterResponseSink) run() {
	s.mu.Lock()
	s.flushed = true
	fns := s.fns
	s.fns = nil
	s.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

type afterResponseKey struct{}

// AfterResponse schedules fn to run once the current request's result is on the
// wire; outside a request handler fn runs immediately.
func AfterResponse(ctx context.Context, fn func()) {
	if sink, ok := ctx.Value(afterResponseKey{}).(*afterResponseSink); ok {
		sink.add(fn)
		return
	}
	fn()
}

type AgentSideConnection struct {
	reader *ndjsonReader
	writer *ndjsonWriter
	closer io.Closer

	agent Agent

	pendingMu sync.Mutex
	pending   map[int64]chan *Response

	nextID atomic.Int64

	cancelMu       sync.Mutex
	sessionCancels map[SessionID]*promptCancel

	reqCancelMu    sync.Mutex
	requestCancels map[string]context.CancelFunc

	extRequest      ExtRequestHandler
	extNotification ExtNotificationHandler

	handlerMu sync.Mutex
	draining  bool
	handlers  sync.WaitGroup

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

func (c *AgentSideConnection) goHandler(fn func()) bool {
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

func (c *AgentSideConnection) waitHandlers() error {
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

func requestCancelKey(id RequestID) string {
	return fmt.Sprintf("%d:%s", id.Kind, id.String_())
}

type promptCancel struct {
	sessionID SessionID
	ctx       context.Context
	cancel    context.CancelFunc
}

func NewAgentSideConnection(rw io.ReadWriteCloser, factory AgentFactory) *AgentSideConnection {
	c := &AgentSideConnection{
		reader:         newNDJSONReader(rw),
		writer:         newNDJSONWriter(rw),
		closer:         rw,
		pending:        make(map[int64]chan *Response),
		sessionCancels: make(map[SessionID]*promptCancel),
		requestCancels: make(map[string]context.CancelFunc),
		closed:         make(chan struct{}),
	}
	c.agent = factory(c)
	return c
}

// SetExtRequestHandler installs h to handle inbound extension requests; nil
// answers them with MethodNotFound. Call it from the AgentFactory.
func (c *AgentSideConnection) SetExtRequestHandler(h ExtRequestHandler) {
	c.extRequest = h
}

// SetExtNotificationHandler installs h to handle inbound extension
// notifications; nil ignores them. Call it from the AgentFactory.
func (c *AgentSideConnection) SetExtNotificationHandler(h ExtNotificationHandler) {
	c.extNotification = h
}

func (c *AgentSideConnection) Run(ctx context.Context) (err error) {
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

func (c *AgentSideConnection) Closed() <-chan struct{} { return c.closed }

func (c *AgentSideConnection) CloseErr() error {
	<-c.closed
	return c.closeErr
}

func (c *AgentSideConnection) shutdown(err error) {
	c.closeOnce.Do(func() {
		c.closeErr = err

		// Barring admission first guarantees the cancel loops below see every
		// handler that will ever run.
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

		c.cancelMu.Lock()
		for sid, pc := range c.sessionCancels {
			pc.cancel()
			delete(c.sessionCancels, sid)
		}
		c.cancelMu.Unlock()

		c.reqCancelMu.Lock()
		for key, cancel := range c.requestCancels {
			cancel()
			delete(c.requestCancels, key)
		}
		c.reqCancelMu.Unlock()

		close(c.closed)
	})
}

func (c *AgentSideConnection) dispatch(ctx context.Context, line []byte) {
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

		var pc *promptCancel
		if msg.Request.Method == MethodSessionPrompt {
			pc = c.registerPromptCancel(reqCtx, msg.Request.Params)
		}
		release := func() {
			c.reqCancelMu.Lock()
			delete(c.requestCancels, key)
			c.reqCancelMu.Unlock()
			cancelReq()
		}
		if !c.goHandler(func() {
			defer release()
			c.handleRequest(reqCtx, msg.Request, pc)
		}) {
			release()
			if pc != nil {
				c.unregisterPromptCancel(pc)
			}
		}
	case IncomingKindNotification:
		// Applied inline on the read loop so a cancel observes its request's
		// registration in wire order.
		switch msg.Notification.Method {
		case MethodSessionCancel:
			c.applySessionCancel(msg.Notification.Params)
		case MethodCancelRequest:
			c.applyCancelRequest(msg.Notification.Params)
		}
		_ = c.goHandler(func() { c.handleNotification(ctx, msg.Notification) })
	}
}

func (c *AgentSideConnection) applyCancelRequest(params json.RawMessage) {
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

func (c *AgentSideConnection) respondToMalformed(line []byte, parseErr error) {
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

func (c *AgentSideConnection) registerPromptCancel(ctx context.Context, params json.RawMessage) *promptCancel {
	var probe struct {
		SessionID SessionID `json:"sessionId"`
	}
	if len(params) == 0 || json.Unmarshal(params, &probe) != nil || probe.SessionID == "" {
		return nil
	}
	promptCtx, cancel := context.WithCancel(ctx)
	pc := &promptCancel{sessionID: probe.SessionID, ctx: promptCtx, cancel: cancel}
	c.cancelMu.Lock()
	if prev, ok := c.sessionCancels[probe.SessionID]; ok {
		// A second prompt on a busy session supersedes the first turn.
		prev.cancel()
	}
	c.sessionCancels[probe.SessionID] = pc
	c.cancelMu.Unlock()
	return pc
}

func (c *AgentSideConnection) unregisterPromptCancel(pc *promptCancel) {
	c.cancelMu.Lock()
	if existing, ok := c.sessionCancels[pc.sessionID]; ok && existing == pc {
		delete(c.sessionCancels, pc.sessionID)
	}
	c.cancelMu.Unlock()
	pc.cancel()
}

func (c *AgentSideConnection) applySessionCancel(params json.RawMessage) {
	var p CancelNotification
	if len(params) == 0 || json.Unmarshal(params, &p) != nil {
		return
	}
	c.cancelMu.Lock()
	if pc, ok := c.sessionCancels[p.SessionID]; ok {
		pc.cancel()
		delete(c.sessionCancels, p.SessionID)
	}
	c.cancelMu.Unlock()
}

func (c *AgentSideConnection) handleRequest(ctx context.Context, req Request, pc *promptCancel) {
	sink := &afterResponseSink{}
	ctx = context.WithValue(ctx, afterResponseKey{}, sink)

	result, rpcErr := c.safeCallMethod(ctx, req, pc)
	if rpcErr != nil {
		_ = c.writer.Write(NewErrorResponse(req.ID, rpcErr))
		return
	}
	resultRaw, err := json.Marshal(result)
	if err != nil {
		_ = c.writer.Write(NewErrorResponse(req.ID, InternalError(err.Error())))
		return
	}
	_ = c.writer.Write(NewResultResponse(req.ID, resultRaw))
	sink.run()
}

func (c *AgentSideConnection) safeCallMethod(ctx context.Context, req Request, pc *promptCancel) (result any, rpcErr *Error) {
	defer func() {
		if r := recover(); r != nil {
			result = nil
			rpcErr = InternalError(fmt.Sprintf("panic in %s handler: %v", req.Method, r))
		}
	}()
	return c.callMethod(ctx, req, pc)
}

func (c *AgentSideConnection) handleNotification(ctx context.Context, n Notification) {
	switch n.Method {
	case MethodSessionCancel:
		var p CancelNotification
		if err := json.Unmarshal(n.Params, &p); err != nil {
			return
		}
		_ = c.agent.Cancel(ctx, p)
	default:
		if IsExtensionMethod(n.Method) && c.extNotification != nil {
			c.extNotification(ctx, n.Method, n.Params)
		}
	}
}

func (c *AgentSideConnection) callMethod(ctx context.Context, req Request, pc *promptCancel) (any, *Error) {
	params := req.Params
	if len(params) == 0 {
		// Treat omitted params as {} so all-optional-params methods still unmarshal.
		params = []byte("{}")
	}
	switch req.Method {
	case MethodInitialize:
		var p InitializeRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.agent.Initialize(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodAuthenticate:
		var p AuthenticateRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.agent.Authenticate(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodLogout:
		var p LogoutRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.agent.Logout(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodSessionNew:
		var p NewSessionRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.agent.NewSession(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodSessionLoad:
		var p LoadSessionRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.agent.LoadSession(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodSessionResume:
		var p ResumeSessionRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.agent.ResumeSession(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodSessionClose:
		var p CloseSessionRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.agent.CloseSession(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodSessionDelete:
		var p DeleteSessionRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.agent.DeleteSession(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodSessionList:
		var p ListSessionsRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.agent.ListSessions(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodSessionSetMode:
		var p SetSessionModeRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.agent.SetSessionMode(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	// MethodSessionSetModel is the unstable Zed model-picker method
	// (session/set_model); an agent with no `models` state returns
	// MethodNotFound from SetSessionModel.
	case MethodSessionSetModel:
		var p SetSessionModelRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.agent.SetSessionModel(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodSessionSetConfigOption:
		var p SetSessionConfigOptionRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, InvalidParams(err.Error())
		}
		resp, err := c.agent.SetSessionConfigOption(ctx, p)
		if err != nil {
			return nil, AsError(err)
		}
		return resp, nil

	case MethodSessionPrompt:
		var p PromptRequest
		if err := json.Unmarshal(params, &p); err != nil {
			if pc != nil {
				c.unregisterPromptCancel(pc)
			}
			return nil, InvalidParams(err.Error())
		}
		// pc is nil only when params were unusable, already rejected above.
		promptCtx := ctx
		if pc != nil {
			promptCtx = pc.ctx
			defer c.unregisterPromptCancel(pc)
		}

		resp, err := c.agent.Prompt(promptCtx, p)
		if err != nil {
			// Spec: after session/cancel, prompt must resolve with the
			// cancelled stop reason, never a JSON-RPC error.
			if promptCtx.Err() == context.Canceled && errors.Is(err, context.Canceled) {
				return PromptResponse{StopReason: StopReasonCancelled}, nil
			}
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

func (c *AgentSideConnection) deliverResponse(resp Response) {
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

func (c *AgentSideConnection) call(ctx context.Context, method string, params any, result any) error {
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

func (c *AgentSideConnection) notify(method string, params any) error {
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

func (c *AgentSideConnection) SessionUpdate(n SessionNotification) error {
	return c.notify(MethodSessionUpdate, n)
}

func (c *AgentSideConnection) RequestPermission(ctx context.Context, req RequestPermissionRequest) (RequestPermissionResponse, error) {
	var resp RequestPermissionResponse
	if err := c.call(ctx, MethodSessionRequestPermission, req, &resp); err != nil {
		return RequestPermissionResponse{}, err
	}
	return resp, nil
}

func (c *AgentSideConnection) ReadTextFile(ctx context.Context, req ReadTextFileRequest) (ReadTextFileResponse, error) {
	var resp ReadTextFileResponse
	if err := c.call(ctx, MethodFSReadTextFile, req, &resp); err != nil {
		return ReadTextFileResponse{}, err
	}
	return resp, nil
}

func (c *AgentSideConnection) WriteTextFile(ctx context.Context, req WriteTextFileRequest) (WriteTextFileResponse, error) {
	var resp WriteTextFileResponse
	if err := c.call(ctx, MethodFSWriteTextFile, req, &resp); err != nil {
		return WriteTextFileResponse{}, err
	}
	return resp, nil
}

func (c *AgentSideConnection) CreateTerminal(ctx context.Context, req CreateTerminalRequest) (CreateTerminalResponse, error) {
	var resp CreateTerminalResponse
	if err := c.call(ctx, MethodTerminalCreate, req, &resp); err != nil {
		return CreateTerminalResponse{}, err
	}
	return resp, nil
}

func (c *AgentSideConnection) TerminalOutput(ctx context.Context, req TerminalOutputRequest) (TerminalOutputResponse, error) {
	var resp TerminalOutputResponse
	if err := c.call(ctx, MethodTerminalOutput, req, &resp); err != nil {
		return TerminalOutputResponse{}, err
	}
	return resp, nil
}

func (c *AgentSideConnection) WaitForTerminalExit(ctx context.Context, req WaitForTerminalExitRequest) (WaitForTerminalExitResponse, error) {
	var resp WaitForTerminalExitResponse
	if err := c.call(ctx, MethodTerminalWaitForExit, req, &resp); err != nil {
		return WaitForTerminalExitResponse{}, err
	}
	return resp, nil
}

func (c *AgentSideConnection) KillTerminal(ctx context.Context, req KillTerminalRequest) (KillTerminalResponse, error) {
	var resp KillTerminalResponse
	if err := c.call(ctx, MethodTerminalKill, req, &resp); err != nil {
		return KillTerminalResponse{}, err
	}
	return resp, nil
}

func (c *AgentSideConnection) ReleaseTerminal(ctx context.Context, req ReleaseTerminalRequest) (ReleaseTerminalResponse, error) {
	var resp ReleaseTerminalResponse
	if err := c.call(ctx, MethodTerminalRelease, req, &resp); err != nil {
		return ReleaseTerminalResponse{}, err
	}
	return resp, nil
}

// CallExtMethod sends an extension request to the client and returns its raw
// result. A canceled ctx aborts the wait and notifies the client.
func (c *AgentSideConnection) CallExtMethod(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
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
// (method must satisfy IsExtensionMethod) to the client.
func (c *AgentSideConnection) SendExtNotification(method string, params json.RawMessage) error {
	if !IsExtensionMethod(method) {
		return fmt.Errorf("libacp: %q is not an extension method (must start with %q)", method, ExtensionMethodPrefix)
	}
	var paramsAny any
	if len(params) > 0 {
		paramsAny = params
	}
	return c.notify(method, paramsAny)
}
