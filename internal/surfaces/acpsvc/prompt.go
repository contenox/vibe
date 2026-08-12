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

// Prompt resolves the session and dispatches the turn to its driver. The driver
// (native chain engine vs. registered downstream ACP agent) owns everything the
// turn does — there is no native-vs-external branch here. It is also where a
// stop reason leaves acpsvc, so a turn that ended short is explained here (see
// explainTurnStop) rather than in each driver.
//
// It is also where this connection claims the session for HITL routing (see
// claimSessionRouting): the client that asked for the turn is the client that
// must answer the permission requests the turn raises, and it is not
// necessarily the client that opened the session.
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

// nativeDriver drives a session against the contenox task-chain engine — the
// historical (non-external) ACP path. It wraps the session's agentservice.Agent
// and owns the chain execution + event-translation flow.
type nativeDriver struct {
	t     *Transport
	agent agentservice.Agent
}

// AgentName is "" for a native session (no external agent attribution).
func (d *nativeDriver) AgentName() string { return "" }

// Close is a no-op: a native session holds no downstream connection.
func (d *nativeDriver) Close() error { return nil }

// AvailableCommands advertises contenox's admin slash-command menu, filtered to
// what this transport can actually run (see (*Transport).acpCommands).
func (d *nativeDriver) AvailableCommands() []libacp.AvailableCommand { return d.t.acpCommands() }

// ConfigOptions returns the chain-engine config selects (model/HITL/think/token,
// plus the workspace root when an allowlist is configured and the agent picker
// when the machine has registered agents).
//
// The agent picker is reached through here by the initialize-time snapshot too
// (workspaceConfigOptions seeds a native driver), which is where a client picks
// the agent for a session that does not exist yet.
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

// SetConfigOption applies a native config change (model/think/policy/token),
// byte-identical to the pre-driver-seam path: it delegates to the same
// transport-level switch the RPC handler used directly before the driver seam,
// dropping the boolean/string union back to the string form the native selects
// consume.
func (d *nativeDriver) SetConfigOption(ctx context.Context, sess *sessionEntry, configID string, value libacp.SessionConfigOptionValue) error {
	return d.t.setSessionConfigOption(ctx, sess, configID, value.AsString())
}

// Prompt runs one native turn: it intercepts slash commands, then executes the
// default chain, translating engine events to session/update notifications.
//
// A turn that parks on a human approval resolves here too, and must not read
// as a completed one. ACP has no suspended stop reason, so the token stays
// end_turn (see mapStopReason) and the park is reported by the two things that
// name it: an announced notice carrying the approval id, and the same trio on
// the response `_meta`, which a finished turn never carries.
//
// Prompt content that could not be forwarded is reported the same way, on the
// sibling contenox.droppedContent envelope (see droppedContentReport). Every
// branch that computes the kinds carries them out: the two command paths, the
// cancellation, the park, and the ordinary ending. Nothing about what is
// dropped, or when, is decided here — only whether the client is told.
func (d *nativeDriver) Prompt(ctx context.Context, req libacp.PromptRequest, sess *sessionEntry) (libacp.PromptResponse, error) {
	t := d.t
	reportErr, reportChange, end := t.tracker().Start(ctx, "prompt", "acp_session", "session_id", string(req.SessionID), "prompt_blocks", len(req.Prompt))
	defer end()

	if t.deps.ChainRegistry == nil || t.deps.ChainRegistry.Default() == nil {
		err := libacp.InternalError("no chain configured")
		reportErr(err)
		return libacp.PromptResponse{}, err
	}

	// Images and audio are extracted first (see extractImageParts and
	// extractAudioParts): FlattenContent's lossy text projection would drop
	// them. An attachment-only prompt is valid.
	images, nonImageBlocks := extractImageParts(req.Prompt)
	audio, textBlocks := extractAudioParts(nonImageBlocks)
	input, droppedContentKinds := libacp.FlattenContent(textBlocks)

	// Pre-flight capability gate (see sessionAudioRefusal): audio the fleet is
	// known unable to accept is refused here, like an extraction refusal — the
	// turn proceeds on the rest of the prompt, and no audio-bearing user
	// message reaches history to re-impose the audio requirement on every
	// later turn of this session. The reason rides the announced notice.
	var audioRefusal string
	if len(audio) > 0 {
		if reason := t.sessionAudioRefusal(ctx, sess); reason != "" {
			audioRefusal = reason
			audio = nil
			droppedContentKinds = appendDroppedKind(droppedContentKinds, string(libacp.ContentKindAudio))
		}
	}

	if input == "" && len(images) == 0 && len(audio) == 0 {
		// An audio-only prompt whose audio the capability gate refused is no
		// operator mistake: the refusal is the answer, announced into the
		// conversation and carried on the error — never a bare "empty prompt".
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

	if name, args, ok := parseCommand(input); ok {
		// A slash command is a text verb; an attached image or audio clip has
		// no meaning here, so it's recorded as dropped rather than silently
		// discarded.
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

	// An input shaped like a command whose name is unknown is answered here
	// rather than forwarded to the model — a mistyped command has one exact
	// answer. Native driver only: an external session's commands belong to
	// its downstream agent.
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

	// When serve wires a native-turn Registry, the turn runs off this
	// connection (see native_turn.go) so a client drop doesn't cancel it.
	// stdio `contenox acp` leaves NativeTurns nil and uses the connection-
	// bound turn below.
	if t.deps.NativeTurns != nil {
		return d.promptViaRegistry(ctx, req, sess, input, images, audio, audioRefusal, droppedContentKinds)
	}

	// Announced before the turn runs, so the operator learns the attachment was
	// discarded ahead of the answer that ignores it. The survival path announces
	// it from inside runNativeTurn for the same reason; the two are exclusive.
	announceDroppedContent(ctx, req.SessionID, droppedContentKinds, audioRefusal, t.sendUpdate)

	promptCtx := libtracker.WithNewRequestID(ctx)
	reqID, _ := promptCtx.Value(libtracker.ContextKeyRequestID).(string)

	// Registered so session/cancel, a session Close/Delete, or a connection
	// drop can abort the running chain; cancelling yields context.Canceled,
	// resolved below as StopReasonCancelled.
	promptCtx, cancelPrompt := context.WithCancel(promptCtx)
	promptReg := t.registerPromptCancel(req.SessionID, cancelPrompt)
	defer func() {
		t.unregisterPromptCancel(req.SessionID, promptReg)
		cancelPrompt()
	}()

	// Per-session HITL policy rides the request context (serve runs one
	// shared hitlservice behind every session) so two concurrent sessions can
	// gate independently; a defaulting session injects nothing.
	if policyName := t.resolveSessionHITLPolicy(sess); policyName != "" {
		promptCtx = hitlservice.WithPolicyName(promptCtx, policyName)
	}
	// The session's own workspace root, so a run that parks on an approval
	// and resumes later — in any process — resolves a relative local_fs/git/jq
	// path exactly as this live call would, not against the resumer's cwd.
	promptCtx = vfs.WithSessionCwd(promptCtx, sess.Cwd)

	// A dispatched unit's mission id, workdir, and compute-resolution bounds
	// ride the same context so its mission tools and model resolution are
	// scoped to this mission. Empty for a chat-mode session.
	if sess.MissionID != "" {
		promptCtx = missiontools.WithMissionID(promptCtx, sess.MissionID)
		promptCtx = missiontools.WithWorkdir(promptCtx, sess.Cwd)
		promptCtx = llmrepo.WithResolutionBounds(promptCtx, sess.resolutionBounds())
	}
	// A session that fired missions carries its own id in, unlocking the
	// supervisor tools so this turn can answer what a unit asks.
	if sess.FiredMissions && sess.InternalSessionID != "" {
		promptCtx = missiontools.WithParentSessionID(promptCtx, sess.InternalSessionID)
	}

	rawCh := make(chan []byte, 64)
	bus := t.deps.Engine.Bus
	if bus != nil && reqID != "" {
		sub, err := bus.Stream(promptCtx, taskengine.TaskEventRequestSubject(reqID), rawCh)
		if err != nil {
			// The prompt still runs without incremental updates; report why.
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

	// The session's effective token budget is the context window; fall back
	// to the model's cap so indicators and engine shifting agree.
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
		// Only context.Canceled is a real cancellation. DeadlineExceeded is a
		// failure the client must see: agentservice.InferStopReason maps both
		// to StopCancelled, so trusting resp.StopReason here would let a hard
		// failure masquerade as a silent cancel.
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
	// A cancelled turn must resolve as cancelled even if the engine absorbed
	// it via a recovery task and returned a "successful" partial result.
	if errors.Is(promptCtx.Err(), context.Canceled) {
		stopReason = libacp.StopReasonCancelled
	}
	suspended := resp.StopReason == agentservice.StopSuspended && stopReason != libacp.StopReasonCancelled
	// Push updatedAt (and the derived title, mirroring session/list's
	// sessionListTitle) so clients notice activity without a re-list.
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
		// ACP has no "suspended" reason and inventing a token would break a
		// client that decodes stopReason as a closed enum, so the spec field
		// stays end_turn. It is never the whole answer: the caller pairs it
		// with suspensionMeta and suspensionNotice, which name the approval
		// the turn is parked on. See stopReasonSuspended.
		return libacp.StopReasonEndTurn
	case agentservice.StopFailed:
		// Same treatment as StopSuspended, and never max_turn_requests: the
		// recovery handler did answer, so the turn ended, and the cause travels
		// through recoveredFailureMeta rather than through a budget token that
		// would send the operator to /clear. See stopReasonFailed.
		return libacp.StopReasonEndTurn
	}
	return libacp.StopReasonEndTurn
}
