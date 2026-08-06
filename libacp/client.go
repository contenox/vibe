package libacp

import "context"

// Client is the editor-side counterpart to Agent (agent.go): the set of
// requests an agent may send to the client, plus the inbound session/update
// notification. ClientSideConnection (clientconn.go) is the mirror image of
// AgentSideConnection: it dispatches these methods for incoming JSON-RPC
// requests/notifications, and exposes the agent-bound methods (Initialize,
// session/new, session/prompt, ...) as outbound calls.
type Client interface {
	RequestPermission(ctx context.Context, req RequestPermissionRequest) (RequestPermissionResponse, error)
	ReadTextFile(ctx context.Context, req ReadTextFileRequest) (ReadTextFileResponse, error)
	WriteTextFile(ctx context.Context, req WriteTextFileRequest) (WriteTextFileResponse, error)
	CreateTerminal(ctx context.Context, req CreateTerminalRequest) (CreateTerminalResponse, error)
	TerminalOutput(ctx context.Context, req TerminalOutputRequest) (TerminalOutputResponse, error)
	WaitForTerminalExit(ctx context.Context, req WaitForTerminalExitRequest) (WaitForTerminalExitResponse, error)
	KillTerminal(ctx context.Context, req KillTerminalRequest) (KillTerminalResponse, error)
	ReleaseTerminal(ctx context.Context, req ReleaseTerminalRequest) (ReleaseTerminalResponse, error)
	// SessionUpdate handles an inbound "session/update" notification. It has no
	// response on the wire (JSON-RPC notifications never do); the returned
	// error is reported to the implementation only, e.g. for logging.
	SessionUpdate(ctx context.Context, n SessionNotification) error
}

type ClientFactory func(conn *ClientSideConnection) Client

// UnimplementedClient rejects every request-shaped Client method with
// MethodNotFound and treats SessionUpdate as a no-op, mirroring
// UnimplementedAgent (agent.go). Embed it to implement only the methods a
// particular client cares about.
type UnimplementedClient struct{}

func (UnimplementedClient) RequestPermission(context.Context, RequestPermissionRequest) (RequestPermissionResponse, error) {
	return RequestPermissionResponse{}, MethodNotFound(MethodSessionRequestPermission)
}
func (UnimplementedClient) ReadTextFile(context.Context, ReadTextFileRequest) (ReadTextFileResponse, error) {
	return ReadTextFileResponse{}, MethodNotFound(MethodFSReadTextFile)
}
func (UnimplementedClient) WriteTextFile(context.Context, WriteTextFileRequest) (WriteTextFileResponse, error) {
	return WriteTextFileResponse{}, MethodNotFound(MethodFSWriteTextFile)
}
func (UnimplementedClient) CreateTerminal(context.Context, CreateTerminalRequest) (CreateTerminalResponse, error) {
	return CreateTerminalResponse{}, MethodNotFound(MethodTerminalCreate)
}
func (UnimplementedClient) TerminalOutput(context.Context, TerminalOutputRequest) (TerminalOutputResponse, error) {
	return TerminalOutputResponse{}, MethodNotFound(MethodTerminalOutput)
}
func (UnimplementedClient) WaitForTerminalExit(context.Context, WaitForTerminalExitRequest) (WaitForTerminalExitResponse, error) {
	return WaitForTerminalExitResponse{}, MethodNotFound(MethodTerminalWaitForExit)
}
func (UnimplementedClient) KillTerminal(context.Context, KillTerminalRequest) (KillTerminalResponse, error) {
	return KillTerminalResponse{}, MethodNotFound(MethodTerminalKill)
}
func (UnimplementedClient) ReleaseTerminal(context.Context, ReleaseTerminalRequest) (ReleaseTerminalResponse, error) {
	return ReleaseTerminalResponse{}, MethodNotFound(MethodTerminalRelease)
}
func (UnimplementedClient) SessionUpdate(context.Context, SessionNotification) error {
	return nil
}
