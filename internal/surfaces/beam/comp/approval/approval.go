// Package approval renders beam's in-session HITL card: tool, arguments,
// policy rule, and diff, in that order — diff last, closest to the y/n
// line — across pending/resolved/cancelled. It decides nothing itself:
// Resolve/MarkCancelled/MarkDetached are app-driven state transitions, and
// there is no client-side "always allow" memory — every gated call reaches a
// human.
package approval

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/surfaces/beam/dialect"
	"github.com/contenox/contenox/internal/surfaces/beam/enginebridge"
	"github.com/contenox/contenox/internal/surfaces/beam/frame"
	"github.com/contenox/contenox/internal/surfaces/beam/sanitize"
	"github.com/contenox/contenox/internal/surfaces/beam/textwidth"
)

const (
	// maxDiffLines bounds the rendered diff, matching hitl_tty.go's cap.
	maxDiffLines = 120

	// maxArgValueDisplay bounds one argument value, matching hitl_tty.go's summariseArg.
	maxArgValueDisplay = 240

	// maxArgBlockLines bounds a multi-line argument block: enough for a
	// script a human would read before approving, small enough that a 4 KB
	// body can't push the rest of the card off screen.
	maxArgBlockLines = 40

	// maxMayCallNames bounds the declared-reach line; beyond this the list
	// stops being reviewable, so the remainder is stated as a count instead.
	maxMayCallNames = 8

	// argBlockIndent puts a block's body under its key, distinct from card chrome.
	argBlockIndent = "    "

	// diffTabStop expands a tab (in a diff or an argument block) to the width
	// git and editors already assume, so code under review keeps its
	// original columns instead of being silently re-indented.
	diffTabStop = 8
)

// ASCIIOk and ASCIINo are the decision glyphs on a Mono terminal, exported
// so testkit's glyph-parity test can check them against style's GlyphSet
// and comp/transcript's card markers.
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
	// StatePending blocks: the tool call is stopped and the live region
	// belongs to this card.
	StatePending State = iota
	// StateResolved means the operator answered — see Allowed.
	StateResolved
	// StateCancelled means the turn was cancelled out from under the card;
	// it stops spinning rather than pretending to still wait for a keystroke.
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
// turn, so at most one card is ever pending — this type carries no queue.
// A Card is owned by the UI goroutine; the underlying Resolve func is
// goroutine-safe and idempotent, but the card's own state is not guarded.
type Card struct {
	ev enginebridge.PermissionRequested

	// args is the decoded RawInput when it is a JSON object; argKeys is its
	// sorted key order, computed once so every render lists arguments
	// identically.
	args    map[string]any
	argKeys []string
	// rawArgs holds RawInput verbatim when it is not a JSON object, shown
	// rather than dropped.
	rawArgs string

	state   State
	allowed bool
	// detached marks a card no turn in this session is waiting on: the turn
	// that raised it ended with the ask still open, which is what a gated
	// call does only by suspending. Answering works exactly the same — the
	// verdict lands on the same durable row — but there is no turn here for
	// Esc to cancel, so the decision line stops offering one.
	detached bool
}

// New builds a pending card from the bridge event, decoding RawInput once so
// a resize re-lays-out the card without re-parsing the request.
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

// Resolve answers the ask and is idempotent: the first call wins, the
// underlying bridge Resolve fires exactly once, and a card already resolved
// or cancelled ignores further calls.
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
// cancelled. It does not call the underlying Resolve — the bridge's own
// cancellation path already resolves outstanding permissions, and calling
// here would put an allow/deny on the wire the operator never gave. An
// already-resolved card is left alone.
func (c *Card) MarkCancelled() {
	if c.state != StatePending {
		return
	}
	c.state = StateCancelled
}

// MarkDetached records that the turn that raised this card has ended with the
// ask still open, so nothing in this session is waiting on it. The ask itself
// is untouched and still answerable — that is what resumes the suspended run —
// so this changes only what the decision line promises. A card already resolved
// or cancelled is left alone: its footer is a verdict, not an offer.
func (c *Card) MarkDetached() {
	if c.state != StatePending {
		return
	}
	c.detached = true
}

// Detached reports whether the card outlived the turn that raised it.
// See MarkDetached.
func (c *Card) Detached() bool { return c.detached }

// State reports the card's lifecycle state.
func (c *Card) State() State { return c.state }

// Allowed reports the decision. It is meaningful only in StateResolved.
func (c *Card) Allowed() bool { return c.allowed }

// ToolCallID is the id of the gated call, so the app can match a card
// against later tool-call events without keeping a side table.
func (c *Card) ToolCallID() string { return c.ev.ToolCallID }

// Render draws the whole card at width: the ask (see Ask) followed by the
// decision line. spinner is the current activity glyph (empty for none),
// shown only while pending.
//
// It is the card as one block, for a surface with the rows to spend on it.
// A bounded live region cannot promise those rows — an over-tall live region
// is kept by its tail, and a live region never enters scrollback — so beam's
// app-shell settles Ask into scrollback and keeps only Prompt live.
func (c *Card) Render(width int, ascii bool, spinner string) []frame.Line {
	if width <= 0 {
		return nil
	}
	return append(c.Ask(width, ascii), clamp(c.footer(ascii, spinner), width, ascii))
}

// Ask is everything the operator needs to read before answering: header,
// tool identity, declared reach, arguments, policy, and the diff — the card
// minus its decision line. It is complete on arrival and never changes, so a
// caller settles it once into scrollback where no row budget can clip it.
//
// Every line fits width except diff body lines and multi-line argument
// block bodies, which are emitted unwrapped and verbatim: a wrapped or
// elided line of either would copy out of the terminal as something that is
// not what is under review.
func (c *Card) Ask(width int, ascii bool) []frame.Line {
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

	// The reach sits with the identity, not the arguments: it answers what a
	// script tool's own arguments cannot.
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

	// Diff last: closest to the decision line, per the CLI's ordering.
	out = append(out, c.diffLines(width, ascii)...)
	return out
}

// Prompt is the card's bounded live block: the subject line naming what is
// being authorised, then the decision line. Exactly [PromptRows] lines at any
// width, so it fits every live region the composer and status bar leave
// room for — a y/n prompt whose subject scrolled away is not a decision an
// operator can make.
func (c *Card) Prompt(width int, ascii bool, spinner string) []frame.Line {
	if width <= 0 {
		return nil
	}
	subject := frame.L(
		frame.S(frame.StyleHITL, headerGlyph(ascii)+" approval required"),
		frame.S(frame.StyleMuted, sep(ascii)),
		frame.S(frame.StyleNone, c.toolIdentity()),
	)
	if target := c.target(); target != "" {
		subject = append(subject, frame.S(frame.StyleMuted, sep(ascii)+target))
	}
	return []frame.Line{
		clamp(subject, width, ascii),
		clamp(c.footer(ascii, spinner), width, ascii),
	}
}

// PromptRows is how many rows Prompt occupies, so a caller can budget for it
// without rendering first.
const PromptRows = 2

// Record is the one line a card leaves behind once it is answered: the
// verdict, what it was about, and the policy that asked. Answering otherwise
// leaves no trace at all — the card is dropped and its own resolved footer
// is never drawn — so the transcript would show a gate that opened for no
// recorded reason. A still-pending card has no verdict to record and returns
// nil; Resolve or MarkCancelled first.
func (c *Card) Record(width int, ascii bool) frame.Line {
	if c.state == StatePending {
		return nil
	}
	verdict := c.footer(ascii, "")
	l := make(frame.Line, 0, len(verdict)+4)
	l = append(l, verdict...)
	l = append(l, frame.S(frame.StyleMuted, sep(ascii)), frame.S(frame.StyleNone, c.toolIdentity()))
	if target := c.target(); target != "" {
		l = append(l, frame.S(frame.StyleMuted, sep(ascii)+target))
	}
	if name := sanitize.Line(c.ev.Meta.PolicyName); name != "" {
		l = append(l, frame.S(frame.StyleMuted, sep(ascii)+"policy "+name))
	}
	return clamp(l, width, ascii)
}

// target names what the call acts on, for the one-line surfaces (Prompt,
// Record) that have no room for the argument list. approvalflow decides
// which argument carries the meaning for a given tool; when it declines
// there is no honest one-word answer and the line simply omits it.
func (c *Card) target() string {
	if len(c.args) == 0 {
		return ""
	}
	return sanitize.Line(dialect.SummarizeToolCallArgs(c.ev.Meta.ToolName, c.args))
}

// diffLines renders the change under review, or a one-line stand-in when
// only new content is known — never a blank diff section.
func (c *Card) diffLines(width int, ascii bool) []frame.Line {
	diff := c.ev.Meta.Diff
	if diff == "" {
		// No diff, but new content exists: say how much rather than dumping it.
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
		// Unwrapped and unclamped so no elision marker appears inside a diff
		// line, but still sanitized: a diff is attacker-controlled content
		// shown to a human as the exact thing they are about to approve, so a
		// bidi override or a screen-clearing CSI here is a defect with the
		// whole HITL gate as its blast radius. Tabs expand rather than fold,
		// since indentation is part of what is being reviewed.
		s := sanitize.ExpandTabs(sanitize.Lines(l), diffTabStop)
		out = append(out, frame.Styled(diffStyle(s), s))
	}
	if len(lines) > maxDiffLines {
		// Wrapped, never truncated: this sentence is load-bearing at any width.
		for _, w := range textwidth.Wrap(truncationWarning(len(lines), ascii), width) {
			out = append(out, frame.Styled(frame.StyleWarn, w))
		}
	}
	return out
}

// truncationWarning states the consequence, not just the arithmetic:
// approving means accepting changes you have not seen.
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
	// The third segment is the only part a detached card must not repeat:
	// offering to cancel a turn that already ended is the card claiming work
	// nothing here is doing. What answering does instead is the honest hint,
	// and it is the same key either way.
	hint := "Esc cancels turn"
	if c.detached {
		hint = "answering resumes the run"
	}
	return append(l,
		frame.S(frame.StyleStrong, "y"),
		frame.S(frame.StyleMuted, " allow"+sep(ascii)),
		frame.S(frame.StyleStrong, "n"),
		frame.S(frame.StyleMuted, " deny"+sep(ascii)+hint),
	)
}

// toolIdentity is what is being asked about: approvalflow's Meta is the
// authority, the event Title the fallback for a peer that sent no _meta.
// Every branch is peer-supplied and sanitized on the way out, since this is
// the line naming what the operator is about to authorise.
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

// argLines renders one argument row, or — for a value that is itself source
// text — the block that value deserves. The scalar row stays the default so
// a 4 KB replacement cannot push the diff off screen; a multi-line value is
// code, and squeezing it onto one row would make the one argument most in
// need of reading the least readable thing on the card. The block is
// suppressed when a diff is also rendered, since the diff is then the
// better, unduplicated view of the same bytes.
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

// blockLines lays a multi-line argument out under its key: one source line
// per frame line, at the block indent, given the diff body's exact
// treatment (unwrapped, sanitized, tabs expanded) and for the same reasons.
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
		// Wrapped, never truncated, unindented — same treatment as the diff's cap notice.
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

// mayCallText is the declared-reach line: the tools this call may itself
// reach while it runs (dialect.Meta.MayCall). A script tool's own
// arguments don't say what it will do, and an ALLOW-tier sub-call raises no
// card of its own, so this is the only place an operator learns it. Rendered
// as a declaration, never a guarantee — nothing here enforces the names —
// and capped by count so a long list can't push the decision off screen.
// Meta.MayCallDeclared's three states read differently: undeclared is
// unbounded, declared-empty is a promise, and no information stays silent.
func mayCallText(m dialect.Meta, ascii bool) string {
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

// argText renders one argument value. dialect.SummarizeToolCallArgs
// gets first refusal, since it knows which key carries the meaning for a
// given tool; only when it declines does this fall back to generic elision.
func (c *Card) argText(k string, budget int, ascii bool) string {
	v := c.args[k]
	if s := dialect.SummarizeToolCallArgs(c.ev.Meta.ToolName, map[string]any{k: v}); s != "" {
		return sanitize.Line(s)
	}
	return summarizeValue(valueText(v), budget, ascii)
}

// valueText flattens a decoded JSON value to reviewable text. Non-strings
// are re-marshalled rather than printed with %v, so nested objects read as JSON.
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
// so the elision is visible rather than silent. The cut is rune-safe, the
// ellipsis degrades in ASCII, and the head is cut to the caller's remaining
// budget: a card line is clamped, so a fixed-size head would push the "[N
// bytes, M lines]" marker off the edge — the head yields, the marker never does.
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
	// Written out as literal "\n" before sanitizing, so a multi-line value
	// still reads as multi-line on its one row instead of being silently spliced.
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

// policyText names the policy that gated the call, its file, and why: the
// matched rule's human-readable cause when it has one (e.g. `shell command
// "rm" matched command_ask_always`), else which rule fired, else that none
// did. The third segment only appears once a named policy is known — a bare
// MatchedRule with no policy would be an index into a document the card
// never identified. Detail displaces the rule index rather than piling onto
// it: an index tells a human almost nothing next to the actual cause. When
// Detail is empty, it reads as the two cases dialect.Meta.MatchedRule
// can mean: a matched rule (shown 1-based, so "rule 1" reads as an ordinal
// no reader can mistake for the wire's 0-based index, and can never be
// misread as "rule 0" meaning none) versus nil, which is the policy's own
// DefaultAction having applied — stated as "no rule matched" rather than
// silently omitted, since that fact is itself worth knowing. Not "policy
// default": a policy is often literally named "default", which rendered as
// "policy default · … · policy default".
func policyText(m dialect.Meta, ascii bool) string {
	name, path := sanitize.Line(m.PolicyName), sanitize.Line(m.PolicyPath)
	detail := sanitize.Line(m.Detail)
	parts := make([]string, 0, 3)
	if name != "" {
		parts = append(parts, "policy "+name)
	}
	if path != "" {
		parts = append(parts, "path "+path)
	}
	if name != "" {
		switch {
		case detail != "":
			parts = append(parts, detail)
		case m.MatchedRule != nil:
			parts = append(parts, fmt.Sprintf("rule %d", *m.MatchedRule+1))
		default:
			parts = append(parts, "no rule matched")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, sep(ascii))
}

// diffStyle classifies a diff body line by its own first character, so
// meaning survives NO_COLOR.
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
