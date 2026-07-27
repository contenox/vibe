package app

import (
	"fmt"
	"strings"

	"github.com/contenox/beam/internal/surfaces/beamtui/comp/brand"
	"github.com/contenox/beam/internal/surfaces/beamtui/comp/statusbar"
	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/keymap"
)

// buildFrame assembles the one frame this iteration commits.
//
// It is the only mutating renderer in beam, and it mutates exactly twice: it
// DRAINS the transcript's settled lines and the queued notices, because both
// are append-only history the terminal takes ownership of the moment they are
// printed. Everything else is rebuilt from scratch, which is what makes a
// resize re-wrap the live region for free and leave scrollback untouched.
func (a *app) buildFrame() frame.Frame {
	if a.width < minWidth || a.height < minHeight {
		// Nothing is drained here: the settled lines stay queued until there
		// is a sane width to fix their shape at, because a taken line's shape
		// is final forever.
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
			Model:    a.deps.Model,
			Provider: a.deps.Provider,
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
		live = append(live, a.card.Render(a.width, a.ascii, spinner)...)
	case a.helpOpen:
		live = append(live, a.overlayHelp()...)
	case a.sessionsOpen:
		// One row of the budget goes to the key hint: the switcher is the one
		// overlay an operator arrives at without having typed anything, so it
		// is the one that has to say how to leave.
		live = append(live, a.sessions.Render(a.width, a.rowBudget()-1, a.ascii)...)
		live = append(live, frame.Styled(frame.StyleMuted, sessionsHint))
	case a.pickerOpen:
		// The walk budget is otherwise INVISIBLE: in a big tree fileaddr stops
		// early, the list looks like an ordinary short list, and an operator
		// whose file did not make it concludes it is not there. One muted row
		// says the index is partial and that narrowing the query is what
		// reaches the rest — which is true, because the query goes to the
		// source and the source re-walks under it (see refreshPicker).
		//
		// Only under a QUERY, though. Browse mode is one directory read, which
		// has no budget to exceed and always shows the whole listing, while
		// Truncated reflects the last WALK — so a search that hit the budget
		// would otherwise leave the warning standing over a complete listing
		// the operator backspaced their way to.
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
		// The caret follows the composer wherever the overlays push it, and
		// hides when a modal owns the keyboard outright — with the palette or
		// the file list open the operator is still typing into the composer,
		// so hiding it there would hide the very caret they are steering.
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

// spinner is the current activity glyph, or "" when nothing is open — an
// empty glyph is what keeps an idle frame byte-identical across ticks.
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
		// The session segment shows the LABEL, not the id: `beam-<uuid>` is
		// the one string in the bar that told an operator nothing about the
		// session it names (D17).
		Session:  a.sessionLabel(),
		Messages: a.messages,
		Model:    a.deps.Model,
		Provider: a.deps.Provider,
		Used:     a.used,
		Size:     a.size,
		Inbox:    a.inbox,
		Health:   statusbar.HealthReady,
	}
	if a.inFlight {
		s.Health = statusbar.HealthWorking
	}
	if frames := a.glyphs.SpinnerFrames; len(frames) > 0 {
		if snap, _, ok := a.live.Aggregate(a.now(), len(frames)); ok {
			s.Activity = snap.Text
			s.Spinner = frames[snap.SpinnerIndex%len(frames)]
		}
	}
	// The quit offer outranks the activity line for the two seconds it stands.
	// It is the one thing on the bar that is about to expire, and it is only
	// ever armed on an idle beam (onCtrlC interrupts a running turn instead of
	// offering), so there is no real activity for it to hide.
	if a.ctrlCArmed() {
		s.Activity = quitHintText
		s.Spinner = ""
	}
	return s
}

// quitHintText is the Ctrl+C double-press offer, rendered live in the status
// bar for exactly as long as the window stands (D3, and see onCtrlC).
const quitHintText = "press ctrl+c again to quit"

// indexTruncatedText is the file picker's partial-index footer.
func indexTruncatedText(ascii bool) string {
	if ascii {
		return "index truncated - keep typing"
	}
	return "index truncated — keep typing"
}

// overlayHelp is the `?` overlay: the registry's own registrations, GROUPED
// by the context they answer in, clipped to the rows the terminal can spare
// with a count of what did not fit.
//
// It gets a larger budget than the palette and the picker on purpose — those
// two are a menu beside a line the operator is still typing, this one IS the
// answer to their question — but it still leaves the composer, the status bar
// and a line of transcript alone.
//
// The grouping is the whole point of the rewrite. A flat projection of the
// registry listed `esc` five times with five different meanings and no way to
// tell which one was live, which is not a help screen, it is the registry's
// internal state with the labels removed. Now each scope is a section under
// its own muted header, the scopes reachable from where the operator actually
// IS come first, and everything else sits under one "elsewhere" divider — so
// the first thing on screen is the answer to "what can I press right now".
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
		fmt.Sprintf("+%d more — /help prints them all", len(rows)-(budget-1)))))
}

// helpLines is `/help`'s scrollback output: the same grouped key list the
// overlay shows, then the commands. Both halves are projections — every key
// row comes from a Binding's own Help and every command row from the merged
// palette set — so beam cannot document a key it does not bind or a command
// the agent does not advertise.
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

// helpScopeLabel names each scope in the language of the thing it belongs to,
// not the language of the keymap. The order is the display order: the two
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
// list, the widest command name, and the indent they both carry.
//
// It is computed rather than fixed because it used to be fixed, at sixteen
// cells, and "ctrl+j / alt+enter" is eighteen — so the one binding with two
// chords pushed its own description out of the column and broke the alignment
// of every row after it. Chords and command names are ASCII, so byte length
// is cell width here.
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
