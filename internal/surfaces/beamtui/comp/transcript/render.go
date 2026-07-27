package transcript

import (
	"fmt"
	"strings"

	"github.com/contenox/beam/internal/surfaces/beamtui/enginebridge"
	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/sanitize"
	"github.com/contenox/beam/internal/surfaces/beamtui/textwidth"
	libacp "github.com/contenox/beam/libacp"
)

// quoteMarker is a blockquote's literal source prefix. It is not a glyph and
// has no ASCII variant: it is what the agent wrote, kept so a copied quote
// round-trips as markdown.
const quoteMarker = "> "

// The ASCII glyphs this component draws, exported so testkit's parity test
// can pin them against the style package's GlyphSet. Components may not
// import style (blueprint rule c) and style may not import components, so the
// two ASCII vocabularies can only be held together from outside — by a test
// that sees both. Three characters have to agree across every surface a Mono
// terminal shows, because a "+" that means "done" here and something else in
// the status bar is a legibility bug that color would normally have covered.
const (
	ASCIIDone   = "+" // completed — style.GlyphSet.Check
	ASCIIFailed = "x" // failed — style.GlyphSet.Cross
	ASCIIUser   = ">" // the user-turn sigil — style.GlyphSet.Collapsed
)

// unit is one settled piece of transcript content, held UNRENDERED until a
// TakeAppends supplies the width. Every glyph choice happens in render, so the
// unicode/ASCII decision is the caller's at take time, not the event's at
// arrival time.
//
// render is the SETTLED shape, and settled prose is emitted unwrapped: one
// source line, one output line, at any width (see [Transcript.TakeAppends]).
type unit interface {
	render(width int, g glyphs) []frame.Line
}

// liveRenderer is implemented by the units whose LIVE shape differs from their
// settled one — the two prose units, which wrap in the live region and nowhere
// else. Everything else renders identically on both paths: a card is one line
// by construction, and code and shell output are unwrapped by the same
// copy-fidelity rule that now covers prose.
type liveRenderer interface {
	renderLive(width int, g glyphs) []frame.Line
}

// renderLive renders u for the bounded live region.
func renderLive(u unit, width int, g glyphs) []frame.Line {
	if lr, ok := u.(liveRenderer); ok {
		return lr.renderLive(width, g)
	}
	return u.render(width, g)
}

// glyphs is the component's character set in both variants. It deliberately
// does not import the style package's GlyphSet — components may not depend on
// style — so the ASCII column is spelled out here and pinned by a test.
type glyphs struct {
	quote      string // blockquote / card body gutter
	user       string // user-turn sigil
	mission    string // mission card marker
	done       string // completed tool call
	failed     string // failed tool call
	pending    string // pending (or spinner-less) tool call
	abandoned  string // a call the turn ended underneath
	shell      string // user shell line marker
	dash       string // turn-notice lead-in
	dot        string // shell reconnect marker
	ellipsis   string
	indentUser string // continuation indent under the user sigil
}

var unicodeGlyphs = glyphs{
	quote:      "▎ ",
	user:       "❯ ",
	mission:    "◆",
	done:       "✓",
	failed:     "✗",
	pending:    "·",
	abandoned:  "·",
	shell:      "!",
	dash:       "—",
	dot:        "·",
	ellipsis:   "…",
	indentUser: "  ",
}

// asciiGlyphs is the legacy-console fallback. Nothing here is width-critical,
// because a card truncates its title against whatever the marker actually
// costs — but the status markers are single cells anyway, which is why "done"
// is "+" and not the "ok" it used to be: two cells put the title of every
// completed card one column right of every failed one, and the ragged edge
// read as a layout bug rather than as a status.
var asciiGlyphs = glyphs{
	quote:      "| ",
	user:       ASCIIUser + " ",
	mission:    "*",
	done:       ASCIIDone,
	failed:     ASCIIFailed,
	pending:    "-",
	abandoned:  "-",
	shell:      "!",
	dash:       "-",
	dot:        "-",
	ellipsis:   "...",
	indentUser: "  ",
}

func glyphsFor(ascii bool) glyphs {
	if ascii {
		return asciiGlyphs
	}
	return unicodeGlyphs
}

// blankUnit is the separator between groups.
type blankUnit struct{}

func (blankUnit) render(int, glyphs) []frame.Line { return []frame.Line{frame.Plain("")} }

// proseUnit is one source line of assistant prose, rendered with beam's
// line-based markdown as ONE logical line.
type proseUnit struct{ text string }

func (u proseUnit) render(_ int, g glyphs) []frame.Line {
	return []frame.Line{parseProse(u.text, g).line()}
}

func (u proseUnit) renderLive(width int, g glyphs) []frame.Line {
	return parseProse(u.text, g).wrap(width)
}

// thoughtUnit is one source line of reasoning. Thoughts render as plain
// dimmed text with no markdown decoration: D57 puts them in the MVP as the
// quieter counterpart to answer text, and a dimmed heading that outshouts the
// answer above it would defeat that.
type thoughtUnit struct{ text string }

func (u thoughtUnit) render(int, glyphs) []frame.Line {
	return []frame.Line{styled(frame.StyleThought, u.text)}
}

func (u thoughtUnit) renderLive(width int, g glyphs) []frame.Line {
	return wrapSpans([]frame.Span{frame.S(frame.StyleThought, u.text)}, width)
}

// tableUnit is one row of a markdown table: a single span, emitted UNWRAPPED
// and UNSTYLED at any width.
//
// A table is column-aligned data whose alignment lives entirely in its spacing
// and its pipes. Running it through the inline-markdown pass turned an
// asterisk in a cell into emphasis and ate its delimiters, and wrapping it
// shredded the columns into rows of pipe fragments — which is what the first
// dogfooding hunt saw. It takes the same treatment as a fenced code line, for
// the same reason: the source IS the layout, so it ships verbatim and the
// terminal decides what to do when it does not fit.
type tableUnit struct{ text string }

func (u tableUnit) render(int, glyphs) []frame.Line {
	return []frame.Line{styled(frame.StyleNone, u.text)}
}

// codeUnit is one line inside a fenced code block: a single span, emitted
// UNWRAPPED at any width. Copy fidelity is constitutional — a code line the
// component split would paste back as two lines — so the engine owns the
// overflow and the terminal's own wrap does what it always does.
type codeUnit struct{ text string }

func (u codeUnit) render(int, glyphs) []frame.Line {
	return []frame.Line{styled(frame.StyleCode, u.text)}
}

// fenceUnit is a ``` delimiter line. It is kept rather than swallowed (the
// source round-trips, and the block's boundary stays visible) but rendered
// muted so it reads as punctuation, not as code.
type fenceUnit struct{ text string }

func (u fenceUnit) render(int, glyphs) []frame.Line {
	return []frame.Line{styled(frame.StyleMuted, u.text)}
}

// shellUnit is one line of shell output. Like code it is emitted unwrapped:
// terminal output is already laid out in columns and re-flowing it would
// scramble anything aligned.
type shellUnit struct{ text string }

func (u shellUnit) render(int, glyphs) []frame.Line {
	return []frame.Line{styled(frame.StyleShell, u.text)}
}

// shellEchoUnit is the marker for a line the user ran on the session's shell.
type shellEchoUnit struct{ command string }

func (u shellEchoUnit) render(_ int, g glyphs) []frame.Line {
	return []frame.Line{styled(frame.StyleShell, g.shell+" "+u.command)}
}

// shellResetUnit marks a re-delivered shell snapshot, so the repeated output
// below it reads as a replay instead of as duplication.
type shellResetUnit struct{}

func (shellResetUnit) render(width int, g glyphs) []frame.Line {
	return wrapSpans([]frame.Span{
		frame.S(frame.StyleMuted, g.dot+" shell reconnected "+g.dash+" replaying"),
	}, width)
}

// userUnit is a user turn: one output line per line the operator typed. The
// sigil carries the whole styling and the text stays unstyled — selecting the
// answer to a question should paste the words, not a color decision — and the
// lines after the first are indented to sit under the sigil.
type userUnit struct{ text string }

func (u userUnit) render(_ int, g glyphs) []frame.Line {
	srcs := splitSourceLines(u.text)
	out := make([]frame.Line, 0, len(srcs))
	for i, src := range srcs {
		prefix := frame.S(frame.StyleUser, g.user)
		if i > 0 {
			prefix = frame.S(frame.StyleNone, g.indentUser)
		}
		out = append(out, buildLine(prefix, frame.S(frame.StyleNone, src)))
	}
	return out
}

// missionUnit is a mission report or ask: the header names the unit that
// spoke, because a report rendered as plain assistant prose is a defect —
// reports are how detached work talks back into a session nobody is watching.
type missionUnit struct {
	agent string
	kind  string
	text  string
	ask   bool
}

func (u missionUnit) render(_ int, g glyphs) []frame.Line {
	who := missionSpeaker(u.agent)
	head := frame.StyleStrong
	verb := " reported"
	if u.ask {
		head = frame.StyleHITL
		verb = " is waiting on you"
	}
	header := []frame.Span{frame.S(head, g.mission+" "+who+verb)}
	if u.kind != "" {
		header = append(header, frame.S(frame.StyleMuted, " ("+u.kind+")"))
	}
	out := []frame.Line{buildLine(header...)}

	if u.text == "" {
		return out
	}
	// One report line in, one out. The gutter marks every line of the body
	// because the body is a quotation — it is somebody else's message, carried
	// into a session that was not watching — and a line that lost the gutter
	// would read as this session's own prose.
	gutter := frame.S(frame.StyleBorder, g.quote)
	for _, src := range splitSourceLines(u.text) {
		out = append(out, buildLine(gutter, frame.S(frame.StyleNone, src)))
	}
	return out
}

// missionSpeaker names the unit a mission card is about. A mission that never
// carried an agent name still gets a card — the attribution is what the card
// exists for, and "mission unit" is a truer answer than a blank.
func missionSpeaker(agent string) string {
	if agent == "" {
		return "mission unit"
	}
	return "unit " + agent
}

// missionStatusUnit is a mission reaching a lifecycle state: one line, one
// status word, styled by the state itself.
//
// The STYLE is the whole point of a separate unit. A landed mission and a
// derailed one are the same sentence with one word changed, and an operator
// skimming a long transcript reads the color before the word — so the four
// terminal states take the four roles that already mean those things everywhere
// else in beam (done / failed / warn / muted for a mission nobody finished),
// and anything else — "open", or a status a later service grew — renders strong
// and unremarkable rather than claiming an outcome it does not know.
type missionStatusUnit struct {
	agent  string
	status string
	reason string
}

func (u missionStatusUnit) render(_ int, g glyphs) []frame.Line {
	head := g.mission + " " + missionSpeaker(u.agent)
	if u.status != "" {
		head += " " + u.status
	}
	spans := []frame.Span{frame.S(missionStatusStyle(u.status), head)}
	if u.reason != "" {
		// Muted, and behind a dash: the reason is context for the status, not a
		// second status. Styling it like the state would make "derailed" and
		// "the branch was gone" read as equally load-bearing.
		spans = append(spans, frame.S(frame.StyleMuted, " "+g.dash+" "+u.reason))
	}
	return []frame.Line{buildLine(spans...)}
}

// missionStatusStyle maps the lifecycle vocabulary onto beam's style roles.
// StyleMuted for abandoned is deliberate: an abandoned mission is not a failure,
// it is work nobody carried on with, and dressing it as an error would put a red
// line in the transcript for a decision the operator made.
func missionStatusStyle(status string) frame.StyleID {
	switch status {
	case enginebridge.MissionStatusLanded:
		return frame.StyleDone
	case enginebridge.MissionStatusDerailed:
		return frame.StyleFailed
	case enginebridge.MissionStatusStuck:
		return frame.StyleWarn
	case enginebridge.MissionStatusAbandoned:
		return frame.StyleMuted
	}
	return frame.StyleStrong
}

// missionPlanUnit is a mission's planner replacing its plan: the revision line
// and the shape of the plan underneath it.
//
// It renders ENTIRELY muted, both lines, and that is the design rather than an
// oversight. A unit reorganizing its own work is the thing the operator
// delegated; it belongs in the record so the transcript can answer "what was it
// doing at 3am", and it must not compete for attention with the answer the
// operator is reading. The bell rules agree — a plan revision never rings.
type missionPlanUnit struct {
	agent       string
	revision    int
	explanation string
	pending     int
	inProgress  int
	completed   int
}

func (u missionPlanUnit) render(_ int, g glyphs) []frame.Line {
	head := fmt.Sprintf("%s %s plan rev %d", g.mission, missionSpeaker(u.agent), u.revision)
	if u.explanation != "" {
		head += " " + g.dash + " " + u.explanation
	}
	// The counts hang under the header on their own row, indented like a user
	// turn's continuation: three numbers spliced onto the explanation would be
	// the first thing truncated on a narrow terminal, and they are the half an
	// operator actually skims.
	sep := " " + g.dot + " "
	counts := fmt.Sprintf("%d done%s%d running%s%d pending", u.completed, sep, u.inProgress, sep, u.pending)
	return []frame.Line{
		styled(frame.StyleMuted, head),
		styled(frame.StyleMuted, g.indentUser+counts),
	}
}

// splitSourceLines cuts multi-line unit text into the rows that will become
// spans, expanding each line's tabs against its own column origin. Tab stops
// are per-line, so this cannot be done before the split — which is exactly
// why sanitize.Lines leaves tabs alone and hands the decision here.
func splitSourceLines(text string) []string {
	out := strings.Split(text, "\n")
	for i, l := range out {
		out[i] = sanitize.ExpandTabs(l, sanitize.DefaultTabStop)
	}
	return out
}

// cardUnit is a tool call's settled one-liner: the same shape the live region
// showed while it ran, so the card does not visibly jump when it settles.
type cardUnit struct {
	title     string
	status    libacp.ToolCallStatus
	abandoned bool
}

func (u cardUnit) render(width int, g glyphs) []frame.Line {
	return []frame.Line{cardLine(u.title, u.status, u.abandoned, "", width, g)}
}

// stopUnit annotates a turn that ended for any reason other than a normal
// end_turn — including a genuine cancel, which is a fact and not an error.
type stopUnit struct{ reason string }

func (u stopUnit) render(width int, g glyphs) []frame.Line {
	return wrapSpans([]frame.Span{
		frame.S(frame.StyleMuted, g.dash+" "+strings.ReplaceAll(u.reason, "_", " ")),
	}, width)
}

// failUnit is a turn that never produced a stop reason at all.
type failUnit struct{ err string }

func (u failUnit) render(width int, g glyphs) []frame.Line {
	text := g.failed + " turn failed"
	if u.err != "" {
		text += ": " + u.err
	}
	return wrapSpans([]frame.Span{frame.S(frame.StyleError, text)}, width)
}

// cardLine renders one collapsed tool-call line — the only shape a card has
// in the MVP (blueprint D44: collapsed by default, uniform, no expansion).
//
// spinner is the animation frame for a running call; an empty spinner falls
// back to the static pending glyph so a settled card and a golden test both
// render without motion. The title is TRUNCATED, never wrapped: a card is one
// line by definition, and a two-line card would break the "updated in place"
// contract the moment it settled at a different width.
func cardLine(title string, status libacp.ToolCallStatus, abandoned bool, spinner string, width int, g glyphs) frame.Line {
	mark, style := g.pending, frame.StylePending
	switch {
	case abandoned:
		mark, style = g.abandoned, frame.StyleSkipped
	case status == libacp.ToolCallStatusCompleted:
		mark, style = g.done, frame.StyleDone
	case status == libacp.ToolCallStatusFailed:
		mark, style = g.failed, frame.StyleFailed
	case status == libacp.ToolCallStatusInProgress && spinner != "":
		mark = spinner
	}

	line := frame.Line{frame.S(style, mark)}
	budget := width - textwidth.Width(mark) - 1
	if width <= 0 {
		budget = 0
	}
	if width > 0 && budget < 1 {
		return line
	}
	if width > 0 && textwidth.Width(title) > budget {
		// The ellipsis has to FIT before it can mark anything. ASCII spends
		// three cells on "...", and runewidth.Truncate handed a tail wider
		// than the budget returns the bare tail — a card that overflowed the
		// terminal to say the title was cut.
		tail := g.ellipsis
		if textwidth.Width(tail) > budget {
			tail = ""
		}
		title = textwidth.Truncate(title, budget, tail)
	}
	return append(line, frame.S(frame.StyleNone, " "), frame.S(frame.StyleTool, title))
}

// proseLine is one source line of markdown, decided but not yet laid out: the
// prefix that carries the line's structure, the indent a wrapped continuation
// of it would hang under, and the styled body.
//
// The split exists because the two render paths recombine the same decision
// differently — settled prose is one unwrapped line, the live tail wraps — and
// the markdown vocabulary must not be interpreted twice.
type proseLine struct {
	prefix frame.Span
	cont   frame.Span
	body   []frame.Span
}

// line is the SETTLED shape: prefix and body on one logical line, however wide.
func (p proseLine) line() frame.Line {
	return buildLine(append([]frame.Span{p.prefix}, p.body...)...)
}

// wrap is the LIVE shape: soft-wrapped to width, continuations hanging under
// the prefix.
func (p proseLine) wrap(width int) []frame.Line {
	return wrapWithPrefix(p.body, width, p.prefix, p.cont)
}

// parseProse interprets ONE source line of markdown. The vocabulary is
// deliberately line-based — headings, blockquotes, list markers, and inline
// code/strong/emphasis — because a streaming renderer settles a line the
// instant its newline arrives and can never revisit it. Anything ambiguous
// degrades to the literal source text: garbling a line is worse than not
// styling it.
//
// Every prefix it produces is SOURCE the agent wrote, never a substituted
// glyph, so a copied line pastes back as the markdown it came from.
func parseProse(src string, g glyphs) proseLine {
	if src == "" {
		return proseLine{}
	}
	indent, rest := splitIndent(src)

	if headingLevel(rest) > 0 {
		// The "#" markers stay: they are the source, they round-trip on a
		// copy, and StyleHeading already carries the emphasis.
		return proseLine{body: []frame.Span{frame.S(frame.StyleHeading, src)}}
	}

	if body, ok := quoteBody(rest); ok {
		// The "> " is the SOURCE, and it stays the source. Substituting the
		// prettier "▎ " gutter styled the line at the cost of the one thing
		// this renderer promises: selecting a quoted passage out of the
		// terminal has to paste back as markdown the agent would recognise,
		// and "▎ quoted" is not that. The style carries the decoration; the
		// text carries the meaning, exactly like the "#" markers a heading
		// keeps a dozen lines up.
		gutter := frame.S(frame.StyleBorder, indent+quoteMarker)
		return proseLine{prefix: gutter, cont: gutter, body: []frame.Span{frame.S(frame.StyleMuted, body)}}
	}

	if marker := bulletMarker(rest); marker != "" {
		return proseLine{
			prefix: frame.S(frame.StyleAssistant, indent+marker),
			cont:   frame.S(frame.StyleNone, strings.Repeat(" ", textwidth.Width(indent+marker))),
			body:   parseInline(rest[len(marker):]),
		}
	}

	return proseLine{body: parseInline(src)}
}

// buildLine assembles one output line from spans, dropping the empty ones (a
// prefix nobody set, a body that was all markers) and merging neighbours that
// share a style. An empty result is a genuinely blank line, not a styled
// nothing.
func buildLine(spans ...frame.Span) frame.Line {
	out := make(frame.Line, 0, len(spans))
	for _, s := range spans {
		if s.Text == "" {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return frame.Plain("")
	}
	return merge(out)
}

// splitIndent separates a line's leading spaces from its content, so a nested
// list or quote keeps its indentation in the prefix rather than in the wrapped
// body.
func splitIndent(s string) (indent, rest string) {
	i := 0
	for i < len(s) && s[i] == ' ' {
		i++
	}
	return s[:i], s[i:]
}

// headingLevel reports the ATX heading level of s, or 0. A "#" with no space
// after it is not a heading — "#1 priority" is prose.
func headingLevel(s string) int {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0
	}
	if n == len(s) || s[n] == ' ' {
		return n
	}
	return 0
}

// quoteBody reports the body of a blockquote line.
func quoteBody(s string) (string, bool) {
	if s == ">" {
		return "", true
	}
	if strings.HasPrefix(s, "> ") {
		return s[2:], true
	}
	return "", false
}

// bulletMarker reports the list marker a line opens with, including its
// trailing space, or "". The marker is kept verbatim in the output; only the
// wrap indent is derived from it.
func bulletMarker(s string) string {
	if len(s) >= 2 && (s[0] == '-' || s[0] == '*' || s[0] == '+') && s[1] == ' ' {
		return s[:2]
	}
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	if n > 0 && n+1 < len(s) && (s[n] == '.' || s[n] == ')') && s[n+1] == ' ' {
		return s[:n+2]
	}
	return ""
}

// isFence reports whether a source line opens or closes a fenced code block.
func isFence(src string) bool {
	_, rest := splitIndent(src)
	return strings.HasPrefix(rest, "```")
}

// isTableRow reports whether a source line is a row of a markdown table.
//
// A leading pipe is the whole test for a body row, which is what every table
// generator emits. The second arm catches the one row shape that legitimately
// starts without one — a header separator written as ":--- | ---:" — and only
// that: it demands a pipe (so a "---" horizontal rule stays prose) and a dash
// (so a lone "|" does not qualify), and rejects any other rune.
func isTableRow(src string) bool {
	_, rest := splitIndent(src)
	if strings.HasPrefix(rest, "|") {
		return true
	}
	pipe, dash := false, false
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '|':
			pipe = true
		case '-':
			dash = true
		case ':', ' ':
		default:
			return false
		}
	}
	return pipe && dash
}

// advanceFence carries fenced-code state across the source lines of one
// message.
func advanceFence(src string, in bool) bool {
	if isFence(src) {
		return !in
	}
	return in
}

// parseInline renders one line's inline markdown into spans: `code`,
// **strong**, *emphasis*. Everything else — including "_" (snake_case is
// identifiers, not emphasis, in a coding harness), nested markers, and any
// unterminated delimiter — stays literal text. Spans are NOT nested: scanning
// left to right, the first marker run that closes cleanly wins and its body is
// one flat run.
func parseInline(s string) []frame.Span {
	var out []frame.Span
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			out = append(out, frame.S(frame.StyleAssistant, lit.String()))
			lit.Reset()
		}
	}

	for i := 0; i < len(s); {
		c := s[i]
		if c != '`' && c != '*' {
			lit.WriteByte(c)
			i++
			continue
		}

		// A marker run is matched or rejected as a WHOLE run: rescanning the
		// tail of a rejected "**" as a single "*" is how a renderer turns
		// "**a*b**" into garbage instead of into itself.
		n := runLen(s, i, c)
		if style, end, ok := inlineSpan(s, i, c, n); ok {
			flush()
			out = append(out, frame.S(style, s[i+n:end]))
			i = end + n
			continue
		}
		lit.WriteString(s[i : i+n])
		i += n
	}
	flush()
	if len(out) == 0 {
		return []frame.Span{frame.S(frame.StyleAssistant, s)}
	}
	return out
}

// inlineSpan reports the style and closing-delimiter offset for the marker run
// of n c-bytes at i, or ok=false when the run is not a span this renderer will
// commit to.
func inlineSpan(s string, i int, c byte, n int) (frame.StyleID, int, bool) {
	if c == '*' && n > 2 {
		// Three or more markers in a row is ambiguous (bold+italic, a
		// literal separator, a typo). Degrade to literal.
		return frame.StyleNone, 0, false
	}
	end := indexRun(s, i+n, c, n)
	if end <= i+n { // no closer, or an empty body
		return frame.StyleNone, 0, false
	}
	body := s[i+n : end]
	if c == '`' {
		return frame.StyleCode, end, true
	}
	if !emphasizable(body) {
		return frame.StyleNone, 0, false
	}
	if n == 2 {
		return frame.StyleStrong, end, true
	}
	return frame.StyleEmphasis, end, true
}

// emphasizable rejects the shapes markdown itself rejects — a body that opens
// or closes on whitespace ("2 * 3 * 4" is arithmetic) — and any body carrying
// another marker, which would need nesting this renderer does not do.
func emphasizable(body string) bool {
	if body == "" {
		return false
	}
	if body[0] == ' ' || body[0] == '\t' || body[len(body)-1] == ' ' || body[len(body)-1] == '\t' {
		return false
	}
	return !strings.Contains(body, "*")
}

// runLen counts the consecutive ch bytes starting at i.
func runLen(s string, i int, ch byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == ch {
		n++
	}
	return n
}

// indexRun finds the next run of EXACTLY n ch bytes at or after from, so a
// single backtick never closes on the first tick of a double run.
func indexRun(s string, from int, ch byte, n int) int {
	for i := from; i < len(s); {
		if s[i] != ch {
			i++
			continue
		}
		r := runLen(s, i, ch)
		if r == n {
			return i
		}
		i += r
	}
	return -1
}

// wrapSpans soft-wraps a styled run to width.
//
// Wrapping is NOT the general case in this component — settled prose ships one
// source line per output line and lets the terminal wrap it, because a break
// this component inserts is a real newline in whatever the operator copies.
// The two callers left are the ones where that reasoning does not apply: the
// live region, which the engine truncates rather than wraps (see
// [Transcript.Live]), and beam's own notice chrome, which is not transcript
// content anybody copies.
func wrapSpans(spans []frame.Span, width int) []frame.Line {
	return wrapWithPrefix(spans, width, frame.Span{}, frame.Span{})
}

// wrapWithPrefix soft-wraps a styled run to width, prefixing the first output
// line with first and every continuation line with cont. Both prefixes count
// against the width.
//
// textwidth.Wrap owns the break decisions — one wrapper for the whole TUI, so
// every component reflows identically. Re-hanging the styled spans over its
// breaks is this function's job.
//
// The rows carry the source EXACTLY: Wrap only ever inserts breaks, so the
// chunks concatenate back to the input, and this function emits each chunk
// unchanged. It used to trim the whitespace sitting on a break, on the theory
// that a space at a row's edge is invisible. It is not invisible to a
// selection, and the theory was wrong in a way arithmetic makes obvious: a
// run of spaces longer than the wrap width becomes a chunk that is ENTIRELY
// whitespace, and trimming both of its ends deleted it. A key-value line
// padded to a column ("name" + 45 spaces + "value") lost the whole gap at
// width 40 and pasted back as "namevalue". Copy fidelity is the one thing
// this renderer is not allowed to trade, so nothing is trimmed and the row
// structure the wrapper produced is what ships.
func wrapWithPrefix(spans []frame.Span, width int, first, cont frame.Span) []frame.Line {
	pad := textwidth.Width(first.Text)
	if w := textwidth.Width(cont.Text); w > pad {
		pad = w
	}
	text := spansText(spans)

	// A prefix at least as wide as the terminal leaves nothing to wrap into.
	// Wrapping at one cell still beats not wrapping at all: the row overflows
	// by the prefix either way, but the BODY stays bounded instead of running
	// the whole source line off the edge. Below the supported minimum width
	// this is damage control, not layout.
	avail := width - pad
	if width > 0 && avail < 1 {
		avail = 1
	}
	chunks := textwidth.Wrap(text, avail)

	out := make([]frame.Line, 0, len(chunks))
	c := spanCursor{spans: spans, text: text}
	for i, chunk := range chunks {
		p := first
		if i > 0 {
			p = cont
		}
		// A no-op against today's Wrap, kept as the seam a word-consuming
		// wrapper would need: it advances the cursor over whitespace the
		// wrapper swallowed rather than assuming a length identity.
		c.skipSpacesBefore(chunk)

		line := frame.Line{}
		if p.Text != "" {
			line = append(line, p)
		}
		line = append(line, c.take(len(chunk))...)
		out = append(out, merge(line))
	}
	if len(out) == 0 {
		out = append(out, frame.Plain(""))
	}
	return out
}

// spanCursor walks a styled run in step with its wrapped text.
type spanCursor struct {
	spans []frame.Span
	text  string
	si    int // index of the span holding the cursor
	so    int // byte offset within that span
	pos   int // byte offset within text
}

// take consumes the next n bytes and returns them as styled spans.
func (c *spanCursor) take(n int) []frame.Span {
	var out []frame.Span
	for n > 0 && c.si < len(c.spans) {
		take := len(c.spans[c.si].Text) - c.so
		if take > n {
			take = n
		}
		if take > 0 {
			out = append(out, frame.S(c.spans[c.si].Style, c.spans[c.si].Text[c.so:c.so+take]))
			c.so += take
			c.pos += take
			n -= take
		}
		if c.so >= len(c.spans[c.si].Text) {
			c.si++
			c.so = 0
		}
	}
	return out
}

// skipSpacesBefore discards whitespace the wrapper consumed at a break. Only
// whitespace is ever skipped, and only while the upcoming chunk does not
// already match — a wrapper that drops nothing leaves the cursor untouched.
func (c *spanCursor) skipSpacesBefore(chunk string) {
	for c.pos < len(c.text) && !strings.HasPrefix(c.text[c.pos:], chunk) {
		if b := c.text[c.pos]; b != ' ' && b != '\t' {
			return
		}
		c.take(1)
	}
}

// merge joins adjacent spans that share a style. Two spans under one style are
// indistinguishable once drawn, so this is purely a simplification — it keeps
// a list marker from arriving as its own span and keeps goldens readable.
func merge(l frame.Line) frame.Line {
	out := l[:0]
	for _, s := range l {
		if n := len(out); n > 0 && out[n-1].Style == s.Style {
			out[n-1].Text += s.Text
			continue
		}
		out = append(out, s)
	}
	return out
}

func spansText(spans []frame.Span) string {
	if len(spans) == 1 {
		return spans[0].Text
	}
	var b strings.Builder
	for _, s := range spans {
		b.WriteString(s.Text)
	}
	return b.String()
}

// styled builds a one-span line, keeping a blank line genuinely blank rather
// than an empty span wearing a style.
func styled(style frame.StyleID, text string) frame.Line {
	if text == "" {
		return frame.Plain("")
	}
	return frame.Styled(style, text)
}
