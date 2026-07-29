package acpsvc

import (
	"context"
	"errors"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/models/llmrepo"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
)

// Prompt resolves the session and dispatches the turn to its driver. The driver
// (native chain engine vs. registered downstream ACP agent) owns everything the
// turn does — there is no native-vs-external branch here.
func (t *Transport) Prompt(ctx context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
	sess, ok := t.sessionFor(req.SessionID)
	if !ok {
		reportErr, _, end := t.tracker().Start(ctx, "prompt", "acp_session", "session_id", string(req.SessionID), "prompt_blocks", len(req.Prompt))
		defer end()
		err := libacp.NewErrorf(libacp.ErrInvalidParams, "unknown session %q", req.SessionID)
		reportErr(err)
		return libacp.PromptResponse{}, err
	}
	return sess.driver.Prompt(ctx, req, sess)
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
// plus the workspace root when an allowlist is configured).
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
func (d *nativeDriver) Prompt(ctx context.Context, req libacp.PromptRequest, sess *sessionEntry) (libacp.PromptResponse, error) {
	t := d.t
	reportErr, reportChange, end := t.tracker().Start(ctx, "prompt", "acp_session", "session_id", string(req.SessionID), "prompt_blocks", len(req.Prompt))
	defer end()

	if t.deps.ChainRegistry == nil || t.deps.ChainRegistry.Default() == nil {
		err := libacp.InternalError("no chain configured")
		reportErr(err)
		return libacp.PromptResponse{}, err
	}

	// Images are extracted first (see extractImageParts): FlattenContent's
	// lossy text projection would drop them. An image-only prompt is valid.
	images, textBlocks := extractImageParts(req.Prompt)
	input, droppedContentKinds := libacp.FlattenContent(textBlocks)
	if input == "" && len(images) == 0 {
		err := libacp.NewError(libacp.ErrInvalidParams, "empty prompt")
		reportErr(err)
		return libacp.PromptResponse{}, err
	}

	if name, args, ok := parseCommand(input); ok {
		// A slash command is a text verb; an attached image has no meaning
		// here, so it's recorded as dropped rather than silently discarded.
		if len(images) > 0 {
			droppedContentKinds = append(droppedContentKinds, string(libacp.ContentKindImage))
		}
		cmdCtx := libtracker.WithNewRequestID(ctx)
		return t.dispatchCommand(cmdCtx, req.SessionID, sess, name, args)
	}

	// An input shaped like a command whose name is unknown is answered here
	// rather than forwarded to the model — a mistyped command has one exact
	// answer. Native driver only: an external session's commands belong to
	// its downstream agent.
	if name, ok := unknownCommandName(input); ok {
		if len(images) > 0 {
			droppedContentKinds = append(droppedContentKinds, string(libacp.ContentKindImage))
		}
		reportChange(string(req.SessionID), map[string]any{
			"stop_reason":           string(libacp.StopReasonEndTurn),
			"unknown_command":       name,
			"dropped_content_kinds": droppedContentKinds,
		})
		return t.answerUnknownCommand(ctx, req.SessionID, name), nil
	}

	// When serve wires a native-turn Registry, the turn runs off this
	// connection (see native_turn.go) so a client drop doesn't cancel it.
	// stdio `contenox acp` leaves NativeTurns nil and uses the connection-
	// bound turn below.
	if t.deps.NativeTurns != nil {
		return d.promptViaRegistry(ctx, req, sess, input, images, droppedContentKinds)
	}

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
			return libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, nil
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
	reportChange(string(req.SessionID), map[string]any{
		"stop_reason":           string(stopReason),
		"request_id":            reqID,
		"dropped_content_kinds": droppedContentKinds,
	})
	return libacp.PromptResponse{StopReason: stopReason}, nil
}

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
		// ACP has no "suspended" reason; the client sees a clean end turn,
		// with the open permission card signaling what it's waiting on.
		return libacp.StopReasonEndTurn
	}
	return libacp.StopReasonEndTurn
}
