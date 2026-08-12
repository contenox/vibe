package agentservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/models/llmrepo"
	"github.com/contenox/contenox/internal/services/chatservice"
	"github.com/contenox/contenox/internal/services/sessionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/google/uuid"
)

type Agent interface {
	Capabilities(ctx context.Context) (*AgentCapabilities, error)
	SessionNew(ctx context.Context, name string) (string, error)
	SessionList(ctx context.Context) ([]*SessionInfo, error)
	SessionLoad(ctx context.Context, name string) (contenoxSessionID string, messages []taskengine.Message, err error)
	// SessionResume switches sessions without loading message history.
	SessionResume(ctx context.Context, name string) (contenoxSessionID string, err error)
	// SessionDelete removes a session and its messages by name; idempotent.
	SessionDelete(ctx context.Context, name string) error
	SessionEnsureDefault(ctx context.Context) (string, error)
	Prompt(ctx context.Context, req PromptRequest) (*PromptResponse, error)
}

type Deps struct {
	Engine      *enginesvc.Engine
	DB          libdb.DBManager
	WorkspaceID string
	Identity    string
}

func New(deps Deps) Agent {
	if deps.Identity == "" {
		deps.Identity = "local-user"
	}
	return &agent{deps: deps}
}

type agent struct {
	deps Deps
}

func (a *agent) Capabilities(_ context.Context) (*AgentCapabilities, error) {
	return &AgentCapabilities{
		LocalTools:      a.deps.Engine.LocalTools,
		SupportsSession: true,
	}, nil
}

func (a *agent) sessionSvc() sessionservice.Service {
	return sessionservice.New(a.deps.DB, a.deps.WorkspaceID, a.tracker())
}

func (a *agent) chatMgr() *chatservice.Manager {
	return chatservice.NewManager(a.deps.WorkspaceID)
}

func (a *agent) SessionNew(ctx context.Context, name string) (string, error) {
	return a.sessionSvc().New(ctx, a.deps.Identity, name)
}

func (a *agent) SessionList(ctx context.Context) ([]*SessionInfo, error) {
	sessions, err := a.sessionSvc().List(ctx, a.deps.Identity)
	if err != nil {
		return nil, err
	}
	out := make([]*SessionInfo, len(sessions))
	for i, s := range sessions {
		out[i] = &SessionInfo{
			ID:           s.ID,
			Name:         s.Name,
			MessageCount: s.MessageCount,
			IsActive:     s.IsActive,
		}
	}
	return out, nil
}

func (a *agent) SessionLoad(ctx context.Context, name string) (string, []taskengine.Message, error) {
	if err := a.sessionSvc().Switch(ctx, a.deps.Identity, name); err != nil {
		return "", nil, err
	}
	sessionID, err := a.sessionSvc().GetActiveID(ctx)
	if err != nil {
		return "", nil, err
	}
	if sessionID == "" {
		return "", nil, fmt.Errorf("no active session after switch")
	}
	msgs, err := a.chatMgr().ListMessages(ctx, a.deps.DB.WithoutTransaction(), sessionID)
	if err != nil {
		return "", nil, err
	}
	return sessionID, msgs, nil
}

func (a *agent) SessionResume(ctx context.Context, name string) (string, error) {
	if err := a.sessionSvc().Switch(ctx, a.deps.Identity, name); err != nil {
		return "", err
	}
	sessionID, err := a.sessionSvc().GetActiveID(ctx)
	if err != nil {
		return "", err
	}
	if sessionID == "" {
		return "", fmt.Errorf("no active session after switch")
	}
	return sessionID, nil
}

func (a *agent) SessionDelete(ctx context.Context, name string) error {
	_, err := a.sessionSvc().Delete(ctx, a.deps.Identity, name)
	if err != nil && errors.Is(err, libdb.ErrNotFound) {
		return nil
	}
	return err
}

func (a *agent) SessionEnsureDefault(ctx context.Context) (string, error) {
	return a.sessionSvc().EnsureDefault(ctx, a.deps.Identity)
}

func (a *agent) tracker() libtracker.ActivityTracker {
	if a.deps.Engine != nil && a.deps.Engine.Tracker != nil {
		return a.deps.Engine.Tracker
	}
	return libtracker.NoopTracker{}
}

func (a *agent) Prompt(ctx context.Context, req PromptRequest) (*PromptResponse, error) {
	promptReportErr, _, promptEnd := a.tracker().Start(ctx, "execute", "prompt", "sessionID", req.SessionID)
	defer promptEnd()

	if req.Chain == nil {
		err := fmt.Errorf("PromptRequest.Chain is required")
		promptReportErr(err)
		return nil, err
	}
	chain := req.Chain

	templateVars := req.TemplateVars
	if templateVars == nil {
		templateVars = map[string]string{}
	}
	templateVars["chain"] = chain.ID
	ctx = taskengine.WithTemplateVars(ctx, templateVars)
	ctx = taskengine.WithRequestedContextLength(ctx, req.ContextLength)
	if req.ToolsAllowlist != nil {
		ctx = taskengine.WithRuntimeToolsAllowlist(ctx, req.ToolsAllowlist)
	}

	var inputVal any
	var inputType taskengine.DataType
	var err error
	if req.InputValue != nil {
		inputVal = req.InputValue
		inputType = req.InputType
	} else {
		inputVal, inputType, err = a.buildChatInput(ctx, req)
		if err != nil {
			promptReportErr(err)
			return nil, err
		}
	}

	if req.SessionID != "" {
		ctx = context.WithValue(ctx, runtimetypes.SessionIDContextKey, req.SessionID)
		// Cache-affinity key, one-way hashed so the raw ID never reaches providers.
		ctx = llmrepo.WithSessionKey(ctx, llmrepo.DeriveSessionKey(req.SessionID))
	}

	if req.Observer != nil {
		if _, isNoop := req.Observer.(NoopObserver); !isNoop {
			stopObs := a.startObserving(ctx, req.Observer)
			defer stopObs()
		}
	}

	ctx = taskengine.WithCheckpointSaver(ctx, a.checkpointSaver(req.SessionID, req.ChainRef))

	output, outputType, stateUnits, execErr := a.deps.Engine.TaskService.Execute(ctx, chain, inputVal, inputType)

	// Suspension is a typed terminal, not a failure; the resume path persists once.
	var susp *taskengine.ChainSuspendedError
	if execErr != nil && errors.As(execErr, &susp) {
		// Closes the row-vs-checkpoint race: resume inline if no hook exists yet.
		if resp, resumed := a.resumeIfAlreadyAnswered(ctx, susp.ApprovalID); resumed {
			return resp, nil
		}
		return &PromptResponse{
			Output:              output,
			OutputType:          outputType,
			Steps:               stateUnits,
			StopReason:          StopSuspended,
			SuspendedApprovalID: susp.ApprovalID,
		}, nil
	}

	if req.SessionID != "" {
		isPoisonPill := false
		if execErr != nil && errors.Is(execErr, taskengine.ErrContextLengthExceeded) {
			if len(stateUnits) <= 1 {
				isPoisonPill = true
				promptReportErr(fmt.Errorf("input rejected to protect session: %w", execErr))
			}
		}

		if !isPoisonPill {
			a.persistHistory(ctx, req.SessionID, inputVal, stateUnits, execErr, req.ChainRef)
		}
	}

	resp := &PromptResponse{
		Output:     output,
		OutputType: outputType,
		Steps:      stateUnits,
		StopReason: InferStopReason(execErr, stateUnits),
	}
	if execErr != nil {
		return resp, fmt.Errorf("chain execution failed: %w", execErr)
	}
	return resp, nil
}

func (a *agent) startObserving(ctx context.Context, obs Observer) func() {
	bus := a.deps.Engine.Bus
	if bus == nil {
		return func() {}
	}
	reqID, ok := ctx.Value(libtracker.ContextKeyRequestID).(string)
	if !ok || reqID == "" {
		return func() {}
	}

	subject := taskengine.StateSubject(reqID)
	streamCtx, cancel := context.WithCancel(ctx)
	rawCh := make(chan []byte, 32)
	sub, err := bus.Stream(streamCtx, subject, rawCh)
	if err != nil {
		cancel()
		return func() {}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-streamCtx.Done():
				return
			case payload, ok := <-rawCh:
				if !ok {
					return
				}
				var unit taskengine.CapturedStateUnit
				if err := json.Unmarshal(payload, &unit); err != nil {
					continue
				}
				obs.OnStepCompleted(unit)
			}
		}
	}()

	return func() {
		cancel()
		_ = sub.Unsubscribe()
		<-done
	}
}

// ComposeUserInput prepends per-turn context artifacts as an "Additional context" block.
func ComposeUserInput(input string, contextBundle map[string]any) string {
	content := input

	if contextBundle != nil {
		if arts, ok := contextBundle["artifacts"].([]any); ok && len(arts) > 0 {
			var ctxParts []string
			for _, a := range arts {
				if m, ok := a.(map[string]any); ok {
					kind := ""
					if k, ok := m["kind"].(string); ok {
						kind = k
					}
					payload := m["payload"]
					ctxParts = append(ctxParts, fmt.Sprintf("[%s] %v", kind, payload))
				}
			}
			if len(ctxParts) > 0 {
				content = "Additional context:\n" + strings.Join(ctxParts, "\n") + "\n\n" + content
			}
		}
	}

	return content
}

func (a *agent) buildChatInput(ctx context.Context, req PromptRequest) (any, taskengine.DataType, error) {
	var history []taskengine.Message

	if req.SessionID != "" {
		sessionReportErr, _, sessionEnd := a.tracker().Start(ctx, "load", "chat_history", "sessionID", req.SessionID)
		msgs, err := a.chatMgr().ListMessages(ctx, a.deps.DB.WithoutTransaction(), req.SessionID)
		if err != nil {
			sessionReportErr(err)
		} else {
			history = msgs
		}
		sessionEnd()
	}

	history = trimHistoryChunked(history, req.HistoryTrim)

	if len(history) == 0 && req.AgentsMD != "" {
		history = append([]taskengine.Message{AgentsMDMessage(req.AgentsMD, req.AgentsMDSource)}, history...)
		_, reportChange, end := a.tracker().Start(ctx, "load", "agents_md", "source", req.AgentsMDSource, "bytes", len(req.AgentsMD))
		reportChange(req.AgentsMDSource, len(req.AgentsMD))
		end()
	}

	inputContent := ComposeUserInput(req.Input, req.Context)

	userMsg := taskengine.Message{ID: uuid.NewString(), Role: "user", Content: inputContent, Images: req.Images, Audio: req.Audio, Timestamp: time.Now().UTC()}
	chatInput := taskengine.ChatHistory{
		Messages: append(history, userMsg),
	}

	return chatInput, taskengine.DataTypeChatHistory, nil
}

func stampTurnProvenance(msgs []taskengine.Message, requestID, chainRef string) {
	for i := range msgs {
		if msgs[i].RequestID != "" {
			continue
		}
		msgs[i].RequestID = requestID
		msgs[i].ChainRef = chainRef
	}
}

func (a *agent) persistHistory(ctx context.Context, sessionID string, input any, stateUnits []taskengine.CapturedStateUnit, chainErr error, chainRef string) {
	chatInput, ok := input.(taskengine.ChatHistory)
	if !ok {
		return
	}

	synthesized := taskengine.SynthesizeHistory(chatInput.Messages, stateUnits, chainErr)
	if requestID, ok := ctx.Value(libtracker.ContextKeyRequestID).(string); ok && requestID != "" {
		stampTurnProvenance(synthesized, requestID, chainRef)
	}
	cleanCtx := context.WithoutCancel(ctx)

	persistReportErr, persistReportChange, persistEnd := a.tracker().Start(cleanCtx, "persist", "chat_history", "sessionID", sessionID)
	defer persistEnd()

	exec, commit, release, txErr := a.deps.DB.WithTransaction(cleanCtx)
	if txErr != nil {
		persistReportErr(fmt.Errorf("start transaction: %w", txErr))
		return
	}
	defer release()

	if persistErr := a.chatMgr().PersistDiff(cleanCtx, exec, sessionID, synthesized); persistErr != nil {
		persistReportErr(fmt.Errorf("persist diff: %w", persistErr))
		return
	}

	if commitErr := commit(cleanCtx); commitErr != nil {
		persistReportErr(fmt.Errorf("commit: %w", commitErr))
		return
	}

	persistReportChange(sessionID, len(synthesized))
}

func AgentsMDMessage(content, path string) taskengine.Message {
	return taskengine.Message{
		ID:        uuid.NewString(),
		Role:      "system",
		Content:   fmt.Sprintf("Project context loaded from %s (AGENTS.md, community standard from agents.md). Treat this as project-specific reference material and conventions, not unconditional rules. Loaded once at session start; if it changes, start a new session to pick up the update.\n\n%s", path, content),
		Timestamp: time.Now().UTC(),
	}
}
