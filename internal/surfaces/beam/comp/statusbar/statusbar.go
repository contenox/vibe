// Package statusbar renders beam's persistent one-line status bar: identity,
// session, activity, context budget, operator inbox, missions and connection
// health. Render is a pure function of (width, State) — no terminal reads,
// no internal state — and never wraps or mid-truncates: whole segments drop
// in a fixed priority when the full set does not fit (see Render).
package statusbar

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/contenox/contenox/internal/surfaces/beam/comp/brand"
	"github.com/contenox/contenox/internal/surfaces/beam/frame"
	"github.com/contenox/contenox/internal/surfaces/beam/sanitize"
	"github.com/contenox/contenox/internal/surfaces/beam/sessionvitals"
	"github.com/contenox/contenox/internal/surfaces/beam/textwidth"
)

// Health is the closed vocabulary connection-lifecycle publishes, closed by
// convention rather than by a Go type: Render treats any other string as
// "not ready" too, rendered rather than panicking, so an unknown value
// degrades safely instead of hiding.
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

	// Session is the active session's display name; empty hides the
	// segment entirely.
	Session string
	// Messages is the turn count in Session, shown as "·N" (ASCII "-N")
	// appended to Session only when > 0.
	Messages int

	// Model and Provider name the active backend. Model empty hides the
	// segment; Provider is an additive "·provider" suffix.
	Model    string
	Provider string

	// Used and Size are context-window token counts. The gauge renders only
	// when Size > 0 — 0 means no usage_update has landed yet, and "0/0
	// (0%)" would be a lie, not an empty state.
	Used int
	Size int

	// Missions is the count of badge-worthy missions; the badge renders
	// only when >= 1.
	Missions int

	// Inbox is how many operator-inbox items have arrived since this beam
	// launched; the badge renders only when >= 1. It is deliberately
	// launch-relative: the inbox is a durable store spanning every beam
	// that ever ran, and answering "how many are unacked" needs a service
	// query (operatorinbox.List), not this component. It resets to zero on
	// relaunch and never decrements.
	Inbox int

	// Health is connection-lifecycle's state name. A health segment renders
	// only when Health is set and != HealthReady, the silent default.
	Health string

	// Activity is the liveness aggregate text, pre-built by the app; empty
	// hides the whole activity segment.
	Activity string
	// Spinner is the current spinner glyph for Activity, already
	// ASCII-safe when ASCII is true; statusbar renders it verbatim, unlike
	// the missions badge below.
	Spinner string
}

// Every string field above is treated as untrusted and sanitized at Render
// (see sanitizeState): none may carry a control character onto the one
// component that is on screen at all times.

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
// full set does not fit width, first to last; identity is absent since it
// is never dropped like the others (see Render). inbox drops one step
// ahead of missions: an inbox arrival already rang the bell, so that badge
// is the cheaper reminder to lose.
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
// trailing spaces to precisely width cells.
//
// Segments render left to right, each populated only when its data applies:
//
//	identity  brand.StatusSegment(s.ASCII) — always present.
//	session   s.Session, plus "·N" (ASCII " (N)") when Messages > 0.
//	          Present when Session != "".
//	activity  s.Spinner (brand-styled) + " " + s.Activity. Present when
//	          Activity != "".
//	gauge     "{Used}/{Size} ({pct}%)". Present only when Size > 0 (a
//	          zero Size means no usage reported yet).
//	inbox     "✉ N" (ASCII "in:N"), StyleHITL. Present only when Inbox >= 1.
//	missions  "◇ N" (ASCII "m:N"). Present only when Missions >= 1.
//	model     s.Model, plus " · "+Provider (ASCII " - "+Provider) when
//	          Provider != "". Present when Model != "".
//	health    s.Health verbatim. Present only when showHealth says it adds
//	          something.
//
// Present segments join on a two-space separator. When the full set does
// not fit width, whole segments drop — never wrapped or mid-truncated — in
// this fixed priority, first to last:
//
//	session, activity, inbox, missions, gauge, model, health, identity
//
// identity is last: every other segment drops to nothing before it does,
// and even then it renders truncated (not blank) rather than disappearing,
// so it is always the leftmost content of a non-empty line.
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
// hold. It runs at Render rather than at the app's assignment sites so this
// is the one place that can be sure it happened.
//
// The bar's segment arithmetic is cell counting on plain text, always padded
// to exactly width: a tab breaks that count, and a newline in a span
// violates frame.Line's one-row contract, so the engine would scroll the
// screen. sanitize.Line removes both, plus any escape sequence a misbehaving
// peer could smuggle in.
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
	if s.usage().Known() {
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

// showHealth is the health segment's presence rule: render only when it adds
// something. "ready" is the silent default and never renders. "working" is
// redundant with an Activity present, since the app publishes both from the
// same in-flight turn, so Activity wins there. Every other state renders
// regardless of what else is on the line.
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
// activity gets no leading space, so it starts at the separator instead of
// one cell past it.
func activityLine(s State) frame.Line {
	if s.Spinner == "" {
		return frame.Line{frame.S(frame.StyleNone, s.Activity)}
	}
	return frame.Line{
		frame.S(frame.StyleBrand, s.Spinner),
		frame.S(frame.StyleNone, " "+s.Activity),
	}
}

// usage is the State's context-window reading as the service models it. The
// numbers, the percentage and the thresholds all belong to sessionvitals; the
// bar only chooses a colour for the pressure it is handed.
func (s State) usage() sessionvitals.ContextUsage {
	return sessionvitals.ContextUsage{Used: s.Used, Size: s.Size}
}

func gaugeLine(s State) frame.Line {
	u := s.usage()
	style := frame.StyleMuted
	switch u.Pressure() {
	case sessionvitals.PressureCritical:
		style = frame.StyleError
	case sessionvitals.PressureHigh:
		style = frame.StyleWarn
	}
	return frame.Line{frame.S(style, u.Text())}
}

// inboxLine is the operator-inbox badge. It wears StyleHITL, the approval
// card's role, since it is the one badge meaning "you are needed" rather
// than another count of things going fine. ASCII spells the word out
// ("in:2") since "m:" (missions) and "@" (mention sigil) are already taken
// on this bar.
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

// metaSep joins a model to its provider, spaced ("qwen3:8b · ollama") to
// match comp/brand's welcome header for the same pair.
func metaSep(ascii bool) string {
	if ascii {
		return " - "
	}
	return " · "
}

// countSuffix is the session segment's message count, or "" with no turns
// yet. The ASCII form is parenthesized rather than hyphenated, since a
// session id is usually hyphenated already and "beam-20a88ab8-2" would read
// as part of the name rather than a count.
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
