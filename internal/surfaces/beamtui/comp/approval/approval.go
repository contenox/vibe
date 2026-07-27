// Package approval renders beam's in-session HITL card: what is being
// asked (tool, arguments, triggering policy rule, and the diff), and the
// three states it can be in — pending, resolved, cancelled.
//
// The card is the TUI counterpart of the CLI's approval prompt
// (internal/surfaces/contenoxcli/hitl_tty.go) and deliberately keeps its
// ordering: identity, then arguments in a STABLE sorted order, then the
// policy that gated the call, and the diff LAST so the most
// decision-relevant content sits closest to the y/n line. A human
// pattern-matching under fatigue should not have to re-find the fields.
//
// Two properties carry over from the CLI verbatim. Argument values are
// summarised, never dumped, so a 4 KB replacement cannot push the diff off
// screen; and a diff longer than the display cap is truncated with a
// warning that says what approving it would mean, not merely that lines
// were hidden.
//
// What this package does NOT do: decide anything. Resolve/MarkCancelled are
// state transitions the app drives from keystrokes, and while a card is
// pending the composer is modal-blocked by the app shell, not from here.
// There is no client-side "always allow" memory of any kind — the codebase
// invariant that every gated call reaches a human (hitl_tty.go's doc
// comment, blueprint 4.14 item 7) holds here too.
//
// The detached-mission approval queue (item 4) is a separate surface and is
// not part of this package.
package approval

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/contenox/beam/internal/services/approvalflow"
	"github.com/contenox/beam/internal/surfaces/beamtui/enginebridge"
	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/sanitize"
	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
)

const (
	// maxDiffLines bounds the rendered diff, matching hitl_tty.go's cap.
	// When it bites, the notice is the last thing above the decision line,
	// where it cannot be scrolled past unnoticed.
	maxDiffLines = 120

	// maxArgValueDisplay bounds one argument value, matching hitl_tty.go's
	// summariseArg. Anything longer reads as a head plus its true size.
	maxArgValueDisplay = 240

	// maxArgBlockLines bounds a multi-line argument rendered as a block. Forty
	// is enough for the whole of a script a human would actually read before
	// approving it, and small enough that a 4 KB body cannot push the rest of
	// the card off screen. When it bites it says so, in the same words the
	// diff cap uses: what approving would mean, not merely that lines were
	// hidden.
	maxArgBlockLines = 40

	// maxMayCallNames bounds the declared-reach line. A script that names more
	// tools than this has stopped being reviewable as a list; the count of
	// what is not shown is what the operator needs then.
	maxMayCallNames = 8

	// argBlockIndent puts a block's body under its key without letting it be
	// mistaken for card chrome.
	argBlockIndent = "    "

	// diffTabStop is what a tab in a diff body — or in a multi-line argument
	// block, which is source text by the same argument — is worth. Eight is what git,
	// every pager and the editor the change came from already assume, so an
	// expanded tab puts the code under review in the columns the author saw
	// it in. Folding it to one space (the rule for a name) would silently
	// re-indent the very lines being approved.
	diffTabStop = 8
)

// ASCIIOk and ASCIINo are the decision glyphs a Mono terminal sees, exported
// so testkit's glyph-parity test can hold them against the style package's
// GlyphSet and comp/transcript's card markers. A "+" that means "allowed"
// here and something else on a settled tool card is a legibility bug in
// exactly the terminals that have no color to fall back on.
const (
	ASCIIOk = "+"
	ASCIINo = "x"
)

const (
	headerUnicode = "◆"
	headerASCII   = "*"

	warnUnicode = "⚠"
	warnASCII   = "!"

	okUnicode   = "✓"
	okASCII     = ASCIIOk
	noUnicode   = "✗"
	noASCII     = ASCIINo
	dashUnicode = "—"
	dashASCII   = "-"

	sepUnicode = " · "
	sepASCII   = " - "

	ellipsisUnicode = "…"
	ellipsisASCII   = "..."
)

// State is where a card sits in its lifecycle.
type State int

const (
	// StatePending is the blocking state: the tool call is stopped and the
	// live region belongs to this card.
	StatePending State = iota
	// StateResolved means the operator answered — see Allowed.
	StateResolved
	// StateCancelled means the turn was cancelled out from under the card
	// (blueprint 4.14 item 10). A cancelled card stops spinning and says so
	// rather than pretending it is still waiting for a keystroke.
	StateCancelled
)

func (s State) String() string {
	switch s {
	case StateResolved:
		return "resolved"
	case StateCancelled:
		return "cancelled"
	}
	return "pending"
}

// Card is one in-session approval ask. Tool calls execute sequentially per
// turn, so at most one card is ever pending (item 2) — this type carries no
// queue.
//
// A Card is owned by the UI goroutine. The underlying Resolve func is
// itself goroutine-safe and idempotent, but the card's own state is not
// guarded.
type Card struct {
	ev enginebridge.PermissionRequested

	// args is the decoded RawInput when it is a JSON object; argKeys is its
	// sorted key order, computed once so every render of one card lists its
	// arguments identically.
	args    map[string]any
	argKeys []string
	// rawArgs holds RawInput verbatim when it is not a JSON object — a
	// shape this renderer does not understand is shown, not dropped.
	rawArgs string

	state   State
	allowed bool
}

// New builds a pending card from the bridge event. It decodes RawInput once
// here rather than on every render, so a resize re-lays-out the card
// without re-parsing the request.
func New(ev enginebridge.PermissionRequested) *Card {
	c := &Card{ev: ev, state: StatePending}
	if len(ev.RawInput) > 0 {
		var m map[string]any
		if err := json.Unmarshal(ev.RawInput, &m); err == nil && m != nil {
			c.args = m
			c.argKeys = make([]string, 0, len(m))
			for k := range m {
				c.argKeys = append(c.argKeys, k)
			}
			sort.Strings(c.argKeys)
		} else {
			c.rawArgs = strings.TrimSpace(string(ev.RawInput))
		}
	}
	return c
}

// Resolve answers the ask and is idempotent: the first call wins, and the
// underlying bridge Resolve is invoked EXACTLY once. A card already
// resolved or cancelled ignores further calls, so a doubled keystroke
// cannot answer the next ask.
func (c *Card) Resolve(allow bool) {
	if c.state != StatePending {
		return
	}
	c.state = StateResolved
	c.allowed = allow
	if c.ev.Resolve != nil {
		c.ev.Resolve(allow)
	}
}

// MarkCancelled flips a pending card to cancelled when the turn is
// cancelled.
//
// It deliberately does NOT call the underlying Resolve: the bridge's
// cancellation path already resolves outstanding permissions as cancelled,
// and answering here would put an allow/deny on the wire that the operator
// never gave. An already-resolved card is left alone — the decision stands.
func (c *Card) MarkCancelled() {
	if c.state != StatePending {
		return
	}
	c.state = StateCancelled
}

// State reports the card's lifecycle state.
func (c *Card) State() State { return c.state }

// Allowed reports the decision. It is meaningful only in StateResolved.
func (c *Card) Allowed() bool { return c.allowed }

// ToolCallID is the id of the gated call, so the app can match a card
// against later tool-call events without keeping a side table.
func (c *Card) ToolCallID() string { return c.ev.ToolCallID }

// Render draws the card at width. spinner is the caller's current activity
// glyph (empty for none), used only while pending — a resolved card never
// animates.
//
// Every line fits width EXCEPT the two that carry source text — diff body
// lines and the body of a multi-line argument block — which are emitted
// unwrapped and verbatim, exactly like transcript code lines: a wrapped or
// elided line of either copies out of the terminal as something that is not
// what is under review (blueprint D1's copy-cleanliness rule, item 8).
func (c *Card) Render(width int, ascii bool, spinner string) []frame.Line {
	if width <= 0 {
		return nil
	}

	out := make([]frame.Line, 0, 8)
	add := func(l frame.Line) { out = append(out, clamp(l, width, ascii)) }

	add(frame.Styled(frame.StyleHITL, headerGlyph(ascii)+" approval required"))
	add(frame.L(
		frame.S(frame.StyleMuted, "tool  "),
		frame.S(frame.StyleNone, c.toolIdentity()),
	))

	// The reach sits with the identity, not with the arguments: it says what
	// this tool IS able to touch, which is the question the arguments of a
	// script tool cannot answer.
	if r := mayCallText(c.ev.Meta, ascii); r != "" {
		add(frame.Styled(frame.StyleMuted, r))
	}

	switch {
	case c.args != nil:
		for _, k := range c.argKeys {
			out = append(out, c.argLines(k, width, ascii)...)
		}
	case c.rawArgs != "":
		add(frame.L(
			frame.S(frame.StyleMuted, "  "),
			frame.S(frame.StyleNone, summarizeValue(c.rawArgs, width-2, ascii)),
		))
	}

	if p := policyText(c.ev.Meta, ascii); p != "" {
		add(frame.Styled(frame.StyleMuted, p))
	}

	// Diff last: closest to the decision line, which is the whole point of
	// the ordering the CLI established.
	out = append(out, c.diffLines(width, ascii)...)
	out = append(out, clamp(c.footer(ascii, spinner), width, ascii))
	return out
}

// diffLines renders the change under review, or the one-line stand-in when
// only new content is known. Never a blank diff section (item 1).
func (c *Card) diffLines(width int, ascii bool) []frame.Line {
	diff := c.ev.Meta.Diff
	if diff == "" {
		// No unified diff, but a new body: say how much is arriving rather
		// than dumping content that was never diffed.
		if n := lineCount(c.ev.Meta.DiffNew); n > 0 {
			return []frame.Line{clamp(
				frame.Styled(frame.StyleMuted, fmt.Sprintf("new content (%d lines)", n)),
				width, ascii,
			)}
		}
		return nil
	}

	lines := strings.Split(strings.TrimSuffix(diff, "\n"), "\n")
	shown := lines
	if len(lines) > maxDiffLines {
		shown = lines[:maxDiffLines]
	}

	out := make([]frame.Line, 0, len(shown)+2)
	for _, l := range shown {
		// Unclamped and unwrapped: no elision marker may ever appear inside
		// a diff body line, and a line this card split would copy out of the
		// terminal as something that is not the change under review.
		//
		// Sanitized, though. A diff is the most attacker-controlled string on
		// this card — it is content out of a repository, presented to a human
		// as the exact thing they are about to approve — so a bidi override
		// that displays the line as the reverse of what it applies, or a CSI
		// that erases the lines above it, is a defect with the whole HITL gate
		// as its blast radius. Tabs EXPAND rather than fold, because
		// indentation is part of what is being reviewed.
		s := sanitize.ExpandTabs(sanitize.Lines(l), diffTabStop)
		out = append(out, frame.Styled(diffStyle(s), s))
	}
	if len(lines) > maxDiffLines {
		// The warning is wrapped, never truncated: this is the one line
		// whose full sentence is load-bearing at any width.
		for _, w := range textwidth.Wrap(truncationWarning(len(lines), ascii), width) {
			out = append(out, frame.Styled(frame.StyleWarn, w))
		}
	}
	return out
}

// truncationWarning states the consequence, not just the arithmetic — the
// CLI's tone, kept word for word in spirit: approving means accepting
// changes you have not seen.
func truncationWarning(total int, ascii bool) string {
	return fmt.Sprintf("%s diff truncated: showing %d of %d %s approving accepts changes you have not seen",
		warnGlyph(ascii), maxDiffLines, total, dashGlyph(ascii))
}

// footer is the decision line, or what the decision was.
func (c *Card) footer(ascii bool, spinner string) frame.Line {
	switch c.state {
	case StateResolved:
		if c.allowed {
			return frame.Styled(frame.StyleDone, okGlyph(ascii)+" allowed")
		}
		return frame.Styled(frame.StyleError, noGlyph(ascii)+" denied")
	case StateCancelled:
		return frame.Styled(frame.StyleMuted, dashGlyph(ascii)+" cancelled")
	}

	l := make(frame.Line, 0, 6)
	if spinner != "" {
		l = append(l, frame.S(frame.StylePending, spinner), frame.S(frame.StyleNone, " "))
	}
	return append(l,
		frame.S(frame.StyleStrong, "y"),
		frame.S(frame.StyleMuted, " allow"+sep(ascii)),
		frame.S(frame.StyleStrong, "n"),
		frame.S(frame.StyleMuted, " deny"+sep(ascii)+"Esc cancels turn"),
	)
}

// toolIdentity is what is being asked about. approvalflow's Meta is the
// authority; the event Title is the fallback for a peer that sent no
// _meta.
//
// Every branch is peer-supplied, so the answer is sanitized on the way out —
// this is the line that names what the operator is about to authorise, and it
// must not be able to rewrite the card around itself.
func (c *Card) toolIdentity() string {
	m := c.ev.Meta
	switch {
	case m.ToolsName != "" && m.ToolName != "":
		return sanitize.Line(m.ToolsName + "." + m.ToolName)
	case m.ToolName != "":
		return sanitize.Line(m.ToolName)
	case m.ToolsName != "":
		return sanitize.Line(m.ToolsName)
	case c.ev.Title != "":
		return sanitize.Line(c.ev.Title)
	}
	return "unknown tool"
}

// argLines renders one argument row — or, for a value that is itself source
// text, the block that value deserves.
//
// The scalar row is the default and stays the default: it is what keeps a 4 KB
// replacement from pushing the diff off screen. But a value with newlines in it
// is code — a script body, a heredoc, a patch — and squeezing code onto one row
// means writing its newlines out as literal "\n" and then cutting the result to
// fit. That renders the ONE argument most in need of reading as the least
// readable thing on the card, which is the exact failure the card exists to
// prevent (found by dogfooding a goja_eval call).
//
// The block is suppressed when the card carries a rendered diff, because then
// the scalar summary's "see diff" is TRUE and the diff below is the better,
// unduplicated rendering of the same bytes. With no diff, nothing else on the
// card shows the content, so the block is the only honest place to read it.
func (c *Card) argLines(k string, width int, ascii bool) []frame.Line {
	if body, ok := c.argBlockBody(k); ok {
		return c.blockLines(k, body, width, ascii)
	}
	prefix := "  " + k + " = "
	return []frame.Line{clamp(frame.L(
		frame.S(frame.StyleMuted, prefix),
		frame.S(frame.StyleNone, c.argText(k, width-textwidth.Width(prefix), ascii)),
	), width, ascii)}
}

// argBlockBody reports the source text of an argument that should render as a
// block, and whether it should. See argLines for the rule.
func (c *Card) argBlockBody(k string) (string, bool) {
	if c.ev.Meta.Diff != "" {
		return "", false
	}
	s, ok := c.args[k].(string)
	if !ok || !strings.Contains(s, "\n") {
		return "", false
	}
	return s, true
}

// blockLines lays a multi-line argument out under its key: one source line per
// frame line, at the block indent.
//
// Body lines take the diff's treatment exactly — unwrapped, sanitized, tabs
// expanded — for the diff's reasons. Unwrapped, because a line this card split
// would copy out of the terminal as something that is not what will run.
// Sanitized, because the body is peer-supplied text presented to a human as the
// thing they are about to authorise, so a CSI that erases the lines above it or
// a bidi override that displays a line as the reverse of what it does is a
// defect with the whole HITL gate as its blast radius. Tabs EXPAND rather than
// fold, because indentation is part of what is being read.
func (c *Card) blockLines(k, body string, width int, ascii bool) []frame.Line {
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	shown := lines
	if len(shown) > maxArgBlockLines {
		shown = shown[:maxArgBlockLines]
	}

	out := make([]frame.Line, 0, len(shown)+3)
	out = append(out, clamp(frame.Styled(frame.StyleMuted, "  "+sanitize.Line(k)+" ="), width, ascii))
	for _, l := range shown {
		s := sanitize.ExpandTabs(sanitize.Lines(l), diffTabStop)
		out = append(out, frame.Styled(frame.StyleCode, argBlockIndent+s))
	}
	if hidden := len(lines) - len(shown); hidden > 0 {
		// Wrapped, never truncated, and unindented — the same treatment the
		// diff's own cap notice gets, because it is the same sentence: the part
		// that says what approving would mean is load-bearing at any width.
		for _, w := range textwidth.Wrap(blockTruncationWarning(hidden, ascii), width) {
			out = append(out, frame.Styled(frame.StyleWarn, w))
		}
	}
	return out
}

// blockTruncationWarning states the consequence, not just the arithmetic —
// truncationWarning's sentence, for content rather than for a diff.
func blockTruncationWarning(hidden int, ascii bool) string {
	return fmt.Sprintf("%s +%d more lines %s approving accepts content you have not seen",
		warnGlyph(ascii), hidden, dashGlyph(ascii))
}

// mayCallText is the declared-reach line: the tools this call may itself reach
// while it runs (approvalflow.Meta.MayCall).
//
// A script tool is the one gated call whose arguments do not say what it will
// do. The tools it calls raise their own cards when they are gated — but an
// ALLOW-tier call raises none, so without this line an operator approving a
// script cannot know it will read files or run git.
//
// It is rendered muted and as a DECLARATION, never as a guarantee: the wording
// is "may call", the names are the author's, and nothing here enforces them.
// The list is capped by count with the remainder stated, so a script that names
// forty tools cannot push the decision off screen — and cannot hide how many it
// named either.
//
// All three states of Meta.MayCallDeclared read differently, because they mean
// different things (see that field): an undeclared reach is unbounded and says
// so, a declared-empty reach is a promise and says that, and no information at
// all — every ordinary tool card — says nothing rather than implying either.
func mayCallText(m approvalflow.Meta, ascii bool) string {
	names := make([]string, 0, len(m.MayCall))
	seen := make(map[string]struct{}, len(m.MayCall))
	for _, raw := range m.MayCall {
		n := strings.TrimSpace(sanitize.Line(raw))
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}

	if len(names) == 0 {
		switch {
		case m.MayCallDeclared == nil:
			return ""
		case *m.MayCallDeclared:
			return "may call  nothing"
		default:
			return "may call  any tool the policy allows" + sep(ascii) + "nothing declared"
		}
	}

	hidden := 0
	if len(names) > maxMayCallNames {
		hidden = len(names) - maxMayCallNames
		names = names[:maxMayCallNames]
	}
	out := "may call  " + strings.Join(names, sep(ascii))
	if hidden > 0 {
		out += fmt.Sprintf("%s+%d more", sep(ascii), hidden)
	}
	return out
}

// argText renders one argument value.
//
// The service's own summariser gets first refusal: approvalflow.
// SummarizeToolCallArgs already knows which key carries the meaning for a
// given tool (a path, a command line, a URL, a grep pattern) and bounds it
// the way every other surface bounds it. Only when it declines — a key it
// has no opinion about, like a file body — does the card fall back to the
// generic elision, which mirrors hitl_tty.go's summariseArg.
func (c *Card) argText(k string, budget int, ascii bool) string {
	v := c.args[k]
	if s := approvalflow.SummarizeToolCallArgs(c.ev.Meta.ToolName, map[string]any{k: v}); s != "" {
		return sanitize.Line(s)
	}
	return summarizeValue(valueText(v), budget, ascii)
}

// valueText flattens a decoded JSON value to reviewable text. Non-strings
// are re-marshalled rather than printed with %v, so a nested object reads
// as JSON the operator can compare against the call they expected.
func valueText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if raw, err := json.Marshal(v); err == nil {
		return string(raw)
	}
	return fmt.Sprintf("%v", v)
}

// summarizeValue elides a long or multi-line value, reporting its true size
// so the elision is VISIBLE rather than silent — hitl_tty.go's summariseArg
// with three changes: the cut is rune-safe (a byte cut can split a rune,
// and a frame span must stay valid text), the ellipsis degrades in ASCII,
// and the head is cut to the caller's remaining budget.
//
// That last one matters more than it looks. The CLI can spend 240
// characters on a head because its terminal wraps; a card line is clamped,
// so a fixed-size head would push the "[N bytes, M lines]" marker off the
// right edge — losing exactly the part that tells the operator something
// was hidden. The head yields; the marker does not.
func summarizeValue(s string, budget int, ascii bool) string {
	if s == "" {
		return ""
	}
	total := len(s)
	lines := strings.Count(s, "\n") + 1
	if total <= maxArgValueDisplay && lines == 1 {
		return sanitize.Line(s)
	}

	head := s
	if r := []rune(head); len(r) > maxArgValueDisplay {
		head = string(r[:maxArgValueDisplay])
	}
	// The literal "\n" is written out before sanitizing so a multi-line value
	// still READS as multi-line on its one row; sanitize.Line would otherwise
	// splice the lines together with no sign there had been a break.
	head = sanitize.Line(strings.ReplaceAll(head, "\n", "\\n"))

	marker := fmt.Sprintf("%s [%d bytes, %d lines %s see diff]",
		ellipsis(ascii), total, lines, dashGlyph(ascii))
	if budget > 0 {
		room := budget - textwidth.Width(marker)
		if room < 0 {
			room = 0
		}
		head = textwidth.Truncate(head, room, "")
	}
	return head + marker
}

// policyText names the rule that gated the call. Both halves are optional:
// a policy without a matched rule path is still worth saying.
func policyText(m approvalflow.Meta, ascii bool) string {
	name, path := sanitize.Line(m.PolicyName), sanitize.Line(m.PolicyPath)
	switch {
	case name != "" && path != "":
		return "policy " + name + sep(ascii) + "rule " + path
	case name != "":
		return "policy " + name
	case path != "":
		return "rule " + path
	}
	return ""
}

// diffStyle classifies a diff body line by its own first character, so
// meaning survives NO_COLOR: the +/- the line already carries is the
// signal, and the style only reinforces it.
func diffStyle(line string) frame.StyleID {
	if line == "" {
		return frame.StyleMuted
	}
	switch line[0] {
	case '+':
		return frame.StyleDone
	case '-':
		return frame.StyleError
	}
	return frame.StyleMuted
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
}

func headerGlyph(ascii bool) string {
	if ascii {
		return headerASCII
	}
	return headerUnicode
}

func warnGlyph(ascii bool) string {
	if ascii {
		return warnASCII
	}
	return warnUnicode
}

func okGlyph(ascii bool) string {
	if ascii {
		return okASCII
	}
	return okUnicode
}

func noGlyph(ascii bool) string {
	if ascii {
		return noASCII
	}
	return noUnicode
}

func dashGlyph(ascii bool) string {
	if ascii {
		return dashASCII
	}
	return dashUnicode
}

func sep(ascii bool) string {
	if ascii {
		return sepASCII
	}
	return sepUnicode
}

func ellipsis(ascii bool) string {
	if ascii {
		return ellipsisASCII
	}
	return ellipsisUnicode
}

// clamp cuts l to at most width cells, rune-safely and span-wise, marking
// the cut with an ellipsis when one fits. It is never applied to a diff
// body line.
func clamp(l frame.Line, width int, ascii bool) frame.Line {
	if width <= 0 {
		return frame.Line{}
	}
	if textwidth.Width(l.Text()) <= width {
		return l
	}

	tail := ellipsis(ascii)
	if textwidth.Width(tail) > width {
		tail = ""
	}
	budget := width - textwidth.Width(tail)

	out := make(frame.Line, 0, len(l)+1)
	used := 0
	for _, s := range l {
		w := textwidth.Width(s.Text)
		if used+w <= budget {
			out = append(out, s)
			used += w
			continue
		}
		if rem := budget - used; rem > 0 {
			out = append(out, frame.S(s.Style, textwidth.Truncate(s.Text, rem, "")))
		}
		break
	}
	if tail != "" {
		out = append(out, frame.S(frame.StyleMuted, tail))
	}
	return out
}
