// Package statusbar renders beam's persistent one-line status bar: the
// bottom-most sliver of the live region that always shows identity,
// session, activity, context budget, the operator inbox, missions and
// connection health.
//
// Like every beam component (blueprint section 1, the testability
// doctrine) Render is a pure function of (width, State): it reads no
// terminal, keeps no state of its own, and never wraps or mid-truncates —
// when the full segment set does not fit, whole segments drop in a fixed,
// documented priority (see Render).
package statusbar

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/contenox/beam/internal/surfaces/beamtui/comp/brand"
	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/sanitize"
	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
)

// Health is the closed vocabulary connection-lifecycle publishes (blueprint
// 4.4 requirement 6). Render treats any other string as "not ready" too
// (rendered, styled as a working/degraded state) rather than panicking —
// the vocabulary is closed by convention, not by a Go type, so an unknown
// value degrades safely instead of hiding.
const (
	HealthReady         = "ready"
	HealthWorking       = "working"
	HealthReconnecting  = "reconnecting"
	HealthError         = "error"
	HealthDisconnected  = "disconnected"
	HealthSetupRequired = "setup_required"
)

// State is everything one status-bar render needs, all sourced from other
// components' publishers (app-shell composites; statusbar never fetches).
type State struct {
	// ASCII selects the glyph fallback: true exactly when the caller's
	// caps profile is Mono, matching brand.Info.ASCII's contract.
	ASCII bool

	// Session is the active session's display name. Empty hides the
	// session segment entirely.
	Session string
	// Messages is the turn count in Session. Shown as "·N" (ASCII "-N")
	// appended to Session only when > 0.
	Messages int

	// Model and Provider name the active backend. Model empty hides the
	// model segment; Provider is an additive "·provider" suffix.
	Model    string
	Provider string

	// Used and Size are context-window token counts. The gauge renders
	// ONLY when Size > 0 — a Size of 0 means no usage_update has landed
	// yet, and showing "0/0 (0%)" would be a lie, not an empty state.
	Used int
	Size int

	// Missions is the count of badge-worthy missions. The badge renders
	// only when >= 1 — zero missions is not "0 missions", it is nothing.
	Missions int

	// Inbox is how many operator-inbox items have arrived SINCE THIS BEAM
	// LAUNCHED. The badge renders only when >= 1, on the same "zero is
	// nothing" rule as Missions.
	//
	// It is deliberately launch-relative, and the honesty matters more than
	// the number: the operator inbox is a DURABLE store (a mission report
	// that reached no live session), so the count a user would call
	// "unacked" spans every beam that ever ran, and answering it needs a
	// service query — operatorinbox.List, whose surface is the CLI's, not
	// this component's. What the app can know without one is what it
	// WATCHED ARRIVE, so that is what this field is: "N things landed in
	// the inbox while you were in here", which is a true statement and the
	// one that earns the bell it comes with. It resets to zero on relaunch
	// and never decrements — beam has no dismiss action to decrement it
	// with. Wiring a real backlog count is a later slice's job, and it
	// replaces this field's meaning rather than adding to it.
	Inbox int

	// Health is connection-lifecycle's state name. Render shows a health
	// segment only when Health is set and != HealthReady: ready is the
	// silent default, every other state is worth a permanent glance.
	Health string

	// Activity is the liveness aggregate text (liveness.Snapshot.Text),
	// pre-built by the app. Empty means nothing is happening right now,
	// which hides the whole activity segment.
	Activity string
	// Spinner is the current spinner glyph for Activity, already
	// ASCII-safe when ASCII is true — statusbar renders it verbatim and
	// never substitutes its own glyph, unlike the missions badge below.
	Spinner string
}

// Every string field above is treated as UNTRUSTED and sanitized at Render
// (see sanitizeState): the session name, model and provider come off the
// wire, and Activity is composed from event text. None of them may carry a
// control character into the one component that is on screen at all times.

// Segment names, in the fixed left-to-right render order. They also name
// entries in dropOrder below; keep the two lists honest with each other.
const (
	segIdentity = "identity"
	segSession  = "session"
	segActivity = "activity"
	segGauge    = "gauge"
	segInbox    = "inbox"
	segMissions = "missions"
	segModel    = "model"
	segHealth   = "health"
)

// dropOrder is the fixed priority Render drops whole segments in when the
// full set does not fit width: first dropped, to last. identity is
// deliberately absent from this list — it is never dropped like the
// others, see Render's doc for what happens to it instead.
// inbox sits immediately AHEAD of missions here — dropped one step sooner —
// because the two badges say related things and the mission count is the one
// that survives. An inbox arrival already rang the bell that told the operator
// about it; the badge is a reminder, and a reminder is the cheaper thing to
// lose when the terminal runs out of room.
var dropOrder = []string{segSession, segActivity, segInbox, segMissions, segGauge, segModel, segHealth}

// separator joins two present segments. It is fixed regardless of ASCII
// mode — only the glyphs inside segments (middot, spinner, badge) vary.
const separator = "  "

// segment pairs a drop-priority name with its rendered content.
type segment struct {
	name string
	line frame.Line
}

// Render composes the status bar's single line for width from s. The
// returned line is exactly one row, rune-safe, and always padded with
// trailing spaces to precisely width cells — callers never need to pad or
// clip it themselves.
//
// Segments render left to right in this fixed order, each populated only
// when its data applies:
//
//	identity  brand.StatusSegment(s.ASCII) — always present.
//	session   s.Session, plus the message count when Messages > 0: "·N"
//	          in unicode, " (N)" in ASCII. Present when Session != "".
//	activity  s.Spinner (brand-styled) + " " + s.Activity. Present when
//	          Activity != "".
//	gauge     "{Used}/{Size} ({pct}%)". Present only when Size > 0 — a
//	          Size of 0 means no usage has been reported yet, and a
//	          gauge would misleadingly read "0/0".
//	inbox     "✉ N" (ASCII "in:N"), StyleHITL. Present only when Inbox >= 1.
//	missions  "◇ N" (ASCII "m:N"). Present only when Missions >= 1.
//	model     s.Model, plus " · "+Provider (ASCII " - "+Provider) when
//	          Provider != "". Present when Model != "".
//	health    s.Health verbatim. Present only when it ADDS something —
//	          see showHealth.
//
// Present segments are joined by a two-space StyleNone separator.
//
// When the full present set does not fit width, Render drops WHOLE
// segments — never wrapping a segment across the line, never
// mid-truncating one — in this fixed priority, first dropped to last:
//
//	session, activity, inbox, missions, gauge, model, health, identity
//
// identity is the last resort and is handled specially: every other
// segment is dropped to nothing, but identity is only ever dropped once
// removing it would still leave the line too wide to be worth it — in
// practice that never happens, because once every other segment is gone
// identity is rendered even if it alone still overflows width, truncated
// (not blank) to fit. The identity segment is therefore always the
// leftmost content of a non-empty status line.
func Render(width int, s State) frame.Line {
	if width <= 0 {
		return frame.Line{}
	}

	s = sanitizeState(s)
	segs := buildSegments(s)
	present := make(map[string]bool, len(segs))
	for _, sg := range segs {
		present[sg.name] = true
	}

	for i := 0; i < len(dropOrder) && totalWidth(segs, present) > width; i++ {
		delete(present, dropOrder[i])
	}

	if totalWidth(segs, present) > width {
		// Only identity remains and even it does not fit alone: truncate
		// it in place rather than rendering nothing.
		for i := range segs {
			if segs[i].name == segIdentity {
				segs[i].line = truncateLine(segs[i].line, width)
			}
		}
	}

	return padLine(buildLine(segs, present), width)
}

// sanitizeState reduces every caller-supplied string in s to text a span may
// hold. It runs at Render rather than at the app's assignment sites because
// this is the one place that can be sure it happened: State is a plain struct
// the app-shell composites from half a dozen publishers, and "everyone
// remembers to sanitize before setting a field" is not an invariant, it is a
// hope.
//
// The bar is a single row that is always padded to EXACTLY width, and its
// segment arithmetic is cell counting on plain text. A tab or a newline in a
// session name breaks that arithmetic outright — a newline in a span violates
// frame.Line's one-row contract, and the engine would draw a status bar that
// scrolled the screen. sanitize.Line removes both, along with the escape
// sequences a model name or a health string from a misbehaving peer could
// otherwise smuggle into the one component that is on screen at all times.
func sanitizeState(s State) State {
	s.Session = sanitize.Line(s.Session)
	s.Model = sanitize.Line(s.Model)
	s.Provider = sanitize.Line(s.Provider)
	s.Health = sanitize.Line(s.Health)
	s.Activity = sanitize.Line(s.Activity)
	s.Spinner = sanitize.Line(s.Spinner)
	return s
}

// buildSegments returns every applicable segment for s, in the fixed
// left-to-right render order. Inapplicable segments (per State's field
// docs) are simply absent from the result.
func buildSegments(s State) []segment {
	segs := make([]segment, 0, 8)
	segs = append(segs, segment{segIdentity, brand.StatusSegment(s.ASCII)})

	if s.Session != "" {
		segs = append(segs, segment{segSession, sessionLine(s)})
	}
	if s.Activity != "" {
		segs = append(segs, segment{segActivity, activityLine(s)})
	}
	if s.Size > 0 {
		segs = append(segs, segment{segGauge, gaugeLine(s)})
	}
	if s.Inbox >= 1 {
		segs = append(segs, segment{segInbox, inboxLine(s)})
	}
	if s.Missions >= 1 {
		segs = append(segs, segment{segMissions, missionsLine(s)})
	}
	if s.Model != "" {
		segs = append(segs, segment{segModel, modelLine(s)})
	}
	if showHealth(s) {
		segs = append(segs, segment{segHealth, healthLine(s)})
	}
	return segs
}

// showHealth is the health segment's presence rule: it renders only when it
// ADDS something to what the bar already says.
//
// "ready" is beam's silent default and never renders. "working" is the other
// state that can be redundant: whenever an Activity is present the bar is
// already saying, in words and with a spinner, that beam is working — and the
// app publishes Health=working and Activity="working" from the same in-flight
// turn, so the bar read "⠋ working … working" for the whole of every turn.
// Activity wins there. Every other state (error, disconnected, reconnecting,
// setup_required, anything unrecognized) is a fact no other segment carries,
// so it renders regardless of what else is on the line.
func showHealth(s State) bool {
	switch {
	case s.Health == "" || s.Health == HealthReady:
		return false
	case s.Health == HealthWorking && s.Activity != "":
		return false
	}
	return true
}

func sessionLine(s State) frame.Line {
	text := s.Session + countSuffix(s.ASCII, s.Messages)
	return frame.Line{frame.S(frame.StyleMuted, text)}
}

// activityLine is the spinner and what it is spinning over. A spinnerless
// activity — a transient hint rather than open work — gets no leading space,
// so it starts at the separator instead of one cell past it.
func activityLine(s State) frame.Line {
	if s.Spinner == "" {
		return frame.Line{frame.S(frame.StyleNone, s.Activity)}
	}
	return frame.Line{
		frame.S(frame.StyleBrand, s.Spinner),
		frame.S(frame.StyleNone, " "+s.Activity),
	}
}

func gaugeLine(s State) frame.Line {
	pct := 0
	if s.Size > 0 {
		pct = s.Used * 100 / s.Size
	}
	style := frame.StyleMuted
	switch {
	case pct >= 90:
		style = frame.StyleError
	case pct >= 75:
		style = frame.StyleWarn
	}
	text := fmt.Sprintf("%d/%d (%d%%)", s.Used, s.Size, pct)
	return frame.Line{frame.S(style, text)}
}

// inboxLine is the operator-inbox badge. It wears StyleHITL — the approval
// card's role — because that is what it is: work that came back to nobody and
// is waiting on a human. It is the only badge on the bar that means "you are
// needed", and it must not read as another count of things going fine.
//
// The ASCII form spells the word out ("in:2") rather than picking a symbol.
// There is no single ASCII character that means "message waiting", and the two
// candidates a fallback would reach for are already spoken for on this bar:
// "m:" is the missions badge, and a bare "@" is the composer's file-mention
// sigil everywhere else in beam.
func inboxLine(s State) frame.Line {
	text := fmt.Sprintf("✉ %d", s.Inbox)
	if s.ASCII {
		text = fmt.Sprintf("in:%d", s.Inbox)
	}
	return frame.Line{frame.S(frame.StyleHITL, text)}
}

func missionsLine(s State) frame.Line {
	text := fmt.Sprintf("◇ %d", s.Missions)
	if s.ASCII {
		text = fmt.Sprintf("m:%d", s.Missions)
	}
	return frame.Line{frame.S(frame.StyleBrand, text)}
}

func modelLine(s State) frame.Line {
	text := s.Model
	if s.Provider != "" {
		text += metaSep(s.ASCII) + s.Provider
	}
	return frame.Line{frame.S(frame.StyleMuted, text)}
}

func healthLine(s State) frame.Line {
	style := frame.StyleWarn
	if s.Health == HealthError || s.Health == HealthDisconnected {
		style = frame.StyleError
	}
	return frame.Line{frame.S(style, s.Health)}
}

// metaSep joins a model to its provider. It is SPACED — "qwen3:8b · ollama"
// — which is the same form comp/brand's welcome header uses for the same
// pair. The two disagreed ("model · provider" up top, "model·provider" down
// here), and a user reading both in one screen had to work out whether they
// were being told the same thing twice or two different things.
func metaSep(ascii bool) string {
	if ascii {
		return " - "
	}
	return " · "
}

// countSuffix is the session segment's message count, or "" for a session
// with no turns yet.
//
// The two forms are not cosmetic variants of each other. In unicode the
// middot is visibly a separator ("beam-20a88ab8·2"), but the ASCII fallback
// used a hyphen — and a session label is USUALLY hyphenated already, so
// "beam-20a88ab8-2" read as an id ending in "-2" rather than as a label with
// two messages in it. The parenthesised form cannot be misread as part of the
// name it follows.
func countSuffix(ascii bool, messages int) string {
	if messages <= 0 {
		return ""
	}
	if ascii {
		return " (" + strconv.Itoa(messages) + ")"
	}
	return "·" + strconv.Itoa(messages)
}

// totalWidth is the cell width Render's present set would occupy: every
// present segment's own width plus one separator between each pair of
// present segments.
func totalWidth(segs []segment, present map[string]bool) int {
	total := 0
	n := 0
	for _, sg := range segs {
		if !present[sg.name] {
			continue
		}
		if n > 0 {
			total += textwidth.Width(separator)
		}
		total += textwidth.Width(sg.line.Text())
		n++
	}
	return total
}

// buildLine concatenates every present segment's spans, in render order,
// joined by the StyleNone separator.
func buildLine(segs []segment, present map[string]bool) frame.Line {
	var out frame.Line
	first := true
	for _, sg := range segs {
		if !present[sg.name] {
			continue
		}
		if !first {
			out = append(out, frame.S(frame.StyleNone, separator))
		}
		out = append(out, sg.line...)
		first = false
	}
	return out
}

// padLine right-pads l with a trailing StyleNone span so its rendered
// width is exactly width cells. l must already be <= width; padLine never
// truncates.
func padLine(l frame.Line, width int) frame.Line {
	w := textwidth.Width(l.Text())
	if w >= width {
		return l
	}
	return append(l, frame.S(frame.StyleNone, strings.Repeat(" ", width-w)))
}

// truncateLine cuts l to at most width cells, span-wise and rune-safe,
// with no ellipsis — Render's identity-alone last resort wants "as much
// as fits", not a marker that more was cut.
func truncateLine(l frame.Line, width int) frame.Line {
	if width <= 0 {
		return frame.Line{}
	}
	out := make(frame.Line, 0, len(l))
	used := 0
	for _, sp := range l {
		w := textwidth.Width(sp.Text)
		if used+w <= width {
			out = append(out, sp)
			used += w
			continue
		}
		if rem := width - used; rem > 0 {
			cut := textwidth.Truncate(sp.Text, rem, "")
			out = append(out, frame.S(sp.Style, cut))
			used += textwidth.Width(cut)
		}
		break
	}
	return out
}
