package acpsvc

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/contenox/beam/internal/kernel/nativeturn"
	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentservice"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/missiontools"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
)

// This file is the acpsvc half of the native-turn survival layer (see the
// runtime/nativeturn package doc). The nativeturn.Registry owns the in-flight turn
// on a serve-rooted context; the pieces here connect it to one ACP connection:
//
//   - nativeTurnViewer bridges a turn's fan-out to this connection's WebSocket (the
//     Transport-as-thin-viewer role), applying the per-connection permission-card
//     suppression at delivery.
//   - nativeEventTranslator turns the engine's bus events into session/update
//     notifications the turn emits into its journal + viewers. It is the
//     turn-scoped counterpart of Transport.publishEvent: identical vocabulary, but
//     it runs OFF the connection (so a drop cannot stop it) with turn-local
//     tool-call sequencing state and no per-connection normalization (each viewer
//     normalizes on delivery).
//
// The turn body itself (the chain execution) lives in prompt.go's nativeDriver.

// nativeTurnViewer is one connection's view onto a native turn: it receives the
// turn's replayed backlog and live session/update stream and writes each onto this
// connection's WebSocket. It is created per prompt / per reattach with a unique id,
// so a reconnecting Transport is a DISTINCT viewer and Detach names exactly it —
// mirroring the external path's per-attachment externalBridge viewer id.
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

// Deliver relays one turn event onto this connection's WebSocket. A tool-call card
// whose approval is currently pending on THIS connection is suppressed — the open
// permission dialog stands in for it — reproducing sendToolCallUpdateGuarded at the
// delivery boundary (see Transport.isPermissionPending). The write itself goes
// through sendUpdate, which applies this connection's own tool-call normalization,
// so a freshly-attached viewer replaying the journal rebuilds its display state
// correctly. Never blocks the turn beyond the WebSocket write, matching the external
// bridge's inline-relay behavior.
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

// nativeEventTranslator converts the task engine's per-request bus events into ACP
// session/update notifications for one turn, emitting each into the turn's journal
// and viewer fan-out. It is the survival-path counterpart of Transport.publishEvent
// and keeps that method's exact vocabulary and gating; the two differences are
// structural, both required by survival:
//
//   - it emits through the turn (journal + all viewers) rather than one connection,
//     so the stream survives a drop and replays on reattach, and
//   - its tool-call invocation-sequencing state is TURN-LOCAL (a fresh turn starts a
//     fresh sequence) rather than living on a Transport that can die mid-turn.
//
// It runs on a single goroutine draining one bus subscription, so its seq/open maps
// need no lock. The permission-pending suppression that publishEvent applies inline
// is deferred to each viewer (nativeTurnViewer.Deliver), because it is per-connection
// state and the translation is now connection-independent.
type nativeEventTranslator struct {
	emit func(ctx context.Context, n libacp.SessionNotification)
	// tokenSizeFallback supplies the session's effective token budget when a
	// token-usage event carries no size of its own — the same fallback publishEvent
	// reads from the live sessionEntry.
	tokenSizeFallback func() int
	// seq/open carry the tool-call invocation counters for this turn (see
	// resolveToolCallWireID). Turn-local and single-goroutine, so unlocked.
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
// notification(s). It mirrors Transport.publishEvent case-for-case; an unparseable
// payload is dropped exactly as there.
func (tr *nativeEventTranslator) publish(ctx context.Context, sid libacp.SessionID, payload []byte) {
	var ev taskengine.TaskEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}
	switch ev.Kind {
	case taskengine.TaskEventStepChunk:
		// Only a handler whose streamed output IS assistant narration reaches the
		// transcript (see publishEvent for the rationale).
		if !taskengine.IsAssistantProseHandler(ev.TaskHandler) {
			return
		}
		if ev.Content != "" {
			tr.emit(ctx, libacp.SessionNotification{SessionID: sid, Update: libacp.NewAgentMessageChunk(ev.Content)})
		}
		if ev.Thinking != "" {
			tr.emit(ctx, libacp.SessionNotification{SessionID: sid, Update: libacp.NewAgentThoughtChunk(ev.Thinking)})
		}
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
		// A mission_plan call also projects the stored plan snapshot as a full-
		// snapshot ACP plan update (see publishEvent); a no-op for every other event.
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
// relays it to this connection as a viewer. It is the survival counterpart of the
// connection-bound turn in nativeDriver.Prompt: the turn's chain executes on the
// Registry's serve-rooted, hard-deadline-bounded context, and this method only
// WATCHES it — awaiting completion to resolve the client's prompt RPC, or, on a
// connection drop, detaching the viewer WITHOUT cancelling the turn (its completion
// is journaled for the reattaching client). session/cancel is the only real cancel;
// it reaches the turn through Transport.Cancel -> Registry.Cancel, independent of
// this connection's context, which is exactly why the wait below keys the drop case
// on connCtx (a bare socket close) rather than the request context (which a
// session/cancel also cancels).
func (d *nativeDriver) promptViaRegistry(ctx context.Context, req libacp.PromptRequest, sess *sessionEntry, input string, droppedContentKinds []string) (libacp.PromptResponse, error) {
	t := d.t
	_ = ctx // the turn owns its own serve-rooted context; ctx governed the prompt preamble only.

	viewer := newNativeTurnViewer(t, req.SessionID)
	turnFn := func(turnCtx context.Context, emit func(context.Context, libacp.SessionNotification)) nativeturn.Result {
		return d.runNativeTurn(turnCtx, req, sess, input, droppedContentKinds, emit)
	}

	turn, _, err := t.deps.NativeTurns.Start(req.SessionID, turnFn, viewer)
	if err != nil {
		// The Registry is closed (serve is shutting down). Resolve cleanly as a
		// cancel rather than faulting the client — nothing durable was lost.
		return libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, nil
	}

	select {
	case <-turn.Done():
		turn.Detach(viewer.ID())
		return nativeResultToResponse(turn.Result())
	case <-t.connCtx.Done():
		// The connection dropped. Detach this viewer; the turn keeps running on the
		// Registry and its result is journaled for the reattaching client. The
		// response returned here is lost with the socket.
		turn.Detach(viewer.ID())
		return libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, nil
	}
}

// nativeResultToResponse maps a completed turn's Result onto the ACP prompt
// response, mirroring the connection-bound path's outcome handling: a clean end / a
// cancellation resolves with the stop reason and no error, while a genuine failure
// resolves with an InternalError (carrying the stop reason when the engine handed
// one back).
func nativeResultToResponse(res nativeturn.Result) (libacp.PromptResponse, error) {
	if res.Err != nil {
		return libacp.PromptResponse{StopReason: res.StopReason}, libacp.InternalError(res.Err.Error())
	}
	return libacp.PromptResponse{StopReason: res.StopReason}, nil
}

// runNativeTurn is the survival-path turn body: the exact chain-execution flow the
// connection-bound nativeDriver.Prompt runs, lifted onto the serve-rooted turnCtx.
// It differs from that path only where survival requires it — the bus subscription
// and event translation emit into the turn's journal + viewers (not one
// connection); cancellation is keyed on the TURN context (a connection drop never
// reaches here); the post-turn SessionInfo is journaled rather than sent
// AfterResponse; and it owns its own turn-scoped tracker span, because the turn
// outlives the connection whose Prompt started it.
func (d *nativeDriver) runNativeTurn(turnCtx context.Context, req libacp.PromptRequest, sess *sessionEntry, input string, droppedContentKinds []string, emit func(context.Context, libacp.SessionNotification)) nativeturn.Result {
	t := d.t

	turnCtx = libtracker.WithNewRequestID(turnCtx)
	reqID, _ := turnCtx.Value(libtracker.ContextKeyRequestID).(string)

	reportErr, reportChange, end := t.tracker().Start(turnCtx, "prompt", "acp_session",
		"session_id", string(req.SessionID), "prompt_blocks", len(req.Prompt))
	defer end()

	// Gate this turn's tool calls under THIS session's chosen HITL policy, and bind a
	// dispatched unit's mission id, onto the serve-rooted turn context — the same
	// per-session injection the connection path does (see prompt.go), riding the turn
	// ctx instead of the request ctx.
	if policyName := t.resolveSessionHITLPolicy(sess); policyName != "" {
		turnCtx = hitlservice.WithPolicyName(turnCtx, policyName)
	}
	if sess.MissionID != "" {
		turnCtx = missiontools.WithMissionID(turnCtx, sess.MissionID)
	}
	// The other end of the same relationship: a session that FIRED missions carries
	// its own id in, unlocking the supervisor tools (see missiontools.WithParentSessionID)
	// so this turn can look at what it dispatched and answer what a unit asks. A
	// session that fired nothing injects nothing and is offered no mission tools.
	if sess.FiredMissions && sess.InternalSessionID != "" {
		turnCtx = missiontools.WithParentSessionID(turnCtx, sess.InternalSessionID)
	}

	// Subscribe to the engine's per-request event stream and translate each event
	// into a session/update emitted through the turn (journal + viewers). Unlike the
	// connection path this runs OFF the connection, so a drop cannot stop it.
	translator := newNativeEventTranslator(emit, sess.effectiveTokenLimit)
	rawCh := make(chan []byte, 64)
	var drainEvents func()
	if bus := t.deps.Engine.Bus; bus != nil && reqID != "" {
		sub, err := bus.Stream(turnCtx, taskengine.TaskEventRequestSubject(reqID), rawCh)
		if err != nil {
			// The turn still runs, but the client gets no incremental updates.
			// Surface why instead of silently degrading (mirrors prompt.go).
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

	// Use the session's effective token budget as the context window, clamped to the
	// model cap when known — identical to prompt.go so indicators and engine shifting
	// stay consistent with the value the user sees.
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
		Chain:          t.deps.ChainRegistry.Default(),
		TemplateVars:   templateVars,
		ToolsAllowlist: toolsAllowlist,
		ContextLength:  contextLen,
	})

	// Drain every translated session/update into the journal BEFORE computing the
	// outcome and emitting the final SessionInfo, so the terminal update is ordered
	// strictly last (mirrors prompt.go's deferred unsubscribe running before the
	// AfterResponse SessionInfo push).
	if drainEvents != nil {
		drainEvents()
	}

	if err != nil {
		// Distinguish a genuine cancellation from an execution failure, keyed on the
		// TURN context only (see prompt.go's rationale). A connection drop does NOT
		// reach here — it never cancels the turn — so the ctx.Err() branch the
		// connection path also checked is deliberately absent. context.Canceled =
		// session/cancel or grace-expiry (a clean stop resolving as cancelled);
		// context.DeadlineExceeded = the hard turn deadline (Belt 2), a FAILURE the
		// reattaching client must see rather than a silent clean stop.
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
	// A cancelled turn resolves cancelled even when the engine salvaged a partial
	// result (see prompt.go). Keyed on context.Canceled specifically: a deadline that
	// fired against a salvaged result is a timeout, not a user cancel.
	if errors.Is(turnCtx.Err(), context.Canceled) {
		stopReason = libacp.StopReasonCancelled
	}

	// Push the post-turn SessionInfo (updatedAt + derived title) as the LAST journaled
	// event, so live viewers and a reattaching client both render the freshened
	// tab/sidebar label. The connection path sent this via AfterResponse; on the
	// survival path it belongs in the journal, where a reconnecting client finds it.
	d.emitSessionInfo(turnCtx, req.SessionID, sess, emit)

	reportChange(string(req.SessionID), map[string]any{
		"stop_reason":           string(stopReason),
		"request_id":            reqID,
		"dropped_content_kinds": droppedContentKinds,
	})
	return nativeturn.Result{StopReason: stopReason}
}

// emitSessionInfo journals the post-turn session_info update (freshened updatedAt +
// the title derived from the first user message), the survival-path equivalent of
// the AfterResponse push in the connection-bound Prompt.
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

// reattachNativeTurn joins this connection to sid's IN-FLIGHT native turn, if one is
// running on the survival Registry — the reconnect path. It is called from
// session/load and session/resume AFTER the durable transcript is (re)established:
// the transcript covers already-persisted turns, the turn journal covers the one
// still in flight (not yet persisted), so the two never overlap and the reconnecting
// client sees the live turn resume without a double-render. The reattached viewer has
// no prompt RPC awaiting it, so it self-detaches when the turn ends or this
// connection drops — which is what arms the anti-zombie belts for the next detach.
// A no-op on the nil-Registry path, for external sessions, and when no turn is in
// flight for sid (AttachIfRunning reports not-running).
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
