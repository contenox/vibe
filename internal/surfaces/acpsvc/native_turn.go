package acpsvc

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/contenox/contenox/internal/kernel/nativeturn"
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

// This file is the acpsvc half of the native-turn survival layer: the
// nativeturn.Registry owns the in-flight turn on a serve-rooted context (see
// prompt.go's nativeDriver for the turn body); nativeTurnViewer bridges its
// fan-out to one connection's WebSocket, and nativeEventTranslator converts
// engine bus events to session/update notifications, running off the
// connection so a drop cannot stop it.

// nativeTurnViewer is one connection's view onto a native turn, writing its
// replayed backlog and live stream to this connection's WebSocket. Created
// per prompt/reattach with a unique id, so a reconnect is a distinct viewer.
type nativeTurnViewer struct {
	t   *Transport
	id  string
	sid libacp.SessionID
}

// newNativeTurnViewer builds a viewer for sid on t with a fresh per-attachment id.
func newNativeTurnViewer(t *Transport, sid libacp.SessionID) *nativeTurnViewer {
	return &nativeTurnViewer{t: t, id: newSessionID("native-view"), sid: sid}
}

// ID is the nativeturn.Viewer id: this viewer's per-attachment identity.
func (v *nativeTurnViewer) ID() string { return v.id }

// Deliver relays one turn event onto this connection's WebSocket. A tool-call
// card whose approval is pending on this connection is suppressed (see
// Transport.isPermissionPending); the open dialog stands in for it. Never
// blocks the turn beyond the WebSocket write.
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
// session/update notifications for one turn, mirroring Transport.publishEvent
// but emitting through the turn (journal + all viewers, surviving a drop)
// with turn-local tool-call sequencing state instead of connection state.
// Runs on a single goroutine draining one bus subscription, so seq/open need
// no lock. Permission-pending suppression is deferred to each viewer.
type nativeEventTranslator struct {
	emit func(ctx context.Context, n libacp.SessionNotification)
	// tokenSizeFallback mirrors the fallback publishEvent reads from the live
	// sessionEntry, for a token-usage event with no size of its own.
	tokenSizeFallback func() int
	// seq/open are turn-local tool-call invocation counters (see
	// resolveToolCallWireID); single-goroutine, so unlocked.
	seq  map[string]int
	open map[string]int
}

// newNativeEventTranslator builds a translator emitting through emit, with the given
// per-turn token-budget fallback.
func newNativeEventTranslator(emit func(ctx context.Context, n libacp.SessionNotification), tokenSizeFallback func() int) *nativeEventTranslator {
	return &nativeEventTranslator{
		emit:              emit,
		tokenSizeFallback: tokenSizeFallback,
		seq:               make(map[string]int),
		open:              make(map[string]int),
	}
}

// publish translates one raw bus payload for sid and emits the resulting
// notification(s), mirroring Transport.publishEvent case-for-case; an
// unparseable payload is dropped.
func (tr *nativeEventTranslator) publish(ctx context.Context, sid libacp.SessionID, payload []byte) {
	var ev taskengine.TaskEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}
	switch ev.Kind {
	case taskengine.TaskEventStepChunk:
		// Only assistant-prose handlers reach the transcript.
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
		// Consumed without a wire notification: the approval card the
		// permission flow already rendered IS the suspension UI — the run is
		// checkpointed and the verdict on that card resumes it. A second
		// frame here would double-render one pending decision.
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
		// A mission_plan call also projects a full-snapshot ACP plan update.
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

// promptViaRegistry runs (or joins) sess's turn on the native-turn Registry
// and relays it to this connection as a viewer. The turn executes on the
// Registry's own serve-rooted context; this method only watches it, detaching
// without cancelling on a connection drop (the turn's completion is
// journaled for the reattaching client). session/cancel is the only real
// cancel, reaching the turn through Transport.Cancel -> Registry.Cancel
// independent of this connection — hence keying the drop case on connCtx
// rather than the request context.
func (d *nativeDriver) promptViaRegistry(ctx context.Context, req libacp.PromptRequest, sess *sessionEntry, input string, images []taskengine.ImagePart, droppedContentKinds []string) (libacp.PromptResponse, error) {
	t := d.t
	_ = ctx // the turn owns its own serve-rooted context

	viewer := newNativeTurnViewer(t, req.SessionID)
	turnFn := func(turnCtx context.Context, emit func(context.Context, libacp.SessionNotification)) nativeturn.Result {
		return d.runNativeTurn(turnCtx, req, sess, input, images, droppedContentKinds, emit)
	}

	turn, _, err := t.deps.NativeTurns.Start(req.SessionID, turnFn, viewer)
	if err != nil {
		// Registry closed (serve shutting down): resolve as a clean cancel.
		return libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, nil
	}

	select {
	case <-turn.Done():
		turn.Detach(viewer.ID())
		return nativeResultToResponse(turn.Result())
	case <-t.connCtx.Done():
		// Connection dropped; the turn keeps running and its result is
		// journaled for the reattaching client.
		turn.Detach(viewer.ID())
		return libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, nil
	}
}

// nativeResultToResponse maps a completed turn's Result onto the ACP prompt
// response: clean end/cancellation resolve with the stop reason and no
// error; a genuine failure resolves as an InternalError.
func nativeResultToResponse(res nativeturn.Result) (libacp.PromptResponse, error) {
	if res.Err != nil {
		return libacp.PromptResponse{StopReason: res.StopReason}, libacp.InternalError(res.Err.Error())
	}
	return libacp.PromptResponse{StopReason: res.StopReason}, nil
}

// runNativeTurn is the survival-path turn body: the same chain-execution flow
// as nativeDriver.Prompt, lifted onto the serve-rooted turnCtx. It differs
// only where survival requires it: events emit into the turn's journal +
// viewers (not one connection); cancellation is keyed on the turn context (a
// connection drop never reaches here); the post-turn SessionInfo is
// journaled rather than sent via AfterResponse; and it owns its own
// turn-scoped tracker span, since the turn outlives the connection that
// started it.
func (d *nativeDriver) runNativeTurn(turnCtx context.Context, req libacp.PromptRequest, sess *sessionEntry, input string, images []taskengine.ImagePart, droppedContentKinds []string, emit func(context.Context, libacp.SessionNotification)) nativeturn.Result {
	t := d.t

	turnCtx = libtracker.WithNewRequestID(turnCtx)
	reqID, _ := turnCtx.Value(libtracker.ContextKeyRequestID).(string)

	reportErr, reportChange, end := t.tracker().Start(turnCtx, "prompt", "acp_session",
		"session_id", string(req.SessionID), "prompt_blocks", len(req.Prompt))
	defer end()

	// Same per-session HITL/mission context injection as prompt.go, riding
	// turnCtx instead of the request ctx.
	if policyName := t.resolveSessionHITLPolicy(sess); policyName != "" {
		turnCtx = hitlservice.WithPolicyName(turnCtx, policyName)
	}
	// The session's own workspace root — see prompt.go's identical line for
	// why this rides every turn, not just mission units.
	turnCtx = vfs.WithSessionCwd(turnCtx, sess.Cwd)
	if sess.MissionID != "" {
		turnCtx = missiontools.WithMissionID(turnCtx, sess.MissionID)
		turnCtx = missiontools.WithWorkdir(turnCtx, sess.Cwd)
		turnCtx = llmrepo.WithResolutionBounds(turnCtx, sess.resolutionBounds())
	}
	if sess.FiredMissions && sess.InternalSessionID != "" {
		turnCtx = missiontools.WithParentSessionID(turnCtx, sess.InternalSessionID)
	}

	// Translates each event into a session/update emitted through the turn
	// (journal + viewers); runs off the connection, so a drop can't stop it.
	translator := newNativeEventTranslator(emit, sess.effectiveTokenLimit)
	rawCh := make(chan []byte, 64)
	var drainEvents func()
	if bus := t.deps.Engine.Bus; bus != nil && reqID != "" {
		sub, err := bus.Stream(turnCtx, taskengine.TaskEventRequestSubject(reqID), rawCh)
		if err != nil {
			// The turn still runs without incremental updates; report why.
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
			return nativeturn.Result{Err: err}
		}
	}

	// Same context-window resolution as prompt.go.
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
		Chain:          t.deps.ChainRegistry.Default(),
		TemplateVars:   templateVars,
		ToolsAllowlist: toolsAllowlist,
		ContextLength:  contextLen,
	})

	// Drain every translated update into the journal before computing the
	// outcome, so the terminal SessionInfo update is ordered strictly last.
	if drainEvents != nil {
		drainEvents()
	}

	if err != nil {
		// Keyed on the turn context only: a connection drop never cancels the
		// turn, so the connection path's ctx.Err() branch doesn't apply here.
		// context.Canceled = session/cancel or grace-expiry; DeadlineExceeded
		// = the hard turn deadline, a failure the client must see.
		cancelled := errors.Is(err, context.Canceled) || errors.Is(turnCtx.Err(), context.Canceled)
		if cancelled {
			reportChange(string(req.SessionID), map[string]any{
				"stop_reason":           string(libacp.StopReasonCancelled),
				"request_id":            reqID,
				"dropped_content_kinds": droppedContentKinds,
			})
			return nativeturn.Result{StopReason: libacp.StopReasonCancelled}
		}
		reportErr(err)
		if resp != nil {
			return nativeturn.Result{StopReason: mapStopReason(resp.StopReason), Err: err}
		}
		return nativeturn.Result{Err: err}
	}

	stopReason := mapStopReason(resp.StopReason)
	if errors.Is(turnCtx.Err(), context.Canceled) {
		stopReason = libacp.StopReasonCancelled
	}

	// Journaled as the last event, so a reattaching client finds the
	// freshened tab/sidebar label too.
	d.emitSessionInfo(turnCtx, req.SessionID, sess, emit)

	reportChange(string(req.SessionID), map[string]any{
		"stop_reason":           string(stopReason),
		"request_id":            reqID,
		"dropped_content_kinds": droppedContentKinds,
	})
	// A suspended chain ends this goroutine by design: the run is
	// checkpointed, and answering the approval resumes it. The Registry
	// surfaces it as StateSuspended until reaped.
	return nativeturn.Result{
		StopReason: stopReason,
		Suspended:  resp != nil && resp.StopReason == agentservice.StopSuspended,
	}
}

// emitSessionInfo journals the post-turn session_info update: the survival
// counterpart of the AfterResponse push in the connection-bound Prompt.
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
// any, on reconnect. Called after the durable transcript is re-established —
// the transcript covers persisted turns, the journal covers the one still in
// flight, so they never overlap. The reattached viewer has no prompt RPC
// awaiting it, so it self-detaches when the turn ends or the connection
// drops. No-op with no Registry, for external sessions, or no turn in flight.
func (t *Transport) reattachNativeTurn(ctx context.Context, sid libacp.SessionID) {
	if t.deps.NativeTurns == nil {
		return
	}
	viewer := newNativeTurnViewer(t, sid)
	turn, ok, err := t.deps.NativeTurns.AttachIfRunning(ctx, sid, viewer)
	if err != nil || !ok {
		return
	}
	go func() {
		select {
		case <-t.connCtx.Done():
		case <-turn.Done():
		}
		turn.Detach(viewer.ID())
	}()
}
