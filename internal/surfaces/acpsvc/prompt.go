package acpsvc

import (
	"context"
	"errors"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/models/llmrepo"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/libtracker"
)

// Prompt resolves the session, claims it for HITL routing, and dispatches the
// turn to its driver.
func (t *Transport) Prompt(ctx context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
	sess, ok := t.sessionFor(req.SessionID)
	if !ok {
		reportErr, _, end := t.tracker().Start(ctx, "prompt", "acp_session", "session_id", string(req.SessionID), "prompt_blocks", len(req.Prompt))
		defer end()
		err := libacp.NewErrorf(libacp.ErrInvalidParams, "unknown session %q", req.SessionID)
		reportErr(err)
		return libacp.PromptResponse{}, err
	}
	t.claimSessionRouting(sess)
	resp, err := sess.driver.Prompt(ctx, req, sess)
	if err != nil {
		return resp, err
	}
	return t.explainTurnStop(ctx, req.SessionID, sess, resp), nil
}

// nativeDriver drives a session against the contenox task-chain engine.
type nativeDriver struct {
	t     *Transport
	agent agentservice.Agent
}

func (d *nativeDriver) AgentName() string { return "" }

func (d *nativeDriver) Close() error { return nil }

func (d *nativeDriver) AvailableCommands() []libacp.AvailableCommand { return d.t.acpCommands() }

func (d *nativeDriver) ConfigOptions(ctx context.Context, sess *sessionEntry) []libacp.SessionConfigOption {
	t := d.t
	opts := []libacp.SessionConfigOption{
		t.modelConfigOption(ctx, sess),
		t.hitlPolicyConfigOption(sess),
		t.thinkConfigOption(sess),
		t.tokenLimitConfigOption(ctx, sess),
	}
	if opt, ok := t.workspaceRootConfigOption(sess); ok {
		opts = append(opts, opt)
	}
	if opt, ok := t.agentConfigOption(ctx, sess); ok {
		opts = append(opts, opt)
	}
	return opts
}

func (d *nativeDriver) SetConfigOption(ctx context.Context, sess *sessionEntry, configID string, value libacp.SessionConfigOptionValue) error {
	return d.t.setSessionConfigOption(ctx, sess, configID, value.AsString())
}

// Prompt runs one native turn: it intercepts slash commands, then executes the
// default chain, translating engine events to session/update notifications.
func (d *nativeDriver) Prompt(ctx context.Context, req libacp.PromptRequest, sess *sessionEntry) (libacp.PromptResponse, error) {
	t := d.t
	reportErr, reportChange, end := t.tracker().Start(ctx, "prompt", "acp_session", "session_id", string(req.SessionID), "prompt_blocks", len(req.Prompt))
	defer end()

	if t.deps.ChainRegistry == nil || t.deps.ChainRegistry.Default() == nil {
		err := libacp.InternalError("no chain configured")
		reportErr(err)
		return libacp.PromptResponse{}, err
	}

	// Extracted before FlattenContent, whose text projection would drop them.
	images, nonImageBlocks := extractImageParts(req.Prompt)
	audio, textBlocks := extractAudioParts(nonImageBlocks)
	input, droppedContentKinds := libacp.FlattenContent(textBlocks)

	var audioRefusal string
	if len(audio) > 0 {
		if reason := t.sessionAudioRefusal(ctx, sess); reason != "" {
			audioRefusal = reason
			audio = nil
			droppedContentKinds = appendDroppedKind(droppedContentKinds, string(libacp.ContentKindAudio))
		}
	}

	if input == "" && len(images) == 0 && len(audio) == 0 {
		if audioRefusal != "" {
			announceDroppedContent(ctx, req.SessionID, droppedContentKinds, audioRefusal, t.sendUpdate)
			err := libacp.NewError(libacp.ErrInvalidParams, audioRefusal)
			reportErr(err)
			return libacp.PromptResponse{}, err
		}
		err := libacp.NewError(libacp.ErrInvalidParams, "empty prompt")
		reportErr(err)
		return libacp.PromptResponse{}, err
	}

	// Handled before dispatchCommand because a command handler returns text and
	// cannot prompt the model.
	if goal, ok := parsePlanCommand(input); ok {
		if !t.hasMissionCapability() {
			err := planPreambleForMissingFleet()
			reportErr(err)
			return libacp.PromptResponse{}, err
		}
		if goal == "" {
			err := errors.New(planUsageLine)
			reportErr(err)
			return libacp.PromptResponse{}, err
		}
		input = planPreamble(goal)
	}

	if name, args, ok := parseCommand(input); ok {
		if len(images) > 0 {
			droppedContentKinds = appendDroppedKind(droppedContentKinds, string(libacp.ContentKindImage))
		}
		if len(audio) > 0 {
			droppedContentKinds = appendDroppedKind(droppedContentKinds, string(libacp.ContentKindAudio))
		}
		cmdCtx := libtracker.WithNewRequestID(ctx)
		announceDroppedContent(ctx, req.SessionID, droppedContentKinds, audioRefusal, t.sendUpdate)
		resp, err := t.dispatchCommand(cmdCtx, req.SessionID, sess, name, args)
		if err != nil {
			return resp, err
		}
		return withDroppedContentMeta(resp, droppedContentKinds), nil
	}

	if name, ok := unknownCommandName(input); ok {
		if len(images) > 0 {
			droppedContentKinds = appendDroppedKind(droppedContentKinds, string(libacp.ContentKindImage))
		}
		if len(audio) > 0 {
			droppedContentKinds = appendDroppedKind(droppedContentKinds, string(libacp.ContentKindAudio))
		}
		reportChange(string(req.SessionID), map[string]any{
			"stop_reason":           string(libacp.StopReasonEndTurn),
			"unknown_command":       name,
			"dropped_content_kinds": droppedContentKinds,
		})
		announceDroppedContent(ctx, req.SessionID, droppedContentKinds, audioRefusal, t.sendUpdate)
		return withDroppedContentMeta(t.answerUnknownCommand(ctx, req.SessionID, name), droppedContentKinds), nil
	}

	// With a native-turn Registry the turn runs off this connection, so a client
	// drop doesn't cancel it.
	if t.deps.NativeTurns != nil {
		return d.promptViaRegistry(ctx, req, sess, input, images, audio, audioRefusal, droppedContentKinds)
	}

	announceDroppedContent(ctx, req.SessionID, droppedContentKinds, audioRefusal, t.sendUpdate)

	promptCtx := libtracker.WithNewRequestID(ctx)
	reqID, _ := promptCtx.Value(libtracker.ContextKeyRequestID).(string)

	promptCtx, cancelPrompt := context.WithCancel(promptCtx)
	promptReg := t.registerPromptCancel(req.SessionID, cancelPrompt)
	defer func() {
		t.unregisterPromptCancel(req.SessionID, promptReg)
		cancelPrompt()
	}()

	// One shared hitlservice sits behind every session, so the per-session policy
	// rides the request context.
	if policyName := t.resolveSessionHITLPolicy(sess); policyName != "" {
		promptCtx = hitlservice.WithPolicyName(promptCtx, policyName)
	}
	promptCtx = vfs.WithSessionCwd(promptCtx, sess.Cwd)

	if sess.MissionID != "" {
		promptCtx = missiontools.WithMissionID(promptCtx, sess.MissionID)
		promptCtx = missiontools.WithWorkdir(promptCtx, sess.Cwd)
		promptCtx = llmrepo.WithResolutionBounds(promptCtx, sess.resolutionBounds())
	}
	// A session that can supervise subagents carries its own id in, unlocking the
	// supervisor tools. A unit is excluded: subagents do not spawn subagents.
	if sess.MissionID == "" && sess.InternalSessionID != "" && t.hasMissionCapability() {
		promptCtx = missiontools.WithParentSessionID(promptCtx, sess.InternalSessionID)
	}

	rawCh := make(chan []byte, 64)
	bus := t.deps.Engine.Bus
	if bus != nil && reqID != "" {
		sub, err := bus.Stream(promptCtx, taskengine.TaskEventRequestSubject(reqID), rawCh)
		if err != nil {
			subErr, _, subEnd := t.tracker().Start(promptCtx, "subscribe", "acp_event_stream", "session_id", string(req.SessionID), "request_id", reqID)
			subErr(err)
			subEnd()
		} else {
			translateDone := make(chan struct{})
			go func() {
				defer close(translateDone)
				t.translateEvents(promptCtx, req.SessionID, rawCh)
			}()
			defer func() {
				_ = sub.Unsubscribe()
				close(rawCh)
				<-translateDone
			}()
		}
	}

	templateVars := t.chainTemplateVars(sess)
	templateVars["think"] = sess.think()
	var toolsAllowlist []string
	if t.deps.DB != nil {
		var err error
		toolsAllowlist, err = t.runtimeToolsAllowlist(promptCtx, runtimetypes.New(t.deps.DB.WithoutTransaction()), sess.McpServerNames)
		if err != nil {
			reportErr(err)
			return libacp.PromptResponse{}, libacp.InternalError(err.Error())
		}
	}

	contextLen := sess.effectiveTokenLimit()
	if contextLen == 0 {
		currentModel := sess.modelOrDefault(t.model())
		for _, state := range t.runtimeStates(promptCtx) {
			for _, pulled := range state.PulledModels {
				if pulled.Model == currentModel && pulled.ContextLength > 0 {
					contextLen = pulled.ContextLength
					break
				}
			}
			if contextLen > 0 {
				break
			}
		}
		if contextLen == 0 {
			for _, state := range t.runtimeStates(promptCtx) {
				for _, pulled := range state.PulledModels {
					if pulled.ContextLength > 0 && (pulled.CanChat || pulled.CanPrompt) {
						contextLen = pulled.ContextLength
						break
					}
				}
				if contextLen > 0 {
					break
				}
			}
		}
	}

	resp, err := d.agent.Prompt(promptCtx, agentservice.PromptRequest{
		SessionID:      sess.InternalSessionID,
		Input:          input,
		Images:         images,
		Audio:          audio,
		Chain:          t.deps.ChainRegistry.Default(),
		TemplateVars:   templateVars,
		ToolsAllowlist: toolsAllowlist,
		ContextLength:  contextLen,
	})
	if err != nil {
		// Only context.Canceled is a real cancellation; agentservice maps
		// DeadlineExceeded to StopCancelled too, so resp.StopReason is not enough.
		cancelled := errors.Is(err, context.Canceled) ||
			errors.Is(promptCtx.Err(), context.Canceled) ||
			errors.Is(ctx.Err(), context.Canceled)
		if cancelled {
			reportChange(string(req.SessionID), map[string]any{
				"stop_reason":           string(libacp.StopReasonCancelled),
				"request_id":            reqID,
				"dropped_content_kinds": droppedContentKinds,
			})
			return withDroppedContentMeta(libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, droppedContentKinds), nil
		}
		reportErr(err)
		if resp != nil {
			return libacp.PromptResponse{StopReason: mapStopReason(resp.StopReason)}, libacp.InternalError(err.Error())
		}
		return libacp.PromptResponse{}, libacp.InternalError(err.Error())
	}
	stopReason := mapStopReason(resp.StopReason)
	// The engine can absorb a cancellation via a recovery task and return a
	// partial success.
	if errors.Is(promptCtx.Err(), context.Canceled) {
		stopReason = libacp.StopReasonCancelled
	}
	suspended := resp.StopReason == agentservice.StopSuspended && stopReason != libacp.StopReasonCancelled
	libacp.AfterResponse(ctx, func() {
		update := libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateSessionInfo,
			UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
		}
		if title := t.sessionInfoTitle(ctx, sess.InternalSessionID); title != "" {
			update.Title = title
		}
		t.sendUpdate(ctx, libacp.SessionNotification{
			SessionID: req.SessionID,
			Update:    update,
		})
	})
	if suspended {
		reportChange(string(req.SessionID), map[string]any{
			"stop_reason":           stopReasonSuspended,
			"approval_id":           resp.SuspendedApprovalID,
			"request_id":            reqID,
			"dropped_content_kinds": droppedContentKinds,
		})
		t.sendUpdate(ctx, libacp.SessionNotification{
			SessionID: req.SessionID,
			Update:    suspensionNotice(resp.SuspendedApprovalID),
		})
		return withDroppedContentMeta(libacp.PromptResponse{StopReason: stopReason, Meta: suspensionMeta(resp.SuspendedApprovalID)}, droppedContentKinds), nil
	}
	if resp.StopReason == agentservice.StopFailed && stopReason != libacp.StopReasonCancelled {
		cause := agentservice.RecoveredFailure(resp.Steps)
		reportChange(string(req.SessionID), map[string]any{
			"stop_reason":           stopReasonFailed,
			"cause":                 cause,
			"request_id":            reqID,
			"dropped_content_kinds": droppedContentKinds,
		})
		t.sendUpdate(ctx, libacp.SessionNotification{
			SessionID: req.SessionID,
			Update:    recoveredFailureNotice(cause),
		})
		return withDroppedContentMeta(libacp.PromptResponse{StopReason: stopReason, Meta: recoveredFailureMeta(cause)}, droppedContentKinds), nil
	}
	reportChange(string(req.SessionID), map[string]any{
		"stop_reason":           string(stopReason),
		"request_id":            reqID,
		"dropped_content_kinds": droppedContentKinds,
	})
	return withDroppedContentMeta(libacp.PromptResponse{StopReason: stopReason}, droppedContentKinds), nil
}

// mapStopReason projects agentservice's stop reasons onto ACP's closed set.
func mapStopReason(r agentservice.StopReason) libacp.StopReason {
	switch r {
	case agentservice.StopEndTurn:
		return libacp.StopReasonEndTurn
	case agentservice.StopMaxTokens:
		return libacp.StopReasonMaxTokens
	case agentservice.StopMaxTurnRequests:
		return libacp.StopReasonMaxTurnRequests
	case agentservice.StopCancelled:
		return libacp.StopReasonCancelled
	case agentservice.StopSuspended:
		// ACP has no suspended reason; the park travels on the response _meta.
		return libacp.StopReasonEndTurn
	case agentservice.StopFailed:
		// As with StopSuspended: the cause travels through recoveredFailureMeta.
		return libacp.StopReasonEndTurn
	}
	return libacp.StopReasonEndTurn
}
