package acpsvc

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/contenox/contenox/internal/kernel/nativeturn"
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

// nativeTurnViewer is one connection's view onto a native turn. Created per
// prompt/reattach with a unique id, so a reconnect is a distinct viewer.
type nativeTurnViewer struct {
	t   *Transport
	id  string
	sid libacp.SessionID
}

func newNativeTurnViewer(t *Transport, sid libacp.SessionID) *nativeTurnViewer {
	return &nativeTurnViewer{t: t, id: newSessionID("native-view"), sid: sid}
}

func (v *nativeTurnViewer) ID() string { return v.id }

// Deliver relays one turn event onto this connection's WebSocket, suppressing a
// tool-call card whose approval dialog is already open here.
func (v *nativeTurnViewer) Deliver(ctx context.Context, ev nativeturn.Event) error {
	n := ev.Update
	if id := n.Update.ToolCallID; id != "" &&
		(n.Update.SessionUpdate == libacp.SessionUpdateToolCall || n.Update.SessionUpdate == libacp.SessionUpdateToolCallUpdate) &&
		v.t.isPermissionPending(n.SessionID, id) {
		return nil
	}
	v.t.sendUpdate(ctx, n)
	return nil
}

// nativeEventTranslator converts the task engine's bus events into ACP
// session/update notifications for one turn. It runs on a single goroutine
// draining one bus subscription, so seq/open need no lock.
type nativeEventTranslator struct {
	emit              func(ctx context.Context, n libacp.SessionNotification)
	tokenSizeFallback func() int
	seq               map[string]int
	open              map[string]int
}

func newNativeEventTranslator(emit func(ctx context.Context, n libacp.SessionNotification), tokenSizeFallback func() int) *nativeEventTranslator {
	return &nativeEventTranslator{
		emit:              emit,
		tokenSizeFallback: tokenSizeFallback,
		seq:               make(map[string]int),
		open:              make(map[string]int),
	}
}

func (tr *nativeEventTranslator) publish(ctx context.Context, sid libacp.SessionID, payload []byte) {
	var ev taskengine.TaskEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}
	switch ev.Kind {
	case taskengine.TaskEventStepChunk:
		if !taskengine.IsAssistantProseHandler(ev.TaskHandler) {
			return
		}
		if ev.Content != "" {
			tr.emit(ctx, libacp.SessionNotification{SessionID: sid, Update: libacp.NewAgentMessageChunk(ev.Content)})
		}
		if ev.Thinking != "" {
			tr.emit(ctx, libacp.SessionNotification{SessionID: sid, Update: libacp.NewAgentThoughtChunk(ev.Thinking)})
		}
	case taskengine.TaskEventStepStreamEnd:
		// ACP end-of-stream is implicit; usage comes from token_usage.
	case taskengine.TaskEventChainSuspended:
		// No wire notification: the approval card already rendered is the
		// suspension UI.
	case taskengine.TaskEventStepStarted:
		if taskengine.IsToolBearingHandler(ev.TaskHandler) {
			return
		}
		tr.emit(ctx, toolCallNotification(sid, ev, libacp.ToolCallStatusInProgress))
	case taskengine.TaskEventStepCompleted:
		if taskengine.IsToolBearingHandler(ev.TaskHandler) {
			return
		}
		tr.emit(ctx, toolCallNotification(sid, ev, libacp.ToolCallStatusCompleted))
	case taskengine.TaskEventStepFailed:
		if taskengine.IsToolBearingHandler(ev.TaskHandler) {
			return
		}
		tr.emit(ctx, toolCallNotification(sid, ev, libacp.ToolCallStatusFailed))
	case taskengine.TaskEventPrint:
		if ev.Content != "" {
			tr.emit(ctx, libacp.SessionNotification{SessionID: sid, Update: libacp.NewAgentMessageChunk(ev.Content)})
		}
	case taskengine.TaskEventToolCallPending:
		id := resolveToolCallWireID(tr.seq, tr.open, sid, ev, false)
		tr.emit(ctx, toolCallPendingNotification(sid, ev, id))
	case taskengine.TaskEventToolCall:
		id := resolveToolCallWireID(tr.seq, tr.open, sid, ev, true)
		tr.emit(ctx, toolCallUpdateNotification(sid, ev, id))
		if note, ok := planUpdateNotification(sid, ev); ok {
			tr.emit(ctx, note)
		}
	case taskengine.TaskEventTokenUsage:
		used := ev.TokenUsed
		size := ev.TokenSize
		if size <= 0 && tr.tokenSizeFallback != nil {
			if eff := tr.tokenSizeFallback(); eff > 0 {
				size = eff
			}
		}
		if size > 0 || used > 0 {
			tr.emit(ctx, libacp.SessionNotification{
				SessionID: sid,
				Update: libacp.SessionUpdate{
					SessionUpdate: libacp.SessionUpdateUsageUpdate,
					Used:          used,
					Size:          size,
				},
			})
		}
	}
}

// promptViaRegistry runs (or joins) sess's turn on the native-turn Registry and
// relays it to this connection as a viewer. The turn executes on the Registry's
// own serve-rooted context; this method only watches it, detaching without
// cancelling on a connection drop.
func (d *nativeDriver) promptViaRegistry(ctx context.Context, req libacp.PromptRequest, sess *sessionEntry, input string, images []taskengine.ImagePart, audio []taskengine.AudioPart, audioRefusal string, droppedContentKinds []string) (libacp.PromptResponse, error) {
	t := d.t
	_ = ctx // the turn owns its own serve-rooted context

	viewer := newNativeTurnViewer(t, req.SessionID)
	turnFn := func(turnCtx context.Context, emit func(context.Context, libacp.SessionNotification)) nativeturn.Result {
		return d.runNativeTurn(turnCtx, req, sess, input, images, audio, audioRefusal, droppedContentKinds, emit)
	}

	turn, _, err := t.deps.NativeTurns.Start(req.SessionID, turnFn, viewer)
	if err != nil {
		// Registry closed (serve shutting down): resolve as a clean cancel.
		return libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, nil
	}
	t.markNativeViewing(req.SessionID)
	defer t.unmarkNativeViewing(req.SessionID)

	select {
	case <-turn.Done():
		turn.Detach(viewer.ID())
		return nativeResultToResponse(turn.Result())
	case <-t.connCtx.Done():
		// The turn keeps running; its result is journaled for the reattaching client.
		turn.Detach(viewer.ID())
		return libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, nil
	}
}

// nativeResultToResponse maps a completed turn's Result onto the ACP prompt
// response.
func nativeResultToResponse(res nativeturn.Result) (libacp.PromptResponse, error) {
	if res.Err != nil {
		return libacp.PromptResponse{StopReason: res.StopReason}, libacp.InternalError(res.Err.Error())
	}
	resp := libacp.PromptResponse{StopReason: res.StopReason}
	if res.Suspended {
		resp.Meta = suspensionMeta(res.ApprovalID)
	}
	return withDroppedContentMeta(resp, res.DroppedContentKinds), nil
}

// runNativeTurn is the survival-path turn body: the same chain-execution flow as
// nativeDriver.Prompt, lifted onto the serve-rooted turnCtx.
func (d *nativeDriver) runNativeTurn(turnCtx context.Context, req libacp.PromptRequest, sess *sessionEntry, input string, images []taskengine.ImagePart, audio []taskengine.AudioPart, audioRefusal string, droppedContentKinds []string, emit func(context.Context, libacp.SessionNotification)) nativeturn.Result {
	t := d.t

	turnCtx = libtracker.WithNewRequestID(turnCtx)
	reqID, _ := turnCtx.Value(libtracker.ContextKeyRequestID).(string)

	reportErr, reportChange, end := t.tracker().Start(turnCtx, "prompt", "acp_session",
		"session_id", string(req.SessionID), "prompt_blocks", len(req.Prompt))
	defer end()

	// Journaled ahead of anything the turn produces, so a reattaching client
	// reads the loss in the order the live one saw it.
	announceDroppedContent(turnCtx, req.SessionID, droppedContentKinds, audioRefusal, emit)

	// Same per-session HITL/mission context injection as prompt.go, riding
	// turnCtx instead of the request ctx.
	if policyName := t.resolveSessionHITLPolicy(sess); policyName != "" {
		turnCtx = hitlservice.WithPolicyName(turnCtx, policyName)
	}
	turnCtx = vfs.WithSessionCwd(turnCtx, sess.Cwd)
	if sess.MissionID != "" {
		turnCtx = missiontools.WithMissionID(turnCtx, sess.MissionID)
		turnCtx = missiontools.WithWorkdir(turnCtx, sess.Cwd)
		turnCtx = llmrepo.WithResolutionBounds(turnCtx, sess.resolutionBounds())
	}
	if sess.MissionID == "" && sess.InternalSessionID != "" && t.hasMissionCapability() {
		turnCtx = missiontools.WithParentSessionID(turnCtx, sess.InternalSessionID)
	}

	translator := newNativeEventTranslator(emit, sess.effectiveTokenLimit)
	rawCh := make(chan []byte, 64)
	var drainEvents func()
	if bus := t.deps.Engine.Bus; bus != nil && reqID != "" {
		sub, err := bus.Stream(turnCtx, taskengine.TaskEventRequestSubject(reqID), rawCh)
		if err != nil {
			subErr, _, subEnd := t.tracker().Start(turnCtx, "subscribe", "acp_event_stream",
				"session_id", string(req.SessionID), "request_id", reqID)
			subErr(err)
			subEnd()
		} else {
			translateDone := make(chan struct{})
			go func() {
				defer close(translateDone)
				for payload := range rawCh {
					translator.publish(turnCtx, req.SessionID, payload)
				}
			}()
			drainEvents = func() {
				_ = sub.Unsubscribe()
				close(rawCh)
				<-translateDone
			}
		}
	}

	templateVars := t.chainTemplateVars(sess)
	templateVars["think"] = sess.think()
	var toolsAllowlist []string
	if t.deps.DB != nil {
		var err error
		toolsAllowlist, err = t.runtimeToolsAllowlist(turnCtx, runtimetypes.New(t.deps.DB.WithoutTransaction()), sess.McpServerNames)
		if err != nil {
			if drainEvents != nil {
				drainEvents()
			}
			reportErr(err)
			return nativeturn.Result{Err: err, DroppedContentKinds: droppedContentKinds}
		}
	}

	contextLen := sess.effectiveTokenLimit()
	if contextLen == 0 {
		currentModel := sess.modelOrDefault(t.model())
		for _, state := range t.runtimeStates(turnCtx) {
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
			for _, state := range t.runtimeStates(turnCtx) {
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

	resp, err := d.agent.Prompt(turnCtx, agentservice.PromptRequest{
		SessionID:      sess.InternalSessionID,
		Input:          input,
		Images:         images,
		Audio:          audio,
		Chain:          t.deps.ChainRegistry.Default(),
		TemplateVars:   templateVars,
		ToolsAllowlist: toolsAllowlist,
		ContextLength:  contextLen,
	})

	// Drain before computing the outcome, so the terminal SessionInfo is last.
	if drainEvents != nil {
		drainEvents()
	}

	if err != nil {
		// Keyed on the turn context only: a connection drop never cancels the turn.
		cancelled := errors.Is(err, context.Canceled) || errors.Is(turnCtx.Err(), context.Canceled)
		if cancelled {
			reportChange(string(req.SessionID), map[string]any{
				"stop_reason":           string(libacp.StopReasonCancelled),
				"request_id":            reqID,
				"dropped_content_kinds": droppedContentKinds,
			})
			return nativeturn.Result{StopReason: libacp.StopReasonCancelled, DroppedContentKinds: droppedContentKinds}
		}
		reportErr(err)
		if resp != nil {
			return nativeturn.Result{StopReason: mapStopReason(resp.StopReason), Err: err, DroppedContentKinds: droppedContentKinds}
		}
		return nativeturn.Result{Err: err, DroppedContentKinds: droppedContentKinds}
	}

	stopReason := mapStopReason(resp.StopReason)
	if errors.Is(turnCtx.Err(), context.Canceled) {
		stopReason = libacp.StopReasonCancelled
	}
	suspended := resp != nil && resp.StopReason == agentservice.StopSuspended && stopReason != libacp.StopReasonCancelled
	approvalID := ""
	if suspended {
		approvalID = resp.SuspendedApprovalID
		emit(turnCtx, libacp.SessionNotification{
			SessionID: req.SessionID,
			Update:    suspensionNotice(approvalID),
		})
	}

	d.emitSessionInfo(turnCtx, req.SessionID, sess, emit)

	loggedReason := string(stopReason)
	if suspended {
		loggedReason = stopReasonSuspended
	}
	reportChange(string(req.SessionID), map[string]any{
		"stop_reason":           loggedReason,
		"request_id":            reqID,
		"dropped_content_kinds": droppedContentKinds,
	})
	return nativeturn.Result{
		StopReason:          stopReason,
		Suspended:           suspended,
		ApprovalID:          approvalID,
		DroppedContentKinds: droppedContentKinds,
	}
}

func (d *nativeDriver) emitSessionInfo(ctx context.Context, sid libacp.SessionID, sess *sessionEntry, emit func(context.Context, libacp.SessionNotification)) {
	update := libacp.SessionUpdate{
		SessionUpdate: libacp.SessionUpdateSessionInfo,
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if title := d.t.sessionInfoTitle(ctx, sess.InternalSessionID); title != "" {
		update.Title = title
	}
	emit(ctx, libacp.SessionNotification{SessionID: sid, Update: update})
}

// reattachNativeTurn joins this connection to sid's in-flight native turn, if
// any, on reconnect. The reattached viewer self-detaches when the turn ends or
// the connection drops.
func (t *Transport) reattachNativeTurn(ctx context.Context, sid libacp.SessionID) {
	if t.deps.NativeTurns == nil {
		return
	}
	viewer := newNativeTurnViewer(t, sid)
	turn, ok, err := t.deps.NativeTurns.AttachIfRunning(ctx, sid, viewer)
	if err != nil || !ok {
		return
	}
	t.markNativeViewing(sid)
	go func() {
		select {
		case <-t.connCtx.Done():
		case <-turn.Done():
		}
		turn.Detach(viewer.ID())
		t.unmarkNativeViewing(sid)
	}()
}
