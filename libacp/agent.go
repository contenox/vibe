package libacp

import "context"

type Agent interface {
	Initialize(ctx context.Context, req InitializeRequest) (InitializeResponse, error)
	Authenticate(ctx context.Context, req AuthenticateRequest) (AuthenticateResponse, error)
	Logout(ctx context.Context, req LogoutRequest) (LogoutResponse, error)
	NewSession(ctx context.Context, req NewSessionRequest) (NewSessionResponse, error)
	LoadSession(ctx context.Context, req LoadSessionRequest) (LoadSessionResponse, error)
	ResumeSession(ctx context.Context, req ResumeSessionRequest) (ResumeSessionResponse, error)
	CloseSession(ctx context.Context, req CloseSessionRequest) (CloseSessionResponse, error)
	DeleteSession(ctx context.Context, req DeleteSessionRequest) (DeleteSessionResponse, error)
	ListSessions(ctx context.Context, req ListSessionsRequest) (ListSessionsResponse, error)
	SetSessionMode(ctx context.Context, req SetSessionModeRequest) (SetSessionModeResponse, error)
	// SetSessionModel switches a session's active model. This is the UNSTABLE Zed
	// model-picker surface (session/set_model, see MethodSessionSetModel); an agent
	// that advertises no `models` state returns MethodNotFound, matching the
	// experimental method's optional-capability contract.
	SetSessionModel(ctx context.Context, req SetSessionModelRequest) (SetSessionModelResponse, error)
	SetSessionConfigOption(ctx context.Context, req SetSessionConfigOptionRequest) (SetSessionConfigOptionResponse, error)
	Prompt(ctx context.Context, req PromptRequest) (PromptResponse, error)
	Cancel(ctx context.Context, req CancelNotification) error
}

type AgentFactory func(conn *AgentSideConnection) Agent

type UnimplementedAgent struct{}

func (UnimplementedAgent) Initialize(context.Context, InitializeRequest) (InitializeResponse, error) {
	return InitializeResponse{}, MethodNotFound(MethodInitialize)
}
func (UnimplementedAgent) Authenticate(context.Context, AuthenticateRequest) (AuthenticateResponse, error) {
	return AuthenticateResponse{}, MethodNotFound(MethodAuthenticate)
}
func (UnimplementedAgent) Logout(context.Context, LogoutRequest) (LogoutResponse, error) {
	return LogoutResponse{}, MethodNotFound(MethodLogout)
}
func (UnimplementedAgent) NewSession(context.Context, NewSessionRequest) (NewSessionResponse, error) {
	return NewSessionResponse{}, MethodNotFound(MethodSessionNew)
}
func (UnimplementedAgent) LoadSession(context.Context, LoadSessionRequest) (LoadSessionResponse, error) {
	return LoadSessionResponse{}, MethodNotFound(MethodSessionLoad)
}
func (UnimplementedAgent) ResumeSession(context.Context, ResumeSessionRequest) (ResumeSessionResponse, error) {
	return ResumeSessionResponse{}, MethodNotFound(MethodSessionResume)
}
func (UnimplementedAgent) CloseSession(context.Context, CloseSessionRequest) (CloseSessionResponse, error) {
	return CloseSessionResponse{}, MethodNotFound(MethodSessionClose)
}
func (UnimplementedAgent) DeleteSession(context.Context, DeleteSessionRequest) (DeleteSessionResponse, error) {
	return DeleteSessionResponse{}, MethodNotFound(MethodSessionDelete)
}
func (UnimplementedAgent) ListSessions(context.Context, ListSessionsRequest) (ListSessionsResponse, error) {
	return ListSessionsResponse{}, MethodNotFound(MethodSessionList)
}
func (UnimplementedAgent) SetSessionMode(context.Context, SetSessionModeRequest) (SetSessionModeResponse, error) {
	return SetSessionModeResponse{}, MethodNotFound(MethodSessionSetMode)
}
func (UnimplementedAgent) SetSessionModel(context.Context, SetSessionModelRequest) (SetSessionModelResponse, error) {
	return SetSessionModelResponse{}, MethodNotFound(MethodSessionSetModel)
}
func (UnimplementedAgent) SetSessionConfigOption(context.Context, SetSessionConfigOptionRequest) (SetSessionConfigOptionResponse, error) {
	return SetSessionConfigOptionResponse{}, MethodNotFound(MethodSessionSetConfigOption)
}
func (UnimplementedAgent) Prompt(context.Context, PromptRequest) (PromptResponse, error) {
	return PromptResponse{}, MethodNotFound(MethodSessionPrompt)
}
func (UnimplementedAgent) Cancel(context.Context, CancelNotification) error {
	return nil
}
