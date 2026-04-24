package chatsessionmodes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/runtime/chatservice"
	"github.com/contenox/contenox/runtime/execservice"
	"github.com/contenox/contenox/runtime/internal/clikv"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/planservice"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/contenox/contenox/runtime/taskchainservice"
	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/google/uuid"
)

type Service struct {
	db           libdbexec.DBManager
	taskService  execservice.TasksEnvService
	chainService taskchainservice.Service
	planService  planservice.Service
	chatManager  *chatservice.Manager
	resolver     ChainResolver
	registry     *ModeRegistry
	workspaceID  string
}

type Deps struct {
	DB           libdbexec.DBManager
	TaskService  execservice.TasksEnvService
	ChainService taskchainservice.Service
	PlanService  planservice.Service
	WorkspaceID  string
	ModeToDefaultChain map[string]string
}

func New(deps Deps) *Service {
	modeMap := deps.ModeToDefaultChain
	if len(modeMap) == 0 {
		modeMap = DefaultChainByMode
	}
	resolver := &MapChainResolver{ModeToChain: modeMap}
	reg := NewDefaultModeRegistry(deps.PlanService)
	return &Service{
		db:           deps.DB,
		taskService:  deps.TaskService,
		chainService: deps.ChainService,
		planService:  deps.PlanService,
		chatManager:  chatservice.NewManager(deps.WorkspaceID),
		resolver:     resolver,
		registry:     reg,
		workspaceID:  deps.WorkspaceID,
	}
}

// SendTurn runs load history → inject → execute → persist.
func (s *Service) SendTurn(ctx context.Context, in TurnInput) (*TurnResult, error) {
	if strings.EqualFold(strings.TrimSpace(in.Mode), "build") {
		return s.sendBuildTurn(ctx, in)
	}

	if strings.TrimSpace(in.Message) == "" {
		return nil, fmt.Errorf("message is required")
	}

	chainRef, err := s.resolver.Resolve(in.ExplicitChainRef, in.Mode)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	injectIn := InjectInput{SessionID: in.SessionID, Now: now, Turn: in}
	var injected []taskengine.Message
	for _, inj := range s.registry.Injectors(in.Mode) {
		part, err := inj.Inject(ctx, injectIn)
		if err != nil {
			return nil, err
		}
		injected = append(injected, part...)
	}

	tx := s.db.WithoutTransaction()
	messages, err := s.chatManager.ListMessages(ctx, tx, in.SessionID)
	if err != nil {
		return nil, err
	}
	messages, err = s.chatManager.AppendMessage(ctx, messages, now, in.Message, "user")
	if err != nil {
		return nil, err
	}
	messages, err = PrependInjectionsBeforeLastUser(messages, injected)
	if err != nil {
		return nil, err
	}

	chain, err := s.chainService.Get(ctx, chainRef)
	if err != nil {
		return nil, err // not found / invalid chain
	}

	rtStore := runtimetypes.New(s.db.WithoutTransaction())
	model := strings.TrimSpace(in.Model)
	provider := strings.TrimSpace(in.Provider)
	if model == "" {
		model = clikv.Read(ctx, rtStore, "default-model")
	}
	if provider == "" {
		provider = clikv.Read(ctx, rtStore, "default-provider")
	}
	if model == "" || provider == "" {
		return nil, fmt.Errorf("%w: set defaults or pass ?model= and ?provider=", ErrMissingModelProvider)
	}

	templateVars := map[string]string{
		"model":    model,
		"provider": provider,
		"mode":     strings.TrimSpace(in.Mode),
	}
	if reqID, ok := ctx.Value(libtracker.ContextKeyRequestID).(string); ok && reqID != "" {
		templateVars["request_id"] = reqID
	}
	execCtx := taskengine.WithTemplateVars(ctx, templateVars)
	// Attach a widget-hint sink so hooks (local_fs.read_file, etc.) can emit
	// inline-rendering hints that the SSE event publisher drains into
	// TaskEvent.Attachments. Phase 5 of the Beam canvas-vision plan.
	execCtx = taskengine.WithWidgetHintSink(execCtx, &taskengine.WidgetHintSink{})

	result, dt, capturedStateUnits, err := s.taskService.Execute(execCtx, chain, taskengine.ChatHistory{Messages: messages}, taskengine.DataTypeChatHistory)
	if err != nil {
		return nil, err
	}

	out := &TurnResult{State: capturedStateUnits}
	switch dt {
	case taskengine.DataTypeChatHistory:
		history, ok := result.(taskengine.ChatHistory)
		if !ok || len(history.Messages) == 0 {
			return nil, fmt.Errorf("chain did not return valid chat history")
		}
		last := history.Messages[len(history.Messages)-1]
		out.Response = last.Content
		out.InputTokenCount = history.InputTokens
		out.OutputTokenCount = history.OutputTokens
		toPersist := MergeChatHistoryPreservingInjections(injected, history.Messages)
		if perr := s.chatManager.PersistDiff(ctx, tx, in.SessionID, toPersist); perr != nil {
			return nil, perr
		}
	case taskengine.DataTypeString, taskengine.DataTypeJSON, taskengine.DataTypeAny, taskengine.DataTypeNil,
		taskengine.DataTypeInt:
		out.Response = FormatChainResultForChat(result)
		messages = append(messages, taskengine.Message{
			ID:        uuid.NewString(),
			Role:      "assistant",
			Content:   out.Response,
			Timestamp: time.Now().UTC(),
		})
		if perr := s.chatManager.PersistDiff(ctx, tx, in.SessionID, messages); perr != nil {
			return nil, perr
		}
	default:
		return nil, fmt.Errorf("unsupported chain output type %q", dt.String())
	}
	return out, nil
}
