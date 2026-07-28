package app

import (
	"context"
	"strings"

	"github.com/contenox/beam/internal/surfaces/beamtui/comp/picker"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/transcript"
	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	libacp "github.com/contenox/beam/libacp"
)

// Session management: beam shows exactly one session at a time, so every act
// in this file is an in-place replacement of the visible session — the
// transcript is rebuilt, the composer's recall list starts over, and the
// status bar re-labels. The surface is three slash commands and one chord,
// registered as palette locals rather than a single `/session <verb>` (the
// palette matches on the first token only).
const (
	// noSessionsText is the switcher's empty state: a workspace whose only
	// session is the current one has nothing to switch to.
	noSessionsText = "no other sessions yet — /new starts one"

	// sessionRosterCap bounds one roster fetch: the switcher shows a recency
	// list, not a paginated archive browser.
	sessionRosterCap = 100

	sessionsHint = "j/k or arrows to move, enter switches, esc closes"
)

// sessionLabel is what the status bar and welcome header call the current
// session: the server's title when there is one, the shortened session name
// otherwise. Matches acpsvc's own precedence (sessionListTitle).
func (a *app) sessionLabel() string {
	if a.sessionTitle != "" {
		return a.sessionTitle
	}
	if a.sessionName != "" {
		return shortSessionName(a.sessionName)
	}
	return shortSessionName(string(a.sessionID))
}

// shortSessionName is the id fallback's display form: the prefix plus the
// first idTailCells of the tail, so `beam-20a88ab8-4f2e-4b0d-9c31-6f1a2b3c4d5e`
// reads as `beam-20a88ab8`. A name whose tail is not long enough to be a
// uuid is returned untouched.
func shortSessionName(name string) string {
	i := strings.IndexByte(name, '-')
	if i < 0 {
		return name
	}
	tail := name[i+1:]
	if len(tail) <= idTailCells {
		return name
	}
	return name[:i+1] + tail[:idTailCells]
}

// idTailCells is how much of a session uuid the label keeps.
const idTailCells = 8

// setSessionTitle adopts a title published for the current session and
// ignores one published for any other, since during a switch's unfiltered
// window session_info_update can arrive for a neighbour session.
func (a *app) setSessionTitle(id libacp.SessionID, title string) {
	if id != a.sessionID || strings.TrimSpace(title) == "" {
		return
	}
	a.sessionTitle = title
}

// openSessions fills and shows the switcher. The roster is fetched once per
// open, matching the file picker's one-walk-per-open rule. A failed fetch
// reports the error and leaves /new available rather than opening onto a lie.
func (a *app) openSessions(ctx context.Context) {
	resp, err := a.deps.Bridge.ListSessions(ctx, libacp.ListSessionsRequest{Cwd: a.deps.Cwd})
	if err != nil {
		a.noticef(frame.StyleError, "session list failed: %v — /new still starts a fresh one", err)
		return
	}
	items := sessionItems(resp.Sessions, a.sessionID, sessionRosterCap)
	a.sessions.SetItems(items)
	a.sessions.SetQuery("")
	a.sessionsOpen = true
	a.closePalette()
	a.closePicker()

	// The roster is the freshest source for this session's own title, so
	// adopt it on the way past rather than leave a stale status-bar label.
	for _, s := range resp.Sessions {
		if s.SessionID == a.sessionID {
			a.setSessionTitle(s.SessionID, s.Title)
			break
		}
	}
}

// sessionItems projects the ACP roster into picker rows: the title as label
// (the id when there is none), the id plus an "active" mark as detail. The
// active session stays in the list, selected first, so Esc changes nothing.
func sessionItems(infos []libacp.SessionInfo, active libacp.SessionID, cap int) []picker.Item {
	items := make([]picker.Item, 0, len(infos))
	for _, s := range infos {
		if len(items) >= cap {
			break
		}
		if s.SessionID == "" {
			continue
		}
		label := strings.TrimSpace(s.Title)
		if label == "" {
			label = string(s.SessionID)
		}
		detail := string(s.SessionID)
		if s.SessionID == active {
			detail += "  (active)"
		}
		items = append(items, picker.Item{ID: string(s.SessionID), Label: label, Detail: detail})
	}
	// The active row leads so Enter on an untouched overlay is a no-op.
	for i, it := range items {
		if it.ID == string(active) {
			moved := append([]picker.Item{items[i]}, items[:i]...)
			items = append(moved, items[i+1:]...)
			break
		}
	}
	return items
}

// sessionsAccept is Enter in the switcher: switch to the highlighted row, or
// simply close when it is the session already on screen.
func (a *app) sessionsAccept(ctx context.Context) {
	it, ok := a.sessions.Selected()
	a.sessionsOpen = false
	if !ok {
		return
	}
	id := libacp.SessionID(it.ID)
	if id == a.sessionID {
		return
	}
	a.switchSession(ctx, id, it.Label)
}

// switchSession replaces the visible session with target. The bridge call
// order — SetActiveSession(""), LoadSession, SetActiveSession(target) —
// follows enginebridge's documented contract: a load's replay hits the wire
// before the caller can act on the response, so the filter must already be
// open. The transcript is rebuilt rather than cleared, since a fresh instance
// is the only reset that cannot leave a half-open tool card or mid-line
// stream behind; the recall list, turn counter and context gauge reset with
// it as per-session facts.
func (a *app) switchSession(ctx context.Context, target libacp.SessionID, label string) {
	if a.inFlight {
		// Switching under a running turn would leave it writing into a
		// transcript nobody can see; refuse and explain instead.
		a.notice(frame.StyleWarn, "a turn is running — ctrl+c interrupts it, then switch")
		return
	}

	a.deps.Bridge.SetActiveSession("")
	if _, err := a.deps.Bridge.LoadSession(ctx, libacp.LoadSessionRequest{
		SessionID: target,
		Cwd:       a.deps.Cwd,
	}); err != nil {
		// Re-close the window on the failure path, or the abandoned attempt
		// leaves every session's updates pointed at this transcript.
		a.deps.Bridge.SetActiveSession(a.sessionID)
		a.noticef(frame.StyleError, "could not open %s: %v — you are still on %s", target, err, a.sessionLabel())
		return
	}

	a.resetForSession(target, string(target), label)
	a.deps.Bridge.SetActiveSession(target)
	a.noticef(frame.StyleMuted, "switched to %s", a.sessionLabel())
}

// newSession mints a session and moves onto it. It opens the same unfiltered
// window as a switch: acpsvc's available_commands_update for the new session
// hits the wire before the caller can know the id to filter for.
func (a *app) newSession(ctx context.Context) {
	if a.inFlight {
		a.notice(frame.StyleWarn, "a turn is running — ctrl+c interrupts it, then /new")
		return
	}

	a.deps.Bridge.SetActiveSession("")
	resp, err := a.deps.Bridge.NewSession(ctx, libacp.NewSessionRequest{Cwd: a.deps.Cwd})
	if err != nil {
		a.deps.Bridge.SetActiveSession(a.sessionID)
		a.noticef(frame.StyleError, "could not start a session: %v — you are still on %s", err, a.sessionLabel())
		return
	}
	if resp.SessionID == "" {
		a.deps.Bridge.SetActiveSession(a.sessionID)
		a.notice(frame.StyleError, "the agent created a session with no id — you are still here")
		return
	}

	a.resetForSession(resp.SessionID, string(resp.SessionID), "")
	a.deps.Bridge.SetActiveSession(resp.SessionID)

	// A new session earns the welcome header again; buildFrame prints it into
	// scrollback on the next commit.
	a.welcomePending = true
}

// resetForSession tears down every piece of state that belonged to the
// session being left and points the loop at the new one. The terminal's
// scrollback and the palette's remote command set are deliberately not
// reset: scrollback stays printed, and the agent republishes commands itself.
func (a *app) resetForSession(id libacp.SessionID, name, title string) {
	a.sessionID = id
	a.sessionName = name
	a.sessionTitle = title

	a.tr = transcript.New()
	a.card = nil
	a.helpOpen = false
	a.sessionsOpen = false
	a.closePicker()
	a.closePalette()

	// Close liveness activities before dropping the map, or a tool call left
	// open by the abandoned session keeps the ticker armed.
	a.endTurn(a.now())

	a.messages = 0
	a.used, a.size = 0, 0
	a.echoSeq = 0
	a.history = nil
	a.comp.SetHistory(nil)
	a.lastPrompt, a.hasLastPrompt = "", false
}

// renameSession is `/rename`. The rename is the agent's — stored server-side
// so it is visible to any ACP client, not just this beam — but the status
// bar adopts the same label locally rather than waiting on a round trip.
func (a *app) renameSession(ctx context.Context, args string) {
	title := strings.TrimSpace(args)
	line := "/rename"
	if title != "" {
		line += " " + title
	}
	if err := a.deps.Bridge.SubmitPrompt(a.sessionID, line); err != nil {
		a.noticef(frame.StyleError, "rename failed: %v", err)
		return
	}
	if title != "" && title != "-" {
		a.sessionTitle = title
	}
	if title == "-" {
		a.sessionTitle = ""
	}
	a.startTurn()
}
