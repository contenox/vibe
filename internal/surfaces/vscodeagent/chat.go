package vscodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/agentsmd"
	"github.com/contenox/contenox/internal/services/chatservice"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/messagestore"
	"github.com/contenox/contenox/internal/services/sessionservice"
	"github.com/google/uuid"
)

const maxSessionTitleLength = 64

type turnInfo struct {
	SessionID string
	TurnID    string
}

type sessionCreateParams struct {
	Name string `json:"name,omitempty"`
}

type sessionLoadParams struct {
	SessionID string `json:"sessionId,omitempty"`
	Name      string `json:"name,omitempty"`
}

type sessionReadParams struct {
	SessionID string `json:"sessionId,omitempty"`
	Name      string `json:"name,omitempty"`
}

type sessionDeleteParams struct {
	SessionID string `json:"sessionId,omitempty"`
	Name      string `json:"name,omitempty"`
}

type sessionListResult struct {
	Sessions []sessionInfo `json:"sessions"`
}

type sessionResult struct {
	Session  sessionInfo   `json:"session"`
	Messages []messageInfo `json:"messages"`
}

type sessionDeleteResult struct {
	Deleted   bool   `json:"deleted"`
	SessionID string `json:"sessionId,omitempty"`
	WasActive bool   `json:"wasActive"`
}

type sessionInfo struct {
	ID           string `json:"id"`
	Name         string `json:"name,omitempty"`
	MessageCount int    `json:"messageCount"`
	IsActive     bool   `json:"isActive"`
	UpdatedAt    string `json:"updatedAt,omitempty"`
}

type messageInfo struct {
	ID         string         `json:"id,omitempty"`
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	Thinking   string         `json:"thinking,omitempty"`
	ToolCallID string         `json:"toolCallId,omitempty"`
	ToolCalls  []toolCallInfo `json:"toolCalls,omitempty"`
	Timestamp  string         `json:"timestamp,omitempty"`
}

type toolCallInfo struct {
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
	RawArgs   string         `json:"rawArgs,omitempty"`
}

type chatSendParams struct {
	SessionID string            `json:"sessionId,omitempty"`
	Input     string            `json:"input"`
	Context   []editorContext   `json:"context,omitempty"`
	Vars      map[string]string `json:"vars,omitempty"`
}

type editorContext struct {
	Kind       string `json:"kind"`
	URI        string `json:"uri,omitempty"`
	LanguageID string `json:"languageId,omitempty"`
	Content    string `json:"content"`
}

type chatSendResult struct {
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`
	// Title may be the (possibly newly derived) session name after ensureMeaningfulSessionTitle.
	Title string `json:"title,omitempty"`
}

type chatCancelParams struct {
	TurnID string `json:"turnId"`
}

type chatCancelResult struct {
	Cancelled bool `json:"cancelled"`
}

type chatDeltaEvent struct {
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`
	Content   string `json:"content,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
}

type chatLifecycleEvent struct {
	SessionID  string        `json:"sessionId"`
	TurnID     string        `json:"turnId"`
	StopReason string        `json:"stopReason,omitempty"`
	Error      string        `json:"error,omitempty"`
	Messages   []messageInfo `json:"messages,omitempty"`
}

type toolCallEvent struct {
	SessionID  string         `json:"sessionId"`
	TurnID     string         `json:"turnId"`
	ToolCallID string         `json:"toolCallId,omitempty"`
	Title      string         `json:"title,omitempty"`
	Status     string         `json:"status"`
	ToolName   string         `json:"toolName,omitempty"`
	TaskID     string         `json:"taskId,omitempty"`
	Input      map[string]any `json:"input,omitempty"`
	Output     string         `json:"output,omitempty"`
	Error      string         `json:"error,omitempty"`
	DiffPath   string         `json:"diffPath,omitempty"`
	DiffOld    string         `json:"diffOld,omitempty"`
	DiffNew    string         `json:"diffNew,omitempty"`
}

type hitlDecisionEvent struct {
	SessionID         string `json:"sessionId"`
	TurnID            string `json:"turnId"`
	ToolsName         string `json:"toolsName,omitempty"`
	ToolName          string `json:"toolName,omitempty"`
	Action            string `json:"action"`
	Reason            string `json:"reason,omitempty"`
	PolicyName        string `json:"policyName,omitempty"`
	PolicyPath        string `json:"policyPath,omitempty"`
	ArgsSummary       string `json:"argsSummary,omitempty"`
	MatchedRule       *int   `json:"matchedRule,omitempty"`
	TimeoutS          int    `json:"timeoutS,omitempty"`
	ApprovalRequested bool   `json:"approvalRequested"`
}

type contextUsageEvent struct {
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId,omitempty"`
	Used      int    `json:"used"`
	Size      int    `json:"size"`
}

type autocompleteParams struct {
	Prefix     string `json:"prefix"`
	Suffix     string `json:"suffix,omitempty"`
	LanguageID string `json:"languageId,omitempty"`
	URI        string `json:"uri,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	MaxTokens  int    `json:"maxTokens,omitempty"`
}

type autocompleteResult struct {
	Completion string `json:"completion"`
}

func (s *Server) sessionSvc() sessionservice.Service {
	return sessionservice.New(s.db, s.workspaceID, s.tracker())
}

func (s *Server) chatMgr() *chatservice.Manager {
	return chatservice.NewManager(s.workspaceID)
}

func (s *Server) tracker() libtracker.ActivityTracker {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	if s.runtime != nil && s.runtime.Engine != nil && s.runtime.Engine.Tracker != nil {
		return s.runtime.Engine.Tracker
	}
	return libtracker.NoopTracker{}
}

type hitlPolicyRef struct {
	Name string
	Path string
}

func (s *Server) activeHITLPolicy(ctx context.Context) hitlPolicyRef {
	name := strings.TrimSpace(clikv.ReadHITLPolicy(ctx, s.store))
	if name == "" {
		name = defaultHITLPolicyName
	}
	return hitlPolicyRef{Name: name, Path: s.hitlPolicyPath(name)}
}

func (s *Server) hitlPolicyPath(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || s.stateDir == "" {
		return ""
	}
	return filepath.Join(s.stateDir, name)
}

type hitlPolicyInfo struct {
	Name   string `json:"name"`
	Path   string `json:"path,omitempty"`
	Active bool   `json:"active,omitempty"`
}

type listHitlPoliciesResult struct {
	Policies         []string         `json:"policies"`
	PolicyDir        string           `json:"policyDir,omitempty"`
	ActivePolicyName string           `json:"activePolicyName,omitempty"`
	ActivePolicyPath string           `json:"activePolicyPath,omitempty"`
	PolicyFiles      []hitlPolicyInfo `json:"policyFiles,omitempty"`
}

func (s *Server) listHitlPolicies(ctx context.Context) listHitlPoliciesResult {
	active := s.activeHITLPolicy(ctx)
	policies := append([]string(nil), s.policyNames...)
	seen := make(map[string]struct{}, len(policies)+1)
	files := make([]hitlPolicyInfo, 0, len(policies)+1)
	for _, name := range policies {
		seen[name] = struct{}{}
		files = append(files, hitlPolicyInfo{
			Name:   name,
			Path:   s.hitlPolicyPath(name),
			Active: name == active.Name,
		})
	}
	if active.Name != "" {
		if _, ok := seen[active.Name]; !ok {
			policies = append(policies, active.Name)
			files = append(files, hitlPolicyInfo{
				Name:   active.Name,
				Path:   active.Path,
				Active: true,
			})
		}
	}
	return listHitlPoliciesResult{
		Policies:         policies,
		PolicyDir:        s.stateDir,
		ActivePolicyName: active.Name,
		ActivePolicyPath: active.Path,
		PolicyFiles:      files,
	}
}

func (s *Server) sessionCreate(ctx context.Context, params sessionCreateParams) (sessionResult, error) {
	title := sessionTitleFromInput(params.Name, "New Contenox session")
	title, err := s.uniqueSessionTitle(ctx, title, "")
	if err != nil {
		return sessionResult{}, err
	}
	id, err := s.sessionSvc().New(ctx, Identity, title)
	if err != nil {
		return sessionResult{}, err
	}
	return s.sessionByID(ctx, id)
}

func (s *Server) sessionList(ctx context.Context) (sessionListResult, error) {
	sessions, err := s.sessionSvc().List(ctx, Identity)
	if err != nil {
		return sessionListResult{}, err
	}
	out := make([]sessionInfo, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, sessionInfoFromService(sess))
	}
	return sessionListResult{Sessions: out}, nil
}

func (s *Server) sessionLoad(ctx context.Context, params sessionLoadParams) (sessionResult, error) {
	session, err := s.resolveSession(ctx, params.SessionID, params.Name)
	if err != nil {
		return sessionResult{}, err
	}
	if err := s.sessionSvc().SetActiveID(ctx, session.ID); err != nil {
		return sessionResult{}, err
	}
	return s.sessionByID(ctx, session.ID)
}

func (s *Server) sessionRead(ctx context.Context, params sessionReadParams) (sessionResult, error) {
	session, err := s.resolveSession(ctx, params.SessionID, params.Name)
	if err != nil {
		return sessionResult{}, err
	}
	return s.sessionByID(ctx, session.ID)
}

func (s *Server) sessionDelete(ctx context.Context, params sessionDeleteParams) (sessionDeleteResult, error) {
	session, err := s.resolveSession(ctx, params.SessionID, params.Name)
	if err != nil {
		return sessionDeleteResult{}, err
	}
	wasActive, err := s.sessionSvc().Delete(ctx, Identity, session.Name)
	if err != nil {
		return sessionDeleteResult{}, err
	}
	s.clearSessionConfig(session.ID)
	return sessionDeleteResult{Deleted: true, SessionID: session.ID, WasActive: wasActive}, nil
}

func (s *Server) chatSend(ctx context.Context, params chatSendParams) (chatSendResult, error) {
	input := strings.TrimSpace(params.Input)
	if input == "" {
		return chatSendResult{}, fmt.Errorf("input is required")
	}
	sessionID := strings.TrimSpace(params.SessionID)
	if sessionID == "" {
		id, err := s.sessionSvc().EnsureDefault(ctx, Identity)
		if err != nil {
			return chatSendResult{}, err
		}
		sessionID = id
	} else if _, err := s.resolveSession(ctx, sessionID, ""); err != nil {
		return chatSendResult{}, err
	}
	if err := s.ensureMeaningfulSessionTitle(ctx, sessionID, input); err != nil {
		return chatSendResult{}, err
	}

	// Fetch the (possibly updated) title so the client can reflect a nice name immediately.
	var derivedTitle string
	if sess, err := s.resolveSession(ctx, sessionID, ""); err == nil {
		derivedTitle = sess.Name
	}

	turnID := uuid.NewString()
	execCtx := libtracker.WithNewRequestID(context.Background())
	execCtx, cancel := context.WithCancel(execCtx)
	reqID, _ := execCtx.Value(libtracker.ContextKeyRequestID).(string)
	s.registerTurn(reqID, turnID, sessionID, cancel)

	fullInput := inputWithEditorContext(input, params.Context)
	vars := s.templateVars(ctx, sessionID)
	for k, v := range params.Vars {
		if strings.TrimSpace(k) != "" {
			vars[k] = v
		}
	}

	if name, args, ok := parseSlashCommand(input); ok {
		go s.runCommandTurn(execCtx, sessionID, turnID, name, args)
		return chatSendResult{SessionID: sessionID, TurnID: turnID, Title: derivedTitle}, nil
	}

	go s.runChatTurn(execCtx, sessionID, turnID, fullInput, vars)
	return chatSendResult{SessionID: sessionID, TurnID: turnID, Title: derivedTitle}, nil
}

func (s *Server) runChatTurn(ctx context.Context, sessionID, turnID, input string, vars map[string]string) {
	reqID, _ := ctx.Value(libtracker.ContextKeyRequestID).(string)
	defer s.unregisterTurn(reqID, turnID)

	_ = s.notify("chatStarted", chatLifecycleEvent{SessionID: sessionID, TurnID: turnID})
	rt, err := s.ensureRuntime(ctx)
	if err != nil {
		s.notifyChatFailure(ctx, sessionID, turnID, err)
		return
	}
	if rt.Agent == nil || rt.Chain == nil {
		s.notifyChatFailure(ctx, sessionID, turnID, fmt.Errorf("runtime is missing agent or chain"))
		return
	}

	agentsMD, agentsMDSource := s.loadAgentsMD()
	resp, err := rt.Agent.Prompt(ctx, agentservice.PromptRequest{
		SessionID:      sessionID,
		Input:          input,
		Chain:          rt.Chain,
		TemplateVars:   vars,
		AgentsMD:       agentsMD,
		AgentsMDSource: agentsMDSource,
	})
	if err != nil {
		if resp != nil && resp.StopReason == agentservice.StopCancelled {
			_ = s.notify("chatCancelled", chatLifecycleEvent{SessionID: sessionID, TurnID: turnID, StopReason: string(resp.StopReason)})
			return
		}
		s.notifyChatFailure(ctx, sessionID, turnID, err)
		return
	}
	messages, loadErr := s.messagesForSession(context.Background(), sessionID)
	if loadErr != nil {
		s.notifyChatFailure(ctx, sessionID, turnID, loadErr)
		return
	}
	_ = s.notify("chatCompleted", chatLifecycleEvent{
		SessionID:  sessionID,
		TurnID:     turnID,
		StopReason: string(resp.StopReason),
		Messages:   messages,
	})
}

func (s *Server) loadAgentsMD() (string, string) {
	content, path, ok := agentsmd.Load(s.workspaceRoot())
	if !ok {
		return "", ""
	}
	return content, path
}

func (s *Server) workspaceRoot() string {
	if strings.TrimSpace(s.workspaceCWD) != "" {
		return s.workspaceCWD
	}
	return "."
}

func (s *Server) notifyChatFailure(ctx context.Context, sessionID, turnID string, err error) {
	event := chatLifecycleEvent{SessionID: sessionID, TurnID: turnID, Error: err.Error()}
	if messages, loadErr := s.messagesForSession(context.Background(), sessionID); loadErr == nil {
		event.Messages = messages
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		_ = s.notify("chatCancelled", event)
		return
	}
	_ = s.notify("chatFailed", event)
}

func (s *Server) chatCancel(params chatCancelParams) chatCancelResult {
	turnID := strings.TrimSpace(params.TurnID)
	if turnID == "" {
		return chatCancelResult{}
	}
	s.turnMu.Lock()
	cancel := s.turns[turnID]
	s.turnMu.Unlock()
	if cancel == nil {
		return chatCancelResult{}
	}
	cancel()
	return chatCancelResult{Cancelled: true}
}

func (s *Server) autocomplete(ctx context.Context, params autocompleteParams) (autocompleteResult, error) {
	if strings.TrimSpace(params.Prefix) == "" && strings.TrimSpace(params.Suffix) == "" {
		return autocompleteResult{}, fmt.Errorf("prefix or suffix is required")
	}
	rt, err := s.ensureRuntime(ctx)
	if err != nil {
		return autocompleteResult{}, err
	}
	if rt.Agent == nil || rt.FIMChain == nil {
		return autocompleteResult{}, fmt.Errorf("autocomplete chain is unavailable")
	}
	maxTokens := params.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 128
	}
	vars, autoRouted := s.autocompleteTemplateVars(ctx, params, maxTokens)
	prompt := "<fim_prefix>" + params.Prefix + "<fim_suffix>" + params.Suffix + "<fim_middle>"
	resp, err := s.promptAutocomplete(ctx, rt, prompt, vars)
	if err != nil && autoRouted {
		resp, err = s.promptAutocomplete(ctx, rt, prompt, s.defaultAutocompleteTemplateVars(ctx, maxTokens))
	}
	if err != nil {
		return autocompleteResult{}, err
	}
	return autocompleteResult{Completion: strings.TrimRightFunc(extractAssistantText(resp.Output), unicode.IsSpace)}, nil
}

func (s *Server) promptAutocomplete(
	ctx context.Context,
	rt *Runtime,
	prompt string,
	vars map[string]string,
) (*agentservice.PromptResponse, error) {
	execCtx, cancel := context.WithTimeout(libtracker.WithNewRequestID(ctx), 20*time.Second)
	defer cancel()
	return rt.Agent.Prompt(execCtx, agentservice.PromptRequest{
		Input:        prompt,
		Chain:        rt.FIMChain,
		TemplateVars: vars,
	})
}

func (s *Server) autocompleteTemplateVars(
	ctx context.Context,
	params autocompleteParams,
	maxTokens int,
) (map[string]string, bool) {
	vars := s.defaultAutocompleteTemplateVars(ctx, maxTokens)
	if provider := strings.TrimSpace(params.Provider); provider != "" {
		vars["provider"] = provider
	}
	if model := strings.TrimSpace(params.Model); model != "" {
		vars["model"] = model
	}
	if strings.TrimSpace(params.Provider) != "" || strings.TrimSpace(params.Model) != "" {
		return vars, false
	}
	if autocompleteProvider, autocompleteModel, ok := s.configuredAutocompleteModel(ctx); ok {
		if autocompleteProvider != "" {
			vars["provider"] = autocompleteProvider
		}
		if autocompleteModel != "" {
			vars["model"] = autocompleteModel
		}
		return vars, false
	}
	if provider, model, ok := s.preferredAutocompleteModel(ctx); ok {
		vars["provider"] = provider
		vars["model"] = model
		return vars, true
	}
	return vars, false
}

func (s *Server) defaultAutocompleteTemplateVars(ctx context.Context, maxTokens int) map[string]string {
	vars := s.templateVars(ctx, "")
	vars["max_tokens"] = fmt.Sprintf("%d", maxTokens)
	return vars
}

func (s *Server) configuredAutocompleteModel(ctx context.Context) (string, string, bool) {
	cfg := s.getConfig(ctx)
	provider := strings.TrimSpace(cfg.DefaultAutocompleteProvider)
	model := strings.TrimSpace(cfg.DefaultAutocompleteModel)
	return provider, model, provider != "" || model != ""
}

func (s *Server) preferredAutocompleteModel(ctx context.Context) (string, string, bool) {
	cfg := s.getConfig(ctx)
	if isCodeAutocompleteModel(cfg.DefaultProvider, cfg.DefaultModel) {
		return cfg.DefaultProvider, cfg.DefaultModel, true
	}
	backends, err := s.store.ListAllBackends(ctx)
	if err != nil {
		return "", "", false
	}
	for _, backend := range backends {
		if backend != nil && strings.EqualFold(strings.TrimSpace(backend.Type), "mistral") {
			return "mistral", "mistral-code-fim-latest", true
		}
	}
	return "", "", false
}

func isCodeAutocompleteModel(provider, model string) bool {
	if !strings.EqualFold(strings.TrimSpace(provider), "mistral") {
		return false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(model, "fim") || strings.Contains(model, "codestral") || strings.Contains(model, "code")
}

func (s *Server) ensureRuntime(ctx context.Context) (*Runtime, error) {
	if s.buildRuntime == nil {
		return nil, ErrSetupRequired
	}
	s.runtimeMu.Lock()
	if s.runtime != nil {
		rt := s.runtime
		s.runtimeMu.Unlock()
		return rt, nil
	}
	s.runtimeMu.Unlock()

	rt, err := s.buildRuntime(ctx, RuntimeHooks{
		AskApproval: s.approvals.AskApproval,
		EventSink:   s.events,
	})
	if err != nil {
		return nil, err
	}
	if rt == nil {
		return nil, ErrSetupRequired
	}
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	if s.runtime != nil {
		rt.stop()
		return s.runtime, nil
	}
	s.runtime = rt
	return rt, nil
}

func (s *Server) resetRuntime() {
	s.runtimeMu.Lock()
	rt := s.runtime
	s.runtime = nil
	s.runtimeMu.Unlock()
	rt.stop()
}

func (s *Server) closeRuntime() {
	s.resetRuntime()
}

func (s *Server) registerTurn(requestID, turnID, sessionID string, cancel context.CancelFunc) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	s.turns[turnID] = cancel
	if requestID != "" {
		s.requestTo[requestID] = turnInfo{SessionID: sessionID, TurnID: turnID}
	}
}

func (s *Server) unregisterTurn(requestID, turnID string) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	delete(s.turns, turnID)
	if requestID != "" {
		delete(s.requestTo, requestID)
	}
}

func (s *Server) cancelAllTurns() {
	s.turnMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.turns))
	for _, cancel := range s.turns {
		cancels = append(cancels, cancel)
	}
	s.turnMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (s *Server) registerRPCRequest(id string, cancel context.CancelFunc) {
	s.rpcMu.Lock()
	defer s.rpcMu.Unlock()
	s.rpcCancels[id] = cancel
}

func (s *Server) unregisterRPCRequest(id string) {
	s.rpcMu.Lock()
	cancel := s.rpcCancels[id]
	delete(s.rpcCancels, id)
	s.rpcMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Server) cancelAllRPCRequests() {
	s.rpcMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.rpcCancels))
	for _, cancel := range s.rpcCancels {
		cancels = append(cancels, cancel)
	}
	s.rpcMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (s *Server) turnByRequestID(requestID string) (turnInfo, bool) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	turn, ok := s.requestTo[requestID]
	return turn, ok
}

func (s *Server) templateVars(ctx context.Context, sessionID string) map[string]string {
	cfg := s.getConfig(ctx)
	vars := map[string]string{
		"model":    cfg.DefaultModel,
		"provider": cfg.DefaultProvider,
		"think":    s.effectiveThink(ctx, sessionID),
	}
	// The seeded chains reference {{var:alt_model|var:default_model}} (and the
	// provider equivalent), so default_model/default_provider must be set
	// whenever a model is known, matching the CLI chat and ACP paths.
	if cfg.DefaultModel != "" {
		vars["default_model"] = cfg.DefaultModel
	}
	if cfg.DefaultProvider != "" {
		vars["default_provider"] = cfg.DefaultProvider
	}
	if cfg.DefaultAltModel != "" {
		vars["alt_model"] = cfg.DefaultAltModel
	}
	if cfg.DefaultAltProvider != "" {
		vars["alt_provider"] = cfg.DefaultAltProvider
	}
	if cfg.DefaultMaxTokens != "" {
		vars["max_tokens"] = cfg.DefaultMaxTokens
	}
	if cfg.DefaultAutocompleteModel != "" {
		vars["autocomplete_model"] = cfg.DefaultAutocompleteModel
	}
	if cfg.DefaultAutocompleteProvider != "" {
		vars["autocomplete_provider"] = cfg.DefaultAutocompleteProvider
	}
	return vars
}

func (s *Server) effectiveThink(ctx context.Context, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID != "" {
		s.sessionCfgMu.Lock()
		think := s.sessionThink[sessionID]
		s.sessionCfgMu.Unlock()
		if strings.TrimSpace(think) != "" {
			return think
		}
	}
	cfg := s.getConfig(ctx)
	if strings.TrimSpace(cfg.DefaultThink) != "" {
		return cfg.DefaultThink
	}
	return "high"
}

func (s *Server) setSessionThink(sessionID, think string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	s.sessionCfgMu.Lock()
	s.sessionThink[sessionID] = think
	s.sessionCfgMu.Unlock()
}

func (s *Server) clearSessionConfig(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	s.sessionCfgMu.Lock()
	delete(s.sessionThink, sessionID)
	s.sessionCfgMu.Unlock()
}

func (s *Server) resolveSession(ctx context.Context, sessionID, name string) (sessionInfo, error) {
	sessionID = strings.TrimSpace(sessionID)
	name = strings.TrimSpace(name)
	sessions, err := s.sessionSvc().List(ctx, Identity)
	if err != nil {
		return sessionInfo{}, err
	}
	for _, sess := range sessions {
		if (sessionID != "" && sess.ID == sessionID) || (name != "" && sess.Name == name) {
			return sessionInfoFromService(sess), nil
		}
	}
	switch {
	case sessionID != "":
		return sessionInfo{}, fmt.Errorf("session %q not found", sessionID)
	case name != "":
		return sessionInfo{}, fmt.Errorf("session %q not found", name)
	default:
		return sessionInfo{}, fmt.Errorf("sessionId or name is required")
	}
}

func (s *Server) sessionByID(ctx context.Context, id string) (sessionResult, error) {
	session, err := s.resolveSession(ctx, id, "")
	if err != nil {
		return sessionResult{}, err
	}
	messages, err := s.messagesForSession(ctx, id)
	if err != nil {
		return sessionResult{}, err
	}
	return sessionResult{Session: session, Messages: messages}, nil
}

func (s *Server) ensureMeaningfulSessionTitle(ctx context.Context, sessionID, input string) error {
	title := sessionTitleFromInput(input, "")
	if title == "" {
		return nil
	}
	session, err := s.resolveSession(ctx, sessionID, "")
	if err != nil {
		return err
	}
	if !isGenericSessionTitle(session.Name) {
		return nil
	}
	title, err = s.uniqueSessionTitle(ctx, title, sessionID)
	if err != nil {
		return err
	}
	if title == "" || title == session.Name {
		return nil
	}
	return messagestore.New(s.db.WithoutTransaction(), s.workspaceID).RenameSession(ctx, sessionID, title)
}

func (s *Server) uniqueSessionTitle(ctx context.Context, title, currentSessionID string) (string, error) {
	title = truncateSessionTitle(strings.TrimSpace(title), "")
	if title == "" {
		title = "New Contenox session"
	}
	sessions, err := s.sessionSvc().List(ctx, Identity)
	if err != nil {
		return "", err
	}
	taken := map[string]struct{}{}
	for _, sess := range sessions {
		if sess == nil || sess.ID == currentSessionID || strings.TrimSpace(sess.Name) == "" {
			continue
		}
		taken[sess.Name] = struct{}{}
	}
	if _, ok := taken[title]; !ok {
		return title, nil
	}
	for i := 2; i < 1000; i++ {
		suffix := fmt.Sprintf(" (%d)", i)
		candidate := truncateSessionTitle(title, suffix)
		if _, ok := taken[candidate]; !ok {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("failed to allocate unique session title for %q", title)
}

func (s *Server) messagesForSession(ctx context.Context, sessionID string) ([]messageInfo, error) {
	messages, err := s.chatMgr().ListMessages(ctx, s.db.WithoutTransaction(), sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]messageInfo, 0, len(messages))
	for _, msg := range messages {
		out = append(out, messageInfoFromTask(msg))
	}
	return out, nil
}

func sessionInfoFromService(sess *sessionservice.SessionInfo) sessionInfo {
	if sess == nil {
		return sessionInfo{}
	}
	info := sessionInfo{
		ID:           sess.ID,
		Name:         sess.Name,
		MessageCount: sess.MessageCount,
		IsActive:     sess.IsActive,
	}
	if !sess.UpdatedAt.IsZero() {
		info.UpdatedAt = sess.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return info
}

func messageInfoFromTask(msg taskengine.Message) messageInfo {
	info := messageInfo{
		ID:         msg.ID,
		Role:       msg.Role,
		Content:    msg.Content,
		Thinking:   msg.Thinking,
		ToolCallID: msg.ToolCallID,
	}
	if !msg.Timestamp.IsZero() {
		info.Timestamp = msg.Timestamp.Format(time.RFC3339Nano)
	}
	for _, call := range msg.CallTools {
		tc := toolCallInfo{
			ID:      call.ID,
			Name:    call.Function.Name,
			RawArgs: call.Function.Arguments,
		}
		if call.Function.Arguments != "" && json.Valid([]byte(call.Function.Arguments)) {
			var args map[string]any
			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err == nil {
				tc.Arguments = args
			}
		}
		info.ToolCalls = append(info.ToolCalls, tc)
	}
	return info
}

func inputWithEditorContext(input string, ctx []editorContext) string {
	if len(ctx) == 0 {
		return input
	}
	var b strings.Builder
	b.WriteString(input)
	b.WriteString("\n\nEditor context:\n")
	for _, item := range ctx {
		if strings.TrimSpace(item.Content) == "" {
			continue
		}
		b.WriteString("\n--- ")
		if item.Kind != "" {
			b.WriteString(item.Kind)
		} else {
			b.WriteString("context")
		}
		if item.URI != "" {
			b.WriteString(" ")
			b.WriteString(item.URI)
		}
		if item.LanguageID != "" {
			b.WriteString(" (")
			b.WriteString(item.LanguageID)
			b.WriteString(")")
		}
		b.WriteString(" ---\n")
		b.WriteString(item.Content)
		b.WriteString("\n")
	}
	return b.String()
}

func sessionTitleFromInput(input, fallback string) string {
	title := normalizeSessionTitle(input)
	title = stripCaseInsensitivePrefix(title, "@contenox ")
	if strings.HasPrefix(title, "/") {
		if name, args, ok := parseSlashCommand(title); ok {
			title = titleFromSlashCommand(name, args)
		}
	}
	title = truncateSessionTitle(normalizeSessionTitle(title), "")
	if title == "" || isGenericSessionTitle(title) {
		return truncateSessionTitle(normalizeSessionTitle(fallback), "")
	}
	return title
}

func titleFromSlashCommand(name, args string) string {
	args = normalizeSessionTitle(args)
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "doctor":
		return "Check Contenox setup"
	case "help":
		return "Contenox help"
	case "compact":
		if args != "" {
			return "Compact history: " + args
		}
		return "Compact session history"
	case "model":
		if args != "" {
			return "Model: " + args
		}
		return "Show model"
	case "provider":
		if args != "" {
			return "Provider: " + args
		}
		return "Show provider"
	case "autocomplete-model":
		if args != "" {
			return "Autocomplete model: " + args
		}
		return "Show autocomplete model"
	case "autocomplete-provider":
		if args != "" {
			return "Autocomplete provider: " + args
		}
		return "Show autocomplete provider"
	case "max-tokens":
		if args != "" {
			return "Max tokens: " + args
		}
		return "Show max tokens"
	case "think":
		if args != "" {
			return "Think: " + args
		}
		return "Show think level"
	case "capability":
		if args != "" {
			return "Capability: " + args
		}
		return "Show model capability"
	case "policy":
		if args != "" {
			return "HITL policy: " + args
		}
		return "Show HITL policy"
	case "websearch":
		if args != "" {
			return "Web search: " + args
		}
		return "Web search"
	default:
		if args != "" {
			return args
		}
		return name
	}
}

func normalizeSessionTitle(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "\"'")
	return strings.Join(strings.Fields(value), " ")
}

func truncateSessionTitle(value, suffix string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	limit := maxSessionTitleLength - len([]rune(suffix))
	if limit <= 0 {
		return strings.TrimSpace(suffix)
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value + suffix
	}
	if suffix == "" {
		limit = maxSessionTitleLength - 3
		if limit <= 0 {
			return "..."
		}
		return strings.TrimSpace(string(runes[:limit])) + "..."
	}
	return strings.TrimSpace(string(runes[:limit])) + suffix
}

func isGenericSessionTitle(title string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	if t == "" {
		return true
	}
	switch t {
	case "default",
		"new contenox session",
		"new session",
		"new chat",
		"contenox chat",
		"vscode-chat",
		"vscode-lm",
		"vscode-agent-session",
		"untitled",
		"untitled chat":
		return true
	}
	if strings.HasPrefix(t, "session-") {
		return true
	}
	// Catch "New session (2)", "new session (3)", etc. produced by uniqueSessionTitle
	// when a generic starter was not renamed.
	if strings.HasPrefix(t, "new session (") || strings.HasPrefix(t, "new chat (") {
		return true
	}
	return false
}

func stripCaseInsensitivePrefix(value, prefix string) string {
	if strings.HasPrefix(strings.ToLower(value), strings.ToLower(prefix)) {
		return strings.TrimSpace(value[len(prefix):])
	}
	return value
}

func extractAssistantText(output any) string {
	switch v := output.(type) {
	case string:
		return v
	case taskengine.ChatHistory:
		for i := len(v.Messages) - 1; i >= 0; i-- {
			if v.Messages[i].Role == "assistant" && v.Messages[i].Content != "" {
				return v.Messages[i].Content
			}
		}
	case *taskengine.ChatHistory:
		if v != nil {
			return extractAssistantText(*v)
		}
	}
	return ""
}
