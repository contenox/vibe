package acpsvc

import (
	"context"
	"errors"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentservice"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/missiontools"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
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

	// Image blocks are pulled out FIRST: FlattenContent is a lossy text
	// projection that drops them, and vision rides the user Message's Images —
	// see extractImageParts. An image-only prompt (empty text, ≥1 image) is a
	// valid turn: "what is this?" asked by attachment alone.
	images, textBlocks := extractImageParts(req.Prompt)
	input, droppedContentKinds := libacp.FlattenContent(textBlocks)
	if input == "" && len(images) == 0 {
		err := libacp.NewError(libacp.ErrInvalidParams, "empty prompt")
		reportErr(err)
		return libacp.PromptResponse{}, err
	}

	if name, args, ok := parseCommand(input); ok {
		// Slash commands are text verbs; an image attached to one has no
		// meaning. Record it as dropped rather than silently discarding.
		if len(images) > 0 {
			droppedContentKinds = append(droppedContentKinds, string(libacp.ContentKindImage))
		}
		cmdCtx := libtracker.WithNewRequestID(ctx)
		return t.dispatchCommand(cmdCtx, req.SessionID, sess, name, args)
	}

	// Survival path: when serve wires a native-turn Registry, the turn runs OFF this
	// connection (on the Registry's serve-rooted context) so a client drop no longer
	// cancels it — this Transport is a thin viewer. See runtime/nativeturn and
	// native_turn.go. The stdio `contenox acp` path leaves NativeTurns nil and keeps
	// the connection-bound turn below, byte-for-byte the historical behavior.
	if t.deps.NativeTurns != nil {
		return d.promptViaRegistry(ctx, req, sess, input, images, droppedContentKinds)
	}

	promptCtx := libtracker.WithNewRequestID(ctx)
	reqID, _ := promptCtx.Value(libtracker.ContextKeyRequestID).(string)

	// Make this turn cancellable and register it so session/cancel (Transport.Cancel),
	// a session Close/Delete, or a connection drop can abort the running chain.
	// promptCtx already inherits libacp's connection-level prompt context, but the
	// server owns cancellation here rather than relying solely on that. The
	// deferred unregister+cancel cleans up on turn end; cancelling produces
	// context.Canceled, which the error path below resolves as StopReasonCancelled.
	promptCtx, cancelPrompt := context.WithCancel(promptCtx)
	promptReg := t.registerPromptCancel(req.SessionID, cancelPrompt)
	defer func() {
		t.unregisterPromptCancel(req.SessionID, promptReg)
		cancelPrompt()
	}()

	// Gate this turn's tool calls under THIS session's chosen HITL policy. serve
	// runs one shared engine (one hitlservice) behind every ACP session, so a
	// concrete per-session selection must ride the request context: WithPolicyName
	// makes hitlservice.Evaluate prefer it over the process-global
	// cli.hitl-policy-name KV, letting two concurrent sessions gate independently.
	// A defaulting session resolves to "" and injects nothing, leaving the global-
	// KV/fallback chain intact (byte-identical to pre-per-session behavior). The
	// context threads synchronously prompt -> agentservice -> taskengine tool
	// gating -> HITLWrapper.Exec -> hitlservice.Evaluate.
	if policyName := t.resolveSessionHITLPolicy(sess); policyName != "" {
		promptCtx = hitlservice.WithPolicyName(promptCtx, policyName)
	}

	// If this session is a dispatched unit on a mission, bind its mission id onto
	// the turn context so this turn's mission_report / mission_ask_attention tools
	// report against THIS mission — the per-mission grant, enforced at
	// construction rather than asserted by the agent. The same synchronous
	// prompt -> agentservice -> taskengine tool path WithPolicyName rides carries
	// it. Empty for a chat-mode session, which injects nothing and whose mission
	// tools therefore resolve to nothing.
	if sess.MissionID != "" {
		promptCtx = missiontools.WithMissionID(promptCtx, sess.MissionID)
		// Bind the unit's workdir so the conclusion verification gate can stat
		// relative artifact refs a result report claims (absolute refs verify
		// without it).
		promptCtx = missiontools.WithWorkdir(promptCtx, sess.Cwd)
	}
	// The other end of the same relationship: a session that FIRED missions carries
	// its own id in, unlocking the supervisor tools (see missiontools.WithParentSessionID)
	// so this turn can look at what it dispatched and answer what a unit asks. A
	// session that fired nothing injects nothing and is offered no mission tools.
	if sess.FiredMissions && sess.InternalSessionID != "" {
		promptCtx = missiontools.WithParentSessionID(promptCtx, sess.InternalSessionID)
	}

	rawCh := make(chan []byte, 64)
	bus := t.deps.Engine.Bus
	if bus != nil && reqID != "" {
		sub, err := bus.Stream(promptCtx, taskengine.TaskEventRequestSubject(reqID), rawCh)
		if err != nil {
			// The prompt still runs, but the client gets no incremental
			// updates. Surface why instead of silently degrading.
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

	// Use the session's effective token budget (chain token_limit or override set via config)
	// as the context window for this prompt. This is clamped to model cap (if known).
	// This makes indicators (which now use the session budget as "size") and engine shifting
	// consistent with the value the user sees and switches.
	contextLen := sess.effectiveTokenLimit()
	if contextLen == 0 {
		// fallback to model cap (for indicator size) if no explicit session budget
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
		// Distinguish a genuine user cancellation from an execution failure that
		// merely SURFACED as a timeout. Only context.Canceled is a cancellation
		// (the client sent session/cancel, or the connection/parent context was
		// torn down). context.DeadlineExceeded — e.g. modeld refusing to load a
		// model, or waiting on a busy single GPU slot until an inner LLM call
		// deadlines — is a FAILURE the client must SEE, not a silent clean stop.
		// agentservice.InferStopReason maps BOTH to StopCancelled, so trusting
		// resp.StopReason (or a bare promptCtx.Err()) here would let a hard
		// failure masquerade as a cancel and vanish from the UI: the client
		// resolves the prompt with no error, drops its "prompting" state, and
		// shows nothing. Key the silent-cancel path on context.Canceled only.
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
	// A cancelled turn MUST resolve with the cancelled stop reason even when
	// the engine absorbed the cancellation and handed back a "successful"
	// partial result (e.g. via a recovery task) — the client sent
	// session/cancel or $/cancel_request and judges conformance by this field.
	// Keyed on context.Canceled specifically (not any ctx error): a deadline
	// that fired against a salvaged result is a timeout, not a user cancel.
	if errors.Is(promptCtx.Err(), context.Canceled) {
		stopReason = libacp.StopReasonCancelled
	}
	// Session pickers key freshness off updatedAt; push it after the turn so
	// clients don't need to re-list to notice activity. Push the derived title
	// alongside it: a session created this connection carried NO title in its
	// session/new SessionInfo, so without this the client's tab/sidebar label
	// is stuck on the raw-id fallback ("Sitzung acp-XXXX") until a full
	// session/list re-list (only on reconnect). Deriving from the first user
	// message here mirrors session/list's sessionListTitle, so the live push
	// and the re-list agree.
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
		// ACP has no "suspended" stop reason: from the client's view the turn
		// ended cleanly, with the still-open permission card telling the user
		// what the run is waiting for. Answering the approval resumes the run
		// server-side (S6) and the continuation reaches the client as a fresh
		// turn's updates.
		return libacp.StopReasonEndTurn
	}
	return libacp.StopReasonEndTurn
}
