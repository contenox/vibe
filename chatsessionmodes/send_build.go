package chatsessionmodes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/clikv"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/plancompile"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/taskengine"
	"github.com/google/uuid"
)

func (s *Service) sendBuildTurn(ctx context.Context, in TurnInput) (*TurnResult, error) {
	if s.planService == nil {
		return nil, fmt.Errorf("build mode requires PlanService")
	}
	if strings.TrimSpace(in.ExplicitChainRef) == "" {
		return nil, fmt.Errorf("chainId query parameter is required for build mode")
	}
	if strings.TrimSpace(in.SummarizerChainRef) == "" {
		return nil, fmt.Errorf("summarizerChainId query parameter is required for build mode")
	}

	now := time.Now().UTC()
	userMsg := strings.TrimSpace(in.Message)
	if userMsg == "" {
		userMsg = "Run compiled active plan"
	}

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
	messages, err = s.chatManager.AppendMessage(ctx, messages, now, userMsg, "user")
	if err != nil {
		return nil, err
	}
	messages, err = PrependInjectionsBeforeLastUser(messages, injected)
	if err != nil {
		return nil, err
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

	sum := sha256.Sum256([]byte(in.SessionID))
	compiledID := "compiled-build-" + hex.EncodeToString(sum[:])[:16]

	res, err := plancompile.RunActiveCompiled(execCtx, s.planService, s.chainService, s.taskService, in.ExplicitChainRef, in.SummarizerChainRef, compiledID, "", nil, nil)
	if err != nil {
		return nil, err
	}

	out := &TurnResult{State: res.State}
	out.Response = FormatChainResultForChat(res.Output)
	messages = append(messages, taskengine.Message{
		ID:        uuid.NewString(),
		Role:      "assistant",
		Content:   out.Response,
		Timestamp: time.Now().UTC(),
	})
	if perr := s.chatManager.PersistDiff(ctx, tx, in.SessionID, messages); perr != nil {
		return nil, perr
	}
	return out, nil
}
