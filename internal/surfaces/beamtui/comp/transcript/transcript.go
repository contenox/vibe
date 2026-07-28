// Package transcript renders one beam session's ordered record — user turns,
// streamed assistant prose, thought traces, tool-call cards, mission reports
// and lifecycle cards, shell output and turn notices — as frame data.
// Settled content is appended once to real scrollback and never repainted
// (never laid out against a width, so it never re-flows on resize); only the
// bounded live region redraws. A line settles when it can no longer change:
// a streamed source line on its newline, a tool card on terminal status, a
// complete-on-arrival event (report, echo, notice) immediately.
package transcript

import (
	"strings"

	"github.com/contenox/contenox/internal/surfaces/beamtui/enginebridge"
	"github.com/contenox/contenox/internal/surfaces/beamtui/frame"
	"github.com/contenox/contenox/internal/surfaces/beamtui/sanitize"
	libacp "github.com/contenox/contenox/libacp"
)

// Transcript is the transcript state machine. The zero value is not usable;
// call [New]. It is not safe for concurrent use — the app loop owns it, in
// the same goroutine that drains the bridge.
type Transcript struct {
	// queue holds settled units awaiting a TakeAppends, stored unrendered:
	// width is not known until the take, which fixes a line's shape forever.
	queue []unit

	// msg is the assistant or thought message currently streaming, nil when
	// no message is open.
	msg *stream
	// shell is the per-session shell pseudo-message: terminal output has no
	// MessageID of its own, so it accumulates under a key of its own.
	shell *stream

	// closed names every identified message key already ended. A delta for
	// one is dropped, since the terminal already printed those lines and
	// re-opening would duplicate a finished turn. Keys with an empty
	// MessageID are never recorded here (see delta).
	closed map[streamKey]bool

	// seenShell marks shell streams that produced at least one byte, to
	// distinguish a reconnect replay from the first (usually empty) snapshot.
	seenShell map[streamKey]bool

	// open lists tool calls not yet terminal, in arrival order. cards keeps
	// every card ever seen, settled included, so a late update for a settled
	// call is ignored rather than reopened.
	open  []*card
	cards map[string]*card

	// prev is the group the last queued unit belonged to; seq mints group
	// identities. Together they place the blank separators (see group).
	prev group
	seq  int64
}

// New returns an empty transcript.
func New() *Transcript {
	return &Transcript{
		cards:     make(map[string]*card),
		closed:    make(map[streamKey]bool),
		seenShell: make(map[streamKey]bool),
	}
}

// Apply folds one bridge event into the transcript state; events that are
// not transcript facts are ignored, so a caller may hand it the whole
// stream. It never renders and never depends on width. Every string is run
// through sanitize at arrival, not at render, so no unit downstream can
// forget to: sanitize.Lines where structure survives, sanitize.Line where it
// becomes a single span.
func (t *Transcript) Apply(ev enginebridge.Event) {
	switch e := ev.(type) {
	case enginebridge.TextDelta:
		t.delta(streamKey{session: e.SessionID, message: e.MessageID, role: roleAssistant}, sanitize.Lines(e.Text))

	case enginebridge.ThoughtDelta:
		t.delta(streamKey{session: e.SessionID, message: e.MessageID, role: roleThought}, sanitize.Lines(e.Text))

	case enginebridge.UserEcho:
		// A user turn is a hard boundary: whatever was streaming belongs to
		// the previous turn.
		t.closeMessage()
		if text := sanitize.Lines(e.Text); text != "" {
			t.startGroup(group{kind: groupUser, n: t.next()})
			t.push(userUnit{text: text})
		}

	case enginebridge.ToolCallOpened:
		t.tool(e.ToolCallID, sanitize.Line(e.Title), e.Kind, e.Status)

	case enginebridge.ToolCallUpdated:
		t.tool(e.ToolCallID, sanitize.Line(e.Title), e.Kind, e.Status)

	case enginebridge.MissionReport:
		// Does not close the streaming message: a report can race the live
		// stream and settles as its own card beside it, never spliced in.
		t.startGroup(group{kind: groupMission, n: t.next()})
		t.push(missionUnit{
			agent: sanitize.Line(e.AgentName),
			kind:  sanitize.Line(e.Kind),
			text:  sanitize.Lines(e.Text),
		})

	case enginebridge.MissionAsk:
		text := e.Text
		if text == "" {
			text = e.Summary
		}
		t.startGroup(group{kind: groupMission, n: t.next()})
		t.push(missionUnit{
			agent: sanitize.Line(e.AgentName),
			text:  sanitize.Lines(text),
			ask:   true,
		})

	case enginebridge.MissionStatusChanged:
		// Same treatment as a report, for the same reason.
		t.startGroup(group{kind: groupMission, n: t.next()})
		t.push(missionStatusUnit{
			agent:  sanitize.Line(e.AgentName),
			status: sanitize.Line(e.New),
			reason: sanitize.Line(e.Reason),
		})

	case enginebridge.MissionPlanRevised:
		t.startGroup(group{kind: groupMission, n: t.next()})
		t.push(missionPlanUnit{
			agent:       sanitize.Line(e.AgentName),
			revision:    e.Revision,
			explanation: sanitize.Line(e.Explanation),
			pending:     e.Pending,
			inProgress:  e.InProgress,
			completed:   e.Completed,
		})

	case enginebridge.ShellRunStarted:
		if cmd := sanitize.Line(e.Command); cmd != "" {
			t.startGroup(group{kind: groupShell, n: t.next()})
			t.push(shellEchoUnit{command: cmd})
		}

	case enginebridge.TerminalChunk:
		t.terminal(e)

	case enginebridge.TurnEnded:
		t.endTurn()
		if r := sanitize.Line(string(e.StopReason)); r != "" && e.StopReason != libacp.StopReasonEndTurn {
			t.startGroup(group{kind: groupNotice, n: t.next()})
			t.push(stopUnit{reason: r})
		}

	case enginebridge.TurnFailed:
		t.endTurn()
		msg := ""
		if e.Err != nil {
			msg = sanitize.Line(e.Err.Error())
		}
		t.startGroup(group{kind: groupNotice, n: t.next()})
		t.push(failUnit{err: msg})
	}
}

// TakeAppends renders and hands over every line that settled since the last
// call, in arrival order, draining the queue; width and ascii are final for
// those lines. Returns nil when nothing settled. width never wraps prose —
// one source line stays one output line, letting the terminal soft-wrap it —
// but still governs what truncates (a tool card) and notice chrome.
func (t *Transcript) TakeAppends(width int, ascii bool) []frame.Line {
	if len(t.queue) == 0 {
		return nil
	}
	g := glyphsFor(ascii)
	out := make([]frame.Line, 0, len(t.queue))
	for _, u := range t.queue {
		out = append(out, u.render(width, g)...)
	}
	t.queue = nil
	return out
}

// Live renders the in-progress tail: the streaming message's unflushed
// line, the shell's unflushed line, and one collapsed line per open tool
// call. spinnerGlyph marks calls actually running; empty falls back to the
// static pending glyph. Rebuilt every call, so it re-wraps on resize for
// free; row-addressed rows are truncated rather than soft-wrapped, the
// opposite of TakeAppends. Returns nil when nothing is in progress.
func (t *Transcript) Live(width int, ascii bool, spinnerGlyph string) []frame.Line {
	g := glyphsFor(ascii)
	var out []frame.Line
	if t.msg != nil && t.msg.buf != "" {
		out = append(out, renderLive(t.msg.lineUnit(t.msg.buf), width, g)...)
	}
	if t.shell != nil && t.shell.buf != "" {
		out = append(out, renderLive(t.shell.lineUnit(t.shell.buf), width, g)...)
	}
	for _, c := range t.open {
		out = append(out, cardLine(c.displayTitle(), c.status, false, spinnerGlyph, width, g))
	}
	return out
}

// HasOpenWork reports whether the transcript is waiting on the agent: a
// message is streaming, or a tool call has not reached a terminal status. A
// shell tail does not count, or a lingering shell prompt would pin an
// indicator on forever.
func (t *Transcript) HasOpenWork() bool {
	return t.msg != nil || len(t.open) > 0
}

// EndReplay settles everything a history replay left open. A session/load
// replay ends with no TurnEnded, so the last replayed message and any
// mid-flight tool call would otherwise dangle in the live region forever.
// Must be called after the replayed events are applied; a live turn
// afterwards opens a new message as usual.
func (t *Transcript) EndReplay() { t.endTurn() }

// role distinguishes the three things that stream a source line at a time.
type role uint8

const (
	roleAssistant role = iota
	roleThought
	roleShell
)

// streamKey identifies a message. A change in any component, including the
// session, closes the current message and starts a new one.
type streamKey struct {
	session libacp.SessionID
	message string
	role    role
}

// stream accumulates source text for one message until newlines settle it.
type stream struct {
	key streamKey
	// buf is the unterminated tail Live renders: sent but not yet ended.
	buf string
	// fence carries markdown fenced-code state across lines of the same
	// message, per-message so an unclosed fence cannot leak into the next.
	fence bool
	// san strips terminal control sequences from shell output and carries
	// partial escapes across chunk boundaries.
	san sanitizer
	// grp is the stream's group identity, for separator placement.
	grp group
}

// lineUnit classifies one settled (or in-progress, for Live) source line
// into the unit that renders it, and is the choke point where the line
// becomes span-safe: tabs expand to 8-column stops (column alignment, not a
// single space), and a sanitize pass catches bidi controls that can arrive
// split across chunks in shell output.
func (s *stream) lineUnit(src string) unit {
	src = sanitize.ExpandTabs(sanitize.Lines(src), sanitize.DefaultTabStop)
	switch s.key.role {
	case roleShell:
		return shellUnit{text: src}
	case roleThought:
		return thoughtUnit{text: src}
	}
	switch {
	case isFence(src):
		return fenceUnit{text: src}
	case s.fence:
		return codeUnit{text: src}
	case isTableRow(src):
		// After the fence arms: inside a code block a pipe is code.
		return tableUnit{text: src}
	}
	return proseUnit{text: src}
}

// card is one tool call's collapsed state, updated in place by ToolCallID.
// Updates are patch-shaped — fields the agent did not restate arrive zero —
// so merging keeps what is already known rather than overwriting it.
type card struct {
	id      string
	title   string
	kind    libacp.ToolKind
	status  libacp.ToolCallStatus
	settled bool
	grp     group
}

func (c *card) displayTitle() string {
	switch {
	case c.title != "":
		return c.title
	case c.kind != "":
		return string(c.kind)
	}
	return "tool call"
}

// delta folds one streamed chunk into its message, flushing every complete
// source line; a chunk ending mid-word settles nothing. A delta for an
// already-closed identified message is dropped; an empty delta with no
// matching stream open is a no-op.
//
// MessageID is in practice absent on assistant chunks, so a whole session's
// replies share one id-less key. A delta after that key closed reopens a new
// message rather than staying dropped: with no id to tell a new turn's first
// chunk from a cancelled turn's stale tail, reopening risks a duplicated
// line rather than a silently lost reply.
func (t *Transcript) delta(k streamKey, text string) {
	if k.message != "" && t.closed[k] {
		return
	}
	if text == "" && (t.msg == nil || t.msg.key != k) {
		return
	}
	if t.msg != nil && t.msg.key != k {
		t.closeMessage()
	}
	if t.msg == nil {
		t.msg = &stream{key: k, grp: group{kind: groupMessage, n: t.next()}}
	}
	t.msg.buf += text
	t.flushLines(t.msg)
}

// flushLines settles every complete source line sitting in the buffer.
func (t *Transcript) flushLines(s *stream) {
	for {
		i := strings.IndexByte(s.buf, '\n')
		if i < 0 {
			return
		}
		line := strings.TrimSuffix(s.buf[:i], "\r")
		s.buf = s.buf[i+1:]
		t.settleLine(s, line)
	}
}

// settleLine queues one source line and advances the message's markdown state.
func (t *Transcript) settleLine(s *stream, src string) {
	t.startGroup(s.grp)
	t.push(s.lineUnit(src))
	if s.key.role == roleAssistant {
		s.fence = advanceFence(src, s.fence)
	}
}

// closeMessage ends the streaming message, settling its final unterminated
// line. An identified key is remembered as closed so the close sticks
// against deltas still in flight; an id-less key is not, since it is shared
// by every message the agent will ever send (see delta).
func (t *Transcript) closeMessage() {
	if t.msg == nil {
		return
	}
	if t.msg.buf != "" {
		t.settleLine(t.msg, t.msg.buf)
		t.msg.buf = ""
	}
	if t.msg.key.message != "" {
		t.closed[t.msg.key] = true
	}
	t.msg = nil
}

// tool opens or merges a tool call, settling its card once the status is
// terminal.
func (t *Transcript) tool(id, title string, kind libacp.ToolKind, status libacp.ToolCallStatus) {
	if id == "" {
		return
	}
	c := t.cards[id]
	if c == nil {
		// An update for a call whose open we never saw still gets a card:
		// history replay and reconnects can start mid-lifecycle.
		c = &card{id: id, status: libacp.ToolCallStatusPending, grp: group{kind: groupTool, n: t.next()}}
		t.cards[id] = c
		t.open = append(t.open, c)
	}
	if c.settled {
		return
	}
	if title != "" {
		c.title = title
	}
	if kind != "" {
		c.kind = kind
	}
	if status != "" {
		c.status = status
	}
	if c.status == libacp.ToolCallStatusCompleted || c.status == libacp.ToolCallStatusFailed {
		t.settleCard(c, false)
	}
}

// settleCard flushes a card's final one-liner and closes it. abandoned marks
// a call the turn ended underneath, which will never report a status and
// would otherwise dangle in the live region forever.
func (t *Transcript) settleCard(c *card, abandoned bool) {
	c.settled = true
	for i, o := range t.open {
		if o == c {
			t.open = append(t.open[:i], t.open[i+1:]...)
			break
		}
	}
	t.startGroup(c.grp)
	t.push(cardUnit{title: c.displayTitle(), status: c.status, abandoned: abandoned})
}

// endTurn closes everything the finished turn owned.
func (t *Transcript) endTurn() {
	t.closeMessage()
	for len(t.open) > 0 {
		t.settleCard(t.open[0], true)
	}
}

// terminal folds one chunk of shell output into the session's shell
// pseudo-message. Reset means the snapshot was re-delivered, so the buffer
// is replaced, not appended to, and the replay is marked so the repetition
// reads as a reconnect rather than duplicated output.
func (t *Transcript) terminal(e enginebridge.TerminalChunk) {
	k := streamKey{session: e.SessionID, role: roleShell}
	if t.shell != nil && t.shell.key != k {
		t.closeShell()
	}
	if e.Reset {
		// Only an actual re-delivery reads as a reconnect; the first
		// subscribe also arrives as a Reset but with nothing yet produced.
		replay := t.seenShell[k]
		t.closeShell()
		if replay {
			t.startGroup(group{kind: groupNotice, n: t.next()})
			t.push(shellResetUnit{})
		}
	}
	if t.shell == nil {
		t.shell = &stream{key: k, grp: group{kind: groupShell, n: t.next()}}
	}
	if e.Chunk == "" {
		return
	}
	t.seenShell[k] = true
	// applyCR runs over the whole buffer, not just the chunk: the start of
	// the line a CR overwrites may have arrived in an earlier chunk.
	t.shell.buf = applyCR(t.shell.buf + t.shell.san.write(e.Chunk))
	t.flushLines(t.shell)
}

// closeShell ends the shell pseudo-message, settling its unterminated tail
// first — dropping it on a reconnect would delete output the operator
// already watched arrive.
func (t *Transcript) closeShell() {
	if t.shell == nil {
		return
	}
	if t.shell.buf != "" {
		t.settleLine(t.shell, t.shell.buf)
		t.shell.buf = ""
	}
	t.shell = nil
}

// applyCR resolves carriage-return semantics in a shell buffer: a CR returns
// the cursor to the start of its line, and everything written before it on
// that line is overwritten (last write wins), so "10%\r20%\r30%\n" settles
// as "30%". CRLF never reaches here as a CR — the sanitizer folds it to a
// bare newline first.
func applyCR(s string) string {
	if strings.IndexByte(s, '\r') < 0 {
		return s
	}
	out := make([]byte, 0, len(s))
	lineStart := 0 // where the current line begins in out
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\r':
			out = out[:lineStart]
		case '\n':
			out = append(out, c)
			lineStart = len(out)
		default:
			out = append(out, c)
		}
	}
	return string(out)
}

// groupKind classifies a run of settled lines for separator placement.
type groupKind uint8

const (
	groupNone groupKind = iota
	groupMessage
	groupTool
	groupShell
	groupMission
	groupUser
	groupNotice
)

// group identifies one run of lines that belong together. n makes two
// same-kind groups distinguishable, so two consecutive assistant messages are
// separated even though both are groupMessage.
type group struct {
	kind groupKind
	n    int64
}

func (t *Transcript) next() int64 {
	t.seq++
	return t.seq
}

// startGroup emits the blank separator between groups: a group change
// inserts one blank line ahead of the new group, except packing kinds (tool
// cards, shell lines run as one block) and two groups both still producing
// — interleaved flushes, like a live stream beside shell output, would
// otherwise get a separator per switch.
func (t *Transcript) startGroup(g group) {
	if t.prev.kind != groupNone && g != t.prev &&
		!(g.kind == t.prev.kind && packs(g.kind)) &&
		!(t.groupOpen(t.prev) && t.groupOpen(g)) {
		t.queue = append(t.queue, blankUnit{})
	}
	t.prev = g
}

// groupOpen reports whether g still has something producing into it: a live
// stream or a tool call not yet terminal. Groups with no producer (a user
// echo, mission report, turn notice) settle whole on arrival and are never
// open.
func (t *Transcript) groupOpen(g group) bool {
	if t.msg != nil && t.msg.grp == g {
		return true
	}
	if t.shell != nil && t.shell.grp == g {
		return true
	}
	for _, c := range t.open {
		if c.grp == g {
			return true
		}
	}
	return false
}

// packs reports whether consecutive groups of this kind run together without
// a separator.
func packs(k groupKind) bool { return k == groupTool || k == groupShell }

func (t *Transcript) push(u unit) { t.queue = append(t.queue, u) }
