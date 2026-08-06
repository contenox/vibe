package app

import (
	"errors"
	"fmt"
	"time"

	"github.com/contenox/contenox/internal/surfaces/beamtui/comp/approval"
	"github.com/contenox/contenox/internal/surfaces/beamtui/enginebridge"
	"github.com/contenox/contenox/internal/surfaces/beamtui/frame"
	"github.com/contenox/contenox/internal/surfaces/beamtui/liveness"
	libacp "github.com/contenox/contenox/libacp"
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

// onBridge folds one runtime event into every consumer: the transcript, the
// liveness tracker, the status-bar counters, the palette's remote half, the
// approval card, and the completion bell. The transcript sees every event and
// ignores the ones that are not transcript facts itself.
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
		// The session's config selects are the argument domains of the
		// commands that set them (e.g. what /model accepts); this is beam's
		// whole value-completion source, re-pushed by the server on change.
		a.pal.SetValueDomains(enginebridge.ValueDomains(e.Options))
		// The same update also says which of those values is in force. The
		// status bar reads it here rather than from Deps, or /model would
		// change the session and leave the bar naming the launch-time model.
		if provider, model, ok := enginebridge.SelectedModel(e.Options); ok {
			a.provider, a.model = provider, model
		}

	case enginebridge.ReplayEnded:
		// Nothing on the wire ends a replay; without this the trailing
		// replayed message would sit unsettled in the live region forever.
		a.tr.EndReplay()

	case enginebridge.SessionInfoUpdated:
		// Adopt the server-derived title live instead of waiting for the
		// next switch or roster open.
		a.setSessionTitle(e.SessionID, e.Title)

	case enginebridge.PermissionRequested:
		a.card = approval.New(e)
		// The ask settles into scrollback whole and immediately: it is
		// complete on arrival, and the live region can neither hold a card
		// this tall (its over-tall tail is what survives, clipping the
		// header away) nor hand what it clips to scrollback afterwards. Only
		// the subject and the decision line stay live (see buildFrame).
		a.notices = append(a.notices, a.card.Ask(a.width, a.ascii)...)
		// Rings even when beam has focus: an unanswered ask blocks a tool
		// call until its ceiling expires.
		a.bell(now, true)

	case enginebridge.PermissionResolved:
		// The fact a card retires on, whichever way the gate ended. Without
		// it a card beam did not itself answer keeps the keyboard forever:
		// typing is swallowed and y/n go to a channel nobody reads. The
		// outcome is not consulted — a card beam answered is already retired
		// with its own verdict, and one it did not is over either way, which
		// is exactly what retireCard records.
		if a.card != nil && a.card.ToolCallID() == e.ToolCallID {
			a.retireCard()
		}

	case enginebridge.MissionReport:
		a.live.Bump(turnActivityID, now)
		if notifiableReportKinds[e.Kind] {
			a.bell(now, false)
		}

	case enginebridge.MissionAsk:
		// A unit is blocked until this is answered, same rule as a
		// permission gate: rings always, focus or not.
		a.bell(now, true)

	case enginebridge.MissionStatusChanged:
		a.trackMission(e.MissionID, e.New)
		// A mission coming to rest rings under the focus-suppressed rule;
		// opening one does not, since the operator just fired it.
		if enginebridge.MissionStatusTerminal(e.New) {
			a.bell(now, false)
		}

	case enginebridge.MissionPlanRevised:
		// Deliberately silent: a unit reorganizing its own work is not a
		// signal that the operator is needed.

	case enginebridge.InboxItemAdded:
		// No session was watching, so there is no focus to suppress against.
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
		// Backstop to PermissionResolved: a turn that ended cancelled leaves
		// no gate anyone can still answer, whether or not the resolution
		// reached beam.
		if e.StopReason == libacp.StopReasonCancelled {
			a.retireCard()
		}
		if notifiableStopReasons[e.StopReason] {
			a.bell(now, false)
		}

	case enginebridge.TurnFailed:
		a.endTurn(now)
		a.retireCard()
		a.noticef(frame.StyleError, "turn failed: %v", e.Err)
	}
}

// retireCard drops the approval card, settling the verdict it reached into
// scrollback first. It is the only way a card is dropped: every retirement
// path leaves a record, and dropping the card is what hands the keyboard
// back to the composer. A card still pending here was never answered by
// anyone, so it is recorded as cancelled rather than as a decision.
func (a *app) retireCard() {
	if a.card == nil {
		return
	}
	a.card.MarkCancelled()
	if l := a.card.Record(a.width, a.ascii); len(l) > 0 {
		a.notices = append(a.notices, l)
	}
	a.card = nil
}

// trackMission keeps the open-mission set the status bar badges. A status
// this build does not recognize counts as still running, matching
// MissionStatusTerminal's own rule; a mission with no id is not counted,
// since nothing could ever retire it.
func (a *app) trackMission(id, status string) {
	if id == "" {
		return
	}
	if enginebridge.MissionStatusTerminal(status) {
		delete(a.missions, id)
		return
	}
	a.missions[id] = true
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
