package app

import (
	"context"
	"strings"

	"github.com/contenox/beam/internal/surfaces/beamtui/comp/picker"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/transcript"
	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	libacp "github.com/contenox/beam/libacp"
)

// Session management (blueprint 4.8). beam shows exactly ONE session at a
// time (D13) — there are no tabs, no split panes and no background transcript
// — so every act in this file is an IN-PLACE replacement of the one visible
// session: the transcript is rebuilt, the composer's recall list starts over,
// and the status bar re-labels.
//
// The surface is three slash commands and one chord, chosen over a single
// `/session <verb>` because the palette matches on the FIRST token only: a
// `/session` entry could complete to "/session " and then leave the operator
// guessing at verbs it cannot list, while three flat names are three rows in
// the same menu that already answers "/". They are palette LOCALS, which is
// what puts them in the bare-"/" menu, in `/help` and in the `?` overlay with
// no extra registration anywhere.
const (
	// noSessionsText is the switcher's empty state. It is a fact, not an
	// error: a workspace whose only session is the current one has nothing to
	// switch TO.
	noSessionsText = "no other sessions yet — /new starts one"

	// sessionRosterCap bounds one roster fetch. The agent pages at 100; beam
	// asks once and shows what came back rather than walking the cursor,
	// because a switcher is a recency list, not an archive browser.
	sessionRosterCap = 100

	// sessionsHint is the switcher's one key line. It is spelled out rather
	// than projected from the registry because the four chords it names are
	// registered under three different binding ids across two scopes, and a
	// projection of that reads as a keymap dump, not as an instruction.
	sessionsHint = "j/k or arrows to move, enter switches, esc closes"
)

// sessionLabel is what the status bar and the welcome header call the current
// session: the server's title when there is one, the shortened session name
// otherwise.
//
// The precedence is deliberate and matches acpsvc's own (sessionListTitle): a
// title is either what the operator named the session or what its first
// message was about, and both beat `beam-<uuid>`, which tells a human nothing
// except that beam made it.
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
// reads as `beam-20a88ab8`.
//
// A title is what an operator wants here, but a title only exists after the
// server has seen a message — so a brand-new session spends its whole first
// turn labelled by a forty-one character uuid, which pushed every other
// segment off the status bar and identified the session no better than its
// first eight hex digits do. Eight is enough to tell two sessions apart in a
// roster a human can read, and the full id is still what /sessions shows as
// each row's detail and what every error message quotes.
//
// A name that is not id-shaped — an operator's own label, a short name like
// "beam-0001" — is returned untouched: the cut only ever applies to a tail
// long enough to be a uuid.
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

// setSessionTitle adopts a title the server published for the CURRENT session
// and ignores one published for any other, which is what makes it safe to
// call from the event fold: session_info_update is a per-session
// notification, and during a switch's unfiltered window it can be a
// neighbour's.
func (a *app) setSessionTitle(id libacp.SessionID, title string) {
	if id != a.sessionID || strings.TrimSpace(title) == "" {
		return
	}
	a.sessionTitle = title
}

// openSessions fills and shows the switcher. The roster is fetched ONCE per
// open — a session list is a database read behind an RPC, and re-fetching it
// per keystroke is the frozen-composer failure the file picker's own
// one-walk-per-open rule exists to prevent.
//
// A failed fetch is error-and-suggest (D16): the operator is told what broke
// and what still works, and the overlay does not open onto a lie.
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

	// The roster is also the freshest thing that ever says what THIS session
	// is called, so adopt the current row's title on the way past: an operator
	// who opens the switcher should not see a stale label in the status bar
	// underneath it.
	for _, s := range resp.Sessions {
		if s.SessionID == a.sessionID {
			a.setSessionTitle(s.SessionID, s.Title)
			break
		}
	}
}

// sessionItems projects the ACP roster into picker rows: the humane title as
// the label (the id only when the agent had nothing better), and the id plus
// an "active" mark as the dimmed detail.
//
// The ACTIVE session stays in the list rather than being filtered out. It is
// where the selection starts, so the operator can see what they are leaving
// and Esc out of the overlay having changed nothing — a list that silently
// omitted the current session would make "which one am I on?" unanswerable
// from the one screen that exists to answer it.
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
	// The active row leads: the selection lands on it, so Enter on an
	// untouched overlay is a no-op rather than a switch nobody asked for.
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

// switchSession replaces the visible session with target.
//
// The bridge call order is enginebridge's documented one — SetActiveSession("")
// to open the unfiltered window, LoadSession to replay, SetActiveSession(target)
// to close it — because a load's replay is written on the wire before the
// caller can act on the load's response, and a filter still pointing at the
// OLD session would drop every replayed line of the new one.
//
// The transcript is REBUILT rather than cleared: a fresh instance is the only
// state reset that cannot leave a half-open tool card, a mid-line stream or a
// settled-line queue behind, and the replay that follows hydrates it. The
// composer's recall list, the turn counter and the context gauge start over
// with it, because all three are facts about a session, not about beam.
func (a *app) switchSession(ctx context.Context, target libacp.SessionID, label string) {
	if a.inFlight {
		// Error-and-suggest (D16). Switching under a running turn would leave
		// the turn writing into a transcript nobody can see and its approval
		// card answerable by an operator who has moved on; refusing costs one
		// keystroke and explains itself.
		a.notice(frame.StyleWarn, "a turn is running — ctrl+c interrupts it, then switch")
		return
	}

	a.deps.Bridge.SetActiveSession("")
	if _, err := a.deps.Bridge.LoadSession(ctx, libacp.LoadSessionRequest{
		SessionID: target,
		Cwd:       a.deps.Cwd,
	}); err != nil {
		// The window has to close again on the failure path or the abandoned
		// attempt leaves every session's updates pointed at this transcript.
		a.deps.Bridge.SetActiveSession(a.sessionID)
		a.noticef(frame.StyleError, "could not open %s: %v — you are still on %s", target, err, a.sessionLabel())
		return
	}

	a.resetForSession(target, string(target), label)
	a.deps.Bridge.SetActiveSession(target)
	a.noticef(frame.StyleMuted, "switched to %s", a.sessionLabel())
}

// newSession mints a session and moves onto it.
//
// It takes the SAME unfiltered window as a switch, for a different reason:
// acpsvc defers a new session's available_commands_update until after the
// session/new response, so the menu update is on the wire before this caller
// can possibly know the id to filter for.
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

	// A new session earns the welcome header again: it is the one-time
	// statement of WHERE YOU ARE, and after a switch that is a different
	// place. buildFrame prints it into scrollback on the next commit.
	a.welcomePending = true
}

// resetForSession tears down every piece of state that belonged to the
// session being left, and points the loop at the new one.
//
// Everything here is per-session by nature. The two things deliberately NOT
// reset are the terminal's scrollback (already printed, and the operator's to
// read) and the palette's remote command set, which the agent republishes for
// the new session on its own.
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

	// Close the turn's liveness activities BEFORE dropping the map, or a tool
	// call left open by the session being abandoned keeps the ticker armed
	// over work beam is no longer watching.
	a.endTurn(a.now())

	a.messages = 0
	a.used, a.size = 0, 0
	a.echoSeq = 0
	a.history = nil
	a.comp.SetHistory(nil)
}

// renameSession is `/rename`. The rename itself is the AGENT's: the title is
// read back by session/list and pushed by session_info_update, so storing it
// server-side is what makes one operator's rename visible in an ACP editor
// and the CLI too, instead of only in the beam that typed it.
//
// beam adopts the new label locally at the same time. That is not a guess
// about what the server will do — it is the same string, and the alternative
// is a status bar that keeps the old name until a turn completes.
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
