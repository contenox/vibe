package app

import (
	"errors"
	"fmt"
	"time"

	"github.com/contenox/beam/internal/surfaces/beamtui/comp/approval"
	"github.com/contenox/beam/internal/surfaces/beamtui/enginebridge"
	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/liveness"
	libacp "github.com/contenox/beam/libacp"
)

// notifiableStopReasons are the turn endings worth a completion bell: the
// turn is genuinely over and the operator's attention is worth reclaiming. A
// cancelled turn is deliberately absent — the operator cancelled it, they are
// already here.
var notifiableStopReasons = map[libacp.StopReason]bool{
	libacp.StopReasonEndTurn:         true,
	libacp.StopReasonMaxTokens:       true,
	libacp.StopReasonMaxTurnRequests: true,
	libacp.StopReasonRefusal:         true,
}

// notifiableReportKinds are the mission-report kinds that mean "a human is
// needed or the work is done" — progress pings never ring.
var notifiableReportKinds = map[string]bool{
	"blocker": true,
	"result":  true,
}

// onBridge folds one runtime event into every consumer: the transcript
// (which renders it), the liveness tracker (which decides whether anything is
// happening), the status-bar counters, the palette's remote half, the
// approval card, and the completion bell.
//
// The transcript sees EVERY event — it ignores the ones that are not
// transcript facts itself — which is what keeps "one event, one place that
// decides what it looks like" true.
func (a *app) onBridge(ev enginebridge.Event) {
	a.tr.Apply(ev)
	now := a.now()

	switch e := ev.(type) {
	case enginebridge.UserEcho:
		// Replay: count it toward the session's turn count so the status bar
		// is right on a resumed session.
		if e.Text != "" {
			a.messages++
			a.history = append(a.history, e.Text)
			a.comp.SetHistory(a.history)
		}

	case enginebridge.ToolCallOpened:
		a.openTool(e.ToolCallID, e.Title, string(e.Kind), e.Status, now)

	case enginebridge.ToolCallUpdated:
		a.openTool(e.ToolCallID, e.Title, string(e.Kind), e.Status, now)

	case enginebridge.TextDelta:
		a.live.Bump(turnActivityID, now)

	case enginebridge.ThoughtDelta:
		a.live.Bump(turnActivityID, now)

	case enginebridge.UsageUpdated:
		a.used, a.size = e.Used, e.Size

	case enginebridge.CommandsUpdated:
		a.pal.SetRemote(e.Commands)

	case enginebridge.ConfigOptionUpdated:
		// The session's config selects are also the ARGUMENT domains of the
		// commands that set them: the model select is the answer to "what can
		// /model be", and it is the server's answer, re-pushed whenever it
		// changes. Handing them to the palette is the whole of beam's
		// value-completion source — there is no list here to go stale, and a
		// session that advertised nothing completes nothing.
		a.pal.SetValueDomains(enginebridge.ValueDomains(e.Options))

	case enginebridge.ReplayEnded:
		// Nothing on the wire ends a replay, so the trailing replayed
		// message would sit unsettled in the live region forever without
		// this — new notices would print above it.
		a.tr.EndReplay()

	case enginebridge.SessionInfoUpdated:
		// The server derives a title from the first user message and pushes
		// it after the turn; the status bar adopts it live instead of
		// waiting for the next switch or roster open.
		a.setSessionTitle(e.SessionID, e.Title)

	case enginebridge.PermissionRequested:
		a.card = approval.New(e)
		// An unanswered ask blocks a tool call until its ceiling expires, so
		// it rings even when beam has focus (D23).
		a.bell(now, true)

	case enginebridge.MissionReport:
		a.live.Bump(turnActivityID, now)
		if notifiableReportKinds[e.Kind] {
			a.bell(now, false)
		}

	case enginebridge.MissionAsk:
		// A unit is BLOCKED until this is answered — the same shape as a
		// permission gate, and rung on the same rule: always, focus or not.
		a.bell(now, true)

	case enginebridge.MissionStatusChanged:
		// A mission coming to rest is a completion, so it rings under the
		// ordinary focus-suppressed rule. Opening one is not: the operator
		// just fired it and is looking at the line that did.
		if enginebridge.MissionStatusTerminal(e.New) {
			a.bell(now, false)
		}

	case enginebridge.MissionPlanRevised:
		// Deliberately silent. A unit reorganizing its own work is the thing
		// the operator delegated; ringing for it would make the one signal
		// that means "you are needed" fire for a unit getting on with it.

	case enginebridge.InboxItemAdded:
		// An inbox item exists precisely BECAUSE no session was watching, so
		// there is no surface for focus to suppress against: the operator is
		// by definition not looking at the mission this came from. It rings
		// always, and leaves a badge behind for after the sound is gone.
		a.inbox++
		a.bell(now, true)

	case enginebridge.TerminalChunk:
		a.live.Bump(turnActivityID, now)

	case enginebridge.ShellRunResult:
		if e.Err != nil {
			a.noticef(styleForShellErr(e.Err), "shell: %v", e.Err)
		}

	case enginebridge.TurnEnded:
		a.endTurn(now)
		if e.StopReason == libacp.StopReasonCancelled && a.card != nil {
			// The cancel beam asked for came back: the card stops pretending
			// to wait for a keystroke.
			a.card.MarkCancelled()
			a.card = nil
		}
		if notifiableStopReasons[e.StopReason] {
			a.bell(now, false)
		}

	case enginebridge.TurnFailed:
		a.endTurn(now)
		if a.card != nil {
			a.card.MarkCancelled()
			a.card = nil
		}
	}
}

// startTurn opens the turn activity. There is no TurnStarted event — the
// submitted prompt IS the start, and the bridge answers only when it ends.
func (a *app) startTurn() {
	a.inFlight = true
	a.live.Open(liveness.KindTurn, turnActivityID, turnLabel, a.now())
}

// endTurn closes the turn and every tool call it left open. A tool call whose
// terminal status never arrived would otherwise keep the ticker armed and the
// spinner spinning over work that has stopped.
func (a *app) endTurn(now time.Time) {
	a.inFlight = false
	a.live.Close(turnActivityID, now)
	for id := range a.openTools {
		a.live.Close(toolActivityPrefix+id, now)
		delete(a.openTools, id)
	}
}

// openTool opens or bumps one tool call's activity, closing it the moment its
// status goes terminal.
func (a *app) openTool(id, title, kind string, status libacp.ToolCallStatus, now time.Time) {
	if id == "" {
		return
	}
	activity := toolActivityPrefix + id
	switch status {
	case libacp.ToolCallStatusCompleted, libacp.ToolCallStatusFailed:
		a.live.Close(activity, now)
		delete(a.openTools, id)
		return
	}
	if !a.openTools[id] {
		a.openTools[id] = true
		a.live.Open(liveness.KindToolCall, activity, toolLabel(title, kind), now)
		return
	}
	a.live.Bump(activity, now)
}

// toolLabel is what the status line calls a running tool call.
func toolLabel(title, kind string) string {
	switch {
	case title != "":
		return title
	case kind != "":
		return kind
	}
	return toolLabelFallback
}

// bell emits the completion signal, suppressed while beam has the window's
// focus (the operator is already looking) unless always is set, and rate-
// limited to one ring per bellWindow however many facts land inside it.
func (a *app) bell(now time.Time, always bool) {
	if !always && a.focusedWindow {
		return
	}
	if a.hasBell && now.Sub(a.lastBell) < bellWindow {
		return
	}
	a.hasBell = true
	a.lastBell = now
	a.deps.Term.Bell()
}

// styleForShellErr distinguishes "the runtime has no shell sessions" — a
// feature that is ABSENT, which is a warning — from a genuine failure.
func styleForShellErr(err error) frame.StyleID {
	if errors.Is(err, enginebridge.ErrShellDisabled) {
		return frame.StyleWarn
	}
	return frame.StyleError
}

// userEcho builds the event a submitted line is folded into the transcript
// as. The message id namespaces beam's own echoes away from the agent's
// message ids, so an echo never splices into a streaming message.
func userEcho(session libacp.SessionID, seq int, text string) enginebridge.UserEcho {
	return enginebridge.UserEcho{
		SessionID: session,
		MessageID: fmt.Sprintf("beam-local-%d", seq),
		Text:      text,
	}
}
