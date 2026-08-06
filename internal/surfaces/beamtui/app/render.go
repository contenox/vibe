package app

import (
	"fmt"
	"strings"

	"github.com/contenox/contenox/internal/surfaces/beamtui/comp/brand"
	"github.com/contenox/contenox/internal/surfaces/beamtui/comp/statusbar"
	"github.com/contenox/contenox/internal/surfaces/beamtui/frame"
	"github.com/contenox/contenox/internal/surfaces/beamtui/keymap"
)

// buildFrame assembles the one frame this iteration commits. It is the only
// mutating renderer in beam: it drains the transcript's settled lines and
// queued notices, both append-only history the terminal takes ownership of
// once printed. Everything else is rebuilt from scratch.
func (a *app) buildFrame() frame.Frame {
	if a.width < minWidth || a.height < minHeight {
		// Nothing is drained: settled lines stay queued until width is sane,
		// since a taken line's shape is final forever.
		return frame.Frame{
			Live:   []frame.Line{frame.Styled(frame.StyleWarn, tooSmallText)},
			Cursor: frame.Cursor{Hidden: true},
		}
	}

	var scrollback []frame.Line
	if a.welcomePending {
		a.welcomePending = false
		scrollback = append(scrollback, brand.Welcome(a.width, brand.Info{
			ASCII:    a.ascii,
			Model:    a.model,
			Provider: a.provider,
			Session:  a.sessionLabel(),
		})...)
	}
	scrollback = append(scrollback, a.tr.TakeAppends(a.width, a.ascii)...)
	if len(a.notices) > 0 {
		scrollback = append(scrollback, a.notices...)
		a.notices = nil
	}

	spinner := a.spinner()
	live := a.tr.Live(a.width, a.ascii, spinner)

	switch {
	case a.card != nil:
		// Only the subject and the decision line: the ask itself was settled
		// into scrollback when it arrived, since a card is far taller than
		// any row budget and the live region's overflow is clipped, not
		// scrolled back (see PermissionRequested in events.go).
		live = append(live, clip(a.card.Prompt(a.width, a.ascii, spinner), a.rowBudget())...)
	case a.helpOpen:
		live = append(live, a.overlayHelp()...)
	case a.sessionsOpen:
		// One row of the budget goes to the key hint, since the switcher is
		// the one overlay an operator arrives at without typing anything —
		// and to the filter once there is one, which has nowhere else to
		// show (the composer is blocked while the switcher is up).
		live = append(live, a.sessions.Render(a.width, a.rowBudget()-1, a.ascii)...)
		live = append(live, frame.Styled(frame.StyleMuted, a.sessionsFooter()))
	case a.pickerOpen:
		// The walk budget is otherwise invisible, so a truncated index gets a
		// muted footer telling the operator to narrow the query. Only under a
		// query: browse mode always shows the whole directory listing, and
		// Truncated reflects the last walk, not the current one.
		budget := a.rowBudget()
		truncated := a.pickerQuery != "" && a.deps.FileSource.Truncated()
		if truncated && budget > 1 {
			budget--
		} else {
			truncated = false
		}
		live = append(live, a.pick.Render(a.width, budget, a.ascii)...)
		if truncated {
			live = append(live, frame.Styled(frame.StyleMuted, indexTruncatedText(a.ascii)))
		}
	case a.pal.IsOpen():
		live = append(live, a.pal.Render(a.width, a.rowBudget(), a.ascii)...)
		if hint, ok := a.pal.ArgHint(a.comp.Draft()); ok {
			live = append(live, frame.Styled(frame.StyleMuted, hint))
		}
	}

	above := len(live)
	blocked := a.composerBlocked()
	live = append(live, a.comp.Render(a.width, !blocked, a.ascii)...)
	live = append(live, statusbar.Render(a.width, a.status()))

	row, col := a.comp.CursorPos()
	return frame.Frame{
		Scrollback: scrollback,
		Live:       live,
		// Hidden only when a modal owns the keyboard outright; the palette
		// and file list keep it visible since the operator is still typing.
		Cursor: frame.Cursor{Row: above + row, Col: col, Hidden: blocked},
	}
}

// rowBudget is how many rows an overlay may spend: the fixed budget, cut down
// on a short terminal so the composer and status bar always survive.
func (a *app) rowBudget() int {
	budget := a.height - 3
	if budget > overlayRows {
		budget = overlayRows
	}
	if budget < 1 {
		budget = 1
	}
	return budget
}

// clip cuts an overlay's lines to its row budget, keeping the HEAD. The
// terminal keeps the tail of an over-tall live region instead, and a live
// region never enters scrollback, so anything past the budget is gone for
// good — the first rows are the ones worth keeping.
func clip(lines []frame.Line, budget int) []frame.Line {
	if budget < 0 || len(lines) <= budget {
		return lines
	}
	return lines[:budget]
}

// spinner is the current activity glyph, or "" when nothing is open, which
// keeps an idle frame byte-identical across ticks.
func (a *app) spinner() string {
	frames := a.glyphs.SpinnerFrames
	if len(frames) == 0 {
		return ""
	}
	snap, _, ok := a.live.Aggregate(a.now(), len(frames))
	if !ok {
		return ""
	}
	return frames[snap.SpinnerIndex%len(frames)]
}

// status assembles the status bar's state from what the other components
// publish. Nothing is fetched here: every number came off the event stream.
func (a *app) status() statusbar.State {
	s := statusbar.State{
		ASCII: a.ascii,
		// The session segment shows the label, not the id.
		Session:  a.sessionLabel(),
		Messages: a.messages,
		Model:    a.model,
		Provider: a.provider,
		Used:     a.used,
		Size:     a.size,
		Missions: len(a.missions),
		Inbox:    a.inbox,
		Health:   statusbar.HealthReady,
	}
	if a.inFlight {
		s.Health = statusbar.HealthWorking
	}
	// Last, and unconditional: a closed engine connection outranks every
	// other health reading, and nothing reopens it.
	if a.disconnected {
		s.Health = statusbar.HealthDisconnected
	}
	if frames := a.glyphs.SpinnerFrames; len(frames) > 0 {
		if snap, _, ok := a.live.Aggregate(a.now(), len(frames)); ok {
			s.Activity = snap.Text
			s.Spinner = frames[snap.SpinnerIndex%len(frames)]
		}
	}
	// The quit offer outranks the activity line for the two seconds it
	// stands; it is only ever armed on an idle beam, so there is no real
	// activity for it to hide.
	if a.ctrlCArmed() {
		s.Activity = quitHintText
		s.Spinner = ""
	}
	return s
}

// quitHintText is the Ctrl+C double-press offer (see onCtrlC).
const quitHintText = "press ctrl+c again to quit"

// indexTruncatedText is the file picker's partial-index footer.
func indexTruncatedText(ascii bool) string {
	if ascii {
		return "index truncated - keep typing"
	}
	return "index truncated — keep typing"
}

// overlayHelp is the `?` overlay: the registry's registrations grouped by the
// scope they answer in, clipped to the rows the terminal can spare with a
// count of what did not fit. Scopes reachable from where the operator
// actually is come first; everything else sits under one "elsewhere" divider.
func (a *app) overlayHelp() []frame.Line {
	rows := a.helpRows(true)
	budget := a.height - 4
	if budget < 1 {
		budget = 1
	}
	if len(rows) <= budget {
		return rows
	}
	out := append([]frame.Line(nil), rows[:budget-1]...)
	return append(out, a.gutterLine(frame.S(frame.StyleMuted,
		fmt.Sprintf("+%d more — /%s prints them all", len(rows)-(budget-1), localKeys))))
}

// helpLines is `/keys`' scrollback output: the same grouped key list the
// overlay shows, then the commands. Both are projections of the registry and
// the palette, so beam cannot document a key or command that does not exist.
func (a *app) helpLines() []frame.Line {
	out := a.helpRows(false)
	out = append(out, frame.Plain(""), frame.Styled(frame.StyleHeading, "commands"))
	col := a.helpColumn()
	for _, e := range a.pal.Filtered() {
		out = append(out, frame.L(
			frame.S(frame.StyleActive, pad(helpIndent+"/"+e.Name, col)),
			frame.S(frame.StyleMuted, e.Description),
		))
	}
	out = append(out, frame.Plain(""))
	return out
}

// helpIndent hangs a key or command row under its section header.
const helpIndent = "  "

// helpColumnGap is the space between the widest chord and the help text.
const helpColumnGap = 2

// helpScopeLabels names each scope for display, in display order: the two
// scopes an operator is normally in first, then the modals.
var helpScopeLabels = []struct {
	scope keymap.Scope
	label string
}{
	{keymap.ScopeComposer, "composer"},
	{keymap.ScopeGlobal, "anywhere"},
	{keymap.ScopePalette, "command menu"},
	{keymap.ScopePicker, "file and session lists"},
	{keymap.ScopeApproval, "approval card"},
	{keymap.ScopeHelp, "this list"},
}

// helpColumn is the column every help row's text hangs off: the widest chord
// list or command name plus indent. Computed rather than fixed, since a
// binding with multiple chords can exceed any fixed guess. Chords and
// command names are ASCII, so byte length is cell width here.
func (a *app) helpColumn() int {
	widest := 0
	for _, e := range a.reg.Help(helpScopes) {
		if n := len(helpIndent) + len(chordText(e)); n > widest {
			widest = n
		}
	}
	for _, e := range a.pal.Filtered() {
		if n := len(helpIndent) + len("/"+e.Name); n > widest {
			widest = n
		}
	}
	return widest + helpColumnGap
}

// helpRows renders the grouped key list, optionally hung off the brand
// gutter (the overlay wants it; `/help`'s scrollback output sits under its
// own heading and does not).
func (a *app) helpRows(gutter bool) []frame.Line {
	line := func(spans ...frame.Span) frame.Line {
		if gutter {
			return a.gutterLine(spans...)
		}
		return frame.L(spans...)
	}

	active := make(map[keymap.Scope]bool)
	for _, s := range a.focus.ActiveScopes() {
		active[s] = true
	}
	byScope := make(map[keymap.Scope][]keymap.HelpEntry)
	for _, e := range a.reg.Help(helpScopes) {
		byScope[e.Scope] = append(byScope[e.Scope], e)
	}

	col := a.helpColumn()
	section := func(label string, entries []keymap.HelpEntry) []frame.Line {
		out := []frame.Line{line(frame.S(frame.StyleMuted, label))}
		for _, e := range entries {
			out = append(out, line(
				frame.S(frame.StyleActive, pad(helpIndent+chordText(e), col)),
				frame.S(frame.StyleMuted, e.Help),
			))
		}
		return out
	}

	out := []frame.Line{line(frame.S(frame.StyleHeading, "keys"))}
	var elsewhere []frame.Line
	for _, g := range helpScopeLabels {
		entries := byScope[g.scope]
		if len(entries) == 0 {
			continue
		}
		if active[g.scope] {
			out = append(out, section(g.label, entries)...)
			continue
		}
		elsewhere = append(elsewhere, section(g.label, entries)...)
	}
	if len(elsewhere) > 0 {
		out = append(out, line(frame.S(frame.StyleMuted, "elsewhere")))
		out = append(out, elsewhere...)
	}
	return out
}

// gutterLine hangs spans off the brand beam-bar, the same device the composer
// and the welcome header mark their regions with.
func (a *app) gutterLine(spans ...frame.Span) frame.Line {
	l := frame.Line{
		frame.S(frame.StyleBrand, a.glyphs.PromptSigil),
		frame.S(frame.StyleNone, " "),
	}
	return append(l, spans...)
}

// chordText is one binding's chords as the operator would say them.
func chordText(e keymap.HelpEntry) string {
	keys := make([]string, 0, len(e.Keys))
	for _, k := range e.Keys {
		keys = append(keys, string(k))
	}
	return strings.Join(keys, " / ")
}

// pad right-pads s to at least width cells. Help rows are ASCII key names and
// command names, so byte length is cell width here; anything wider simply
// gets one separating space rather than a misaligned truncation.
func pad(s string, width int) string {
	if len(s) >= width {
		return s + " "
	}
	return s + strings.Repeat(" ", width-len(s))
}
