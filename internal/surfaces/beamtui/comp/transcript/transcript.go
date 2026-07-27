// Package transcript renders one beam session's ordered record — user turns,
// streamed assistant prose, thought traces, tool-call cards, mission reports
// and mission lifecycle cards, shell output and turn notices — as frame data.
//
// # The inline model
//
// beam renders inline (blueprint D1, ruled by the constitutional copy/paste
// decision): settled content is APPENDED to the terminal's real scrollback
// exactly once and never repainted, and only a bounded live region redraws.
// That makes this component a state machine with two render outputs rather
// than the usual pure (state, width) → []Line:
//
//   - [Transcript.TakeAppends] hands over the lines that became settled since
//     the last call. They are styled AT TAKE TIME, and once taken they are
//     gone — the caller owns them, because the terminal already owns the rows
//     they were printed on. A later resize can never re-flow them, which is
//     exactly the resize-immunity the inline model buys (blueprint requirement
//     13: already-settled lines never visibly re-flow).
//   - [Transcript.Live] re-renders the in-progress tail from scratch on every
//     call, so a resize re-wraps it for free.
//
// What a settled line is NOT is laid out against a width. One source line
// becomes one logical line, and the terminal soft-wraps it — because a
// soft-wrapped row is still one line to a selection, while a break this
// component inserted is a real newline in whatever the operator copies. That
// is the same constitutional rule the code-fence path has always followed, now
// applied to prose, tables and user turns alike. Width survives for the two
// things that are not copyable content: a tool card, which is one line by
// definition and truncates, and beam's own turn notices.
//
// A line is settled when it can no longer change: a source line of streamed
// prose settles when its newline arrives, a tool card settles when its status
// goes terminal, and a complete-on-arrival event (mission report, user echo,
// turn notice) settles immediately.
//
// # Scope
//
// One Transcript renders one session (blueprint D13: a single visible
// session). SessionID nevertheless participates in the stream key, so a stray
// event from another session starts a new message instead of splicing into the
// live one — the same "never splice" rule that keeps a racing mission report
// out of the stream it arrives beside.
//
// The component is a pure renderer: it calls no service, sends nothing, and
// reads no terminal. Event types that are not transcript facts (permission
// requests, command menus, config, usage, session info, plans) belong to other
// components and are ignored here.
package transcript

import (
	"strings"

	"github.com/contenox/beam/internal/surfaces/beamtui/enginebridge"
	"github.com/contenox/beam/internal/surfaces/beamtui/frame"
	"github.com/contenox/beam/internal/surfaces/beamtui/sanitize"
	libacp "github.com/contenox/beam/libacp"
)

// Transcript is the transcript state machine. The zero value is not usable;
// call [New]. It is not safe for concurrent use — the app loop owns it, in the
// same goroutine that drains the bridge.
type Transcript struct {
	// queue holds settled units awaiting a TakeAppends. Units are stored
	// UNRENDERED: the width is not known until the take, and the take is
	// what fixes a line's shape forever.
	queue []unit

	// msg is the assistant or thought message currently streaming, nil when
	// no message is open.
	msg *stream
	// shell is the per-session shell pseudo-message: terminal output has no
	// MessageID of its own, so it accumulates and flushes exactly like a
	// streamed message under a key of its own.
	shell *stream

	// closed names every IDENTIFIED message key that has already been ended.
	// A delta arriving for one is DROPPED — the mirror of the cards' settled
	// guard, and for the same reason: the terminal has already printed those
	// lines, so re-opening the message would append a second copy of a turn
	// the operator watched finish. Late deltas are routine after a cancel,
	// where the agent's in-flight chunks land behind the TurnEnded that
	// stopped it.
	//
	// Keys with an EMPTY MessageID are deliberately never recorded here (see
	// delta).
	closed map[streamKey]bool

	// seenShell marks shell streams that have produced at least one byte,
	// which is what distinguishes a reconnect replay from the first
	// (usually empty) subscribe snapshot.
	seenShell map[streamKey]bool

	// open lists the tool calls that have not reached a terminal status, in
	// arrival order — the collapsed cards the live region shows. cards keeps
	// every card ever seen, settled ones included, so a late update for an
	// already-settled call is ignored rather than re-opening it.
	open  []*card
	cards map[string]*card

	// prev is the group the last queued unit belonged to, and seq mints
	// group identities. Together they place the blank separator lines that
	// let the transcript breathe (see group).
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

// Apply folds one bridge event into the transcript state. Events that are not
// transcript facts are ignored, so a caller may hand it the whole stream.
//
// Apply never renders: it only decides what has settled. Nothing here depends
// on a width, which is what lets the same event script be replayed at 60, 80
// and 120 columns in a test and produce identical settlement.
//
// # Sanitizing
//
// Every string that arrives here is UNTRUSTED — agent prose, a tool title
// naming a file somebody else's repository chose, a mission report, an error
// string — and every one of them ends up in a frame.Span, which the engine
// draws as literal cells. So each is run through the sanitize package HERE,
// at arrival, not at render: a value that entered the state machine clean
// cannot be forgotten by one of the eight units that render it. Text the
// caller will split on newlines uses sanitize.Lines (structure survives);
// everything that becomes a single span uses sanitize.Line.
func (t *Transcript) Apply(ev enginebridge.Event) {
	switch e := ev.(type) {
	case enginebridge.TextDelta:
		t.delta(streamKey{session: e.SessionID, message: e.MessageID, role: roleAssistant}, sanitize.Lines(e.Text))

	case enginebridge.ThoughtDelta:
		t.delta(streamKey{session: e.SessionID, message: e.MessageID, role: roleThought}, sanitize.Lines(e.Text))

	case enginebridge.UserEcho:
		// A user turn is a hard boundary: whatever was streaming belongs to
		// the turn before it.
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
		// Deliberately does NOT close the streaming message: a report is
		// delivered into the session that fired the mission and can race the
		// live stream. It settles as its own card beside the stream, never
		// spliced into it (blueprint requirement 2).
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
		// Same treatment as a report, for the same reason: a mission coming to
		// rest is detached work talking back into a session that may be busy
		// with something else, so it settles as its own card beside whatever is
		// streaming and is never spliced into it.
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
// call, in arrival order, and drains the queue. The caller appends the result
// to [frame.Frame].Scrollback; the terminal prints it once.
//
// width and ascii are applied here and are final for those lines: this is the
// moment a settled line's shape is fixed. Returns nil when nothing settled.
//
// Note what width does NOT do: it does not wrap prose. One source line becomes
// exactly one output line, however long — the engine prints scrollback RAW, so
// the terminal soft-wraps it and a soft-wrapped row stays ONE logical line
// that native selection rejoins. Wrapping here would put a real newline in the
// middle of every paragraph the operator copies out, which the code-fence path
// has always refused to do and which beam --help promises it does not do.
// Width still governs what truncates (a tool card is one line by definition)
// and what beam's own notice chrome wraps to.
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

// Live renders the in-progress tail: the streaming message's unflushed source
// line, the shell's unflushed output line, and one collapsed card line per
// open tool call. spinnerGlyph marks the calls that are actually running; an
// empty spinnerGlyph falls back to the static pending glyph, which is what
// keeps a golden test free of animation.
//
// The result is rebuilt on every call and belongs to the bounded live region,
// so it repaints — and therefore re-wraps on resize — for free. Returns nil
// when nothing is in progress.
//
// The live region is the one place beam wraps prose ITSELF (renderLive). It is
// row-addressed and repainted, so the engine TRUNCATES a row too wide for the
// terminal instead of letting the terminal soft-wrap it; an unwrapped live
// tail would simply go invisible past the right edge until its newline
// arrived. Settled lines take the opposite path for the opposite reason — see
// [Transcript.TakeAppends].
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
// message is still streaming, or a tool call has not reached a terminal
// status. A shell tail does not count — the user's own shell is not the
// agent's work, and a shell prompt fragment would otherwise pin an indicator
// on forever.
func (t *Transcript) HasOpenWork() bool {
	return t.msg != nil || len(t.open) > 0
}

// EndReplay settles everything a history replay left open.
//
// A session/load replays its transcript as ordinary chunk events and then just
// stops: there is no TurnEnded at the end of history, and no event of any kind
// that says "that was all of it". The last replayed message therefore had
// nothing to settle it and stayed in the live region for the rest of the
// session — with every notice the app queued afterwards printing ABOVE it,
// because scrollback is by definition above the live region. A replayed tool
// call left mid-flight dangled the same way, with no TurnEnded coming to
// abandon it (blueprint requirement 12).
//
// Within a replay the boundaries take care of themselves: a UserEcho is a hard
// turn boundary and closes the assistant message before it. This is the end of
// the LAST turn, the one boundary the event stream does not carry, so the
// caller that knows the replay is over supplies it.
//
// It is a settle, not a close: it must be called AFTER the replayed events
// have been applied (it acts on state, it does not arm a barrier), and a live
// turn arriving afterwards opens a new message as usual.
func (t *Transcript) EndReplay() { t.endTurn() }

// role distinguishes the three things that stream a source line at a time.
type role uint8

const (
	roleAssistant role = iota
	roleThought
	roleShell
)

// streamKey identifies a message. A change in ANY component — including the
// session — closes the current message and starts a new one; the blueprint's
// "never splice" rule is enforced here and nowhere else.
type streamKey struct {
	session libacp.SessionID
	message string
	role    role
}

// stream accumulates SOURCE text for one message until newlines settle it.
type stream struct {
	key streamKey
	// buf is the unterminated tail: the part of the current source line the
	// agent has sent but not yet ended. It is what Live renders.
	buf string
	// fence carries markdown fenced-code state across source lines of the
	// same message. It is per-message on purpose: an unclosed fence must not
	// leak into the next message's rendering.
	fence bool
	// san strips terminal control sequences from shell output and carries
	// partial escapes across chunk boundaries.
	san sanitizer
	// grp is the stream's group identity, for separator placement.
	grp group
}

// lineUnit classifies one settled (or, for Live, one in-progress) source line
// of this stream into the unit that renders it.
//
// It is also the choke point where a source line becomes SPAN-SAFE, which is
// why the sanitizing happens here and not in each of the five units below.
// Tabs expand to the 8-column stops a terminal would have used: a tab in `go
// test` output or a code block is column alignment, and both collapsing it to
// one space and leaving it in a span (where it measures as one cell and draws
// as up to eight) are wrong. The sanitize pass on top is redundant for
// assistant text, which arrived clean, and is not redundant for shell output:
// the byte sanitizer above knows escape sequences but not the bidi controls,
// and a bidi rune can arrive split across two chunks where only a complete
// line can see it whole.
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

// card is one tool call's collapsed state. Updates are patch-shaped — fields
// the agent did not restate arrive zero — so merging keeps what it already
// knows (blueprint 4.9 requirement 5: updated in place by ToolCallID, never
// re-appended per transition).
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

// delta folds one streamed chunk into its message, flushing every source line
// the chunk completed. A chunk that ends mid-word settles nothing.
//
// Two arrivals produce nothing at all. A delta for an IDENTIFIED message the
// turn already ENDED is dropped: those lines are printed, the terminal owns
// them, and re-opening the message would append a duplicate below the notice
// that said the turn was over. And an EMPTY delta with no matching stream open
// is a no-op rather than an empty message — an agent that keeps a stream alive
// with zero-length chunks (or a bridge that synthesises one) would otherwise
// pin HasOpenWork true and spin an activity indicator over nothing.
//
// # Why the closed-set only binds identified messages
//
// MessageID is OPTIONAL on the ACP wire and in practice absent: nothing in
// libacp stamps one on an agent_message_chunk, so every assistant message a
// real agent streams shares the key {session, "", assistant}. Applying the
// closed-set to that key made the first TurnEnded of a process close assistant
// prose FOREVER — the second turn's reply, and every reply after it, was
// dropped here while the tool cards, the gauge and the spinner all kept
// moving. That was the first dogfooding hunt's blocker.
//
// So for an id-less key a delta arriving after the stream was closed OPENS A
// NEW MESSAGE. There is no signal that would let it do better: the two things
// such a delta can be — the first chunk of the next turn, or the stale tail of
// the turn that was just cancelled — are byte-identical on the wire. They
// carry no id, no turn number and no ordinal, and this component has no clock
// to age them with (Apply is a pure fold, which is what lets a script replay
// identically at three widths). Guessing is therefore the whole design space,
// and the two guesses are not symmetric: dropping a real reply loses the
// agent's answer with no trace, while duplicating a straggler tail repeats a
// line or two the operator already read, below a notice that says the turn
// ended. One is a blocker, the other is cosmetic — so reopening always wins.
//
// The mission-report race the closed-set was built for is untouched: those
// messages always carry ids (mission-report-<reportID>), and so does any agent
// that groups its chunks properly.
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
// line. The end of a message is the only thing that can settle a line the
// agent never terminated.
//
// An IDENTIFIED key is remembered as closed, which is what makes the close
// STICK against the deltas still in flight behind it. An id-less key is not:
// it is shared by every message the agent will ever send, so remembering it
// would close the stream for the rest of the process (see delta).
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
		// history replay and reconnects are allowed to start mid-lifecycle.
		c = &card{id: id, status: libacp.ToolCallStatusPending, grp: group{kind: groupTool, n: t.next()}}
		t.cards[id] = c
		t.open = append(t.open, c)
	}
	if c.settled {
		// Terminal is terminal: a trailing update (output, locations) must
		// not re-append a card the terminal already printed.
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

// settleCard flushes a card's final one-liner and closes it. abandoned marks a
// call the turn ended underneath — it will never report a status, and leaving
// it in the live region would dangle forever (blueprint requirement 12).
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
// pseudo-message. Reset means the snapshot was re-delivered, so the buffer is
// REPLACED, not appended to (the bridge's contract), and the replay is marked
// so the repetition reads as a reconnect rather than as duplicated output.
func (t *Transcript) terminal(e enginebridge.TerminalChunk) {
	k := streamKey{session: e.SessionID, role: roleShell}
	if t.shell != nil && t.shell.key != k {
		t.closeShell()
	}
	if e.Reset {
		// Only an actual RE-delivery reads as a reconnect. The very first
		// subscribe also arrives as a Reset (the bridge replays the current
		// scrollback snapshot, usually empty), and announcing "replaying"
		// before the shell ever produced a byte confused the fresh-session
		// welcome (found in live e2e).
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
	// applyCR runs over the WHOLE buffer, not the chunk: a carriage return
	// overwrites the line it lands on, and the start of that line may have
	// arrived in an earlier chunk.
	t.shell.buf = applyCR(t.shell.buf + t.shell.san.write(e.Chunk))
	t.flushLines(t.shell)
}

// closeShell ends the shell pseudo-message, SETTLING its unterminated tail
// first.
//
// That flush is the whole point. A prompt fragment or a progress line the
// shell never newline-terminated is output the operator watched arrive;
// dropping it on a reconnect (or on a session change) would delete visible
// history to make room for a replay of the same output, which reads as the
// terminal losing text. It settles like any other unterminated line — the
// same rule that closes an assistant message's final line.
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
// the cursor to the start of the line it is on, so everything written to that
// line before it is OVERWRITTEN — last write wins. "10%\r20%\r30%\n" is a
// progress counter that settles as one line reading "30%", which is what the
// operator saw; keeping all three (or dropping the CR and running them
// together as "10%20%30%") is a transcript of a line that never existed.
//
// CRLF never reaches here as a CR: the sanitizer folds it to a bare newline
// across chunk boundaries, so a Windows-flavoured line ending is a line
// ending and not an overwrite of the line it ends.
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

// startGroup emits the blank separator that keeps the transcript breathing: a
// group change inserts one blank line ahead of the new group. Tool cards and
// shell lines pack — a run of five tool calls is one visual block, not five
// stanzas — and the transcript never opens or closes on a blank, because the
// live region follows the last settled line immediately.
//
// The exception is two groups that are BOTH still open. An assistant message
// streaming while a shell command prints is not two stanzas, it is two things
// happening at once, and they interleave their flushes: a separator per
// switch turned a fifty-line build log into a hundred lines of alternating
// text and blanks. Neither group has ended, so neither has earned the row.
// Leaving a group that is still running for another one that is still running
// just continues — and coming back later continues again.
//
// The condition is deliberately on BOTH sides rather than on the previous
// group alone. A mission report landing beside a live stream is exactly the
// case blueprint requirement 2 exists for: it settles as its own card, never
// spliced into the stream it raced, and the blank line is what makes that
// visible. A report has no producer of its own — it arrives complete — so it
// is never "still open", and it keeps its separator.
func (t *Transcript) startGroup(g group) {
	if t.prev.kind != groupNone && g != t.prev &&
		!(g.kind == t.prev.kind && packs(g.kind)) &&
		!(t.groupOpen(t.prev) && t.groupOpen(g)) {
		t.queue = append(t.queue, blankUnit{})
	}
	t.prev = g
}

// groupOpen reports whether g still has something producing into it: a live
// stream or a tool call that has not reached a terminal status. Groups with
// no producer — a user echo, a mission report, a turn notice — settle whole
// on arrival and are therefore never open. Neither is a card at the moment it
// settles: settleCard drops it from open first, so the run it just finished
// reads as closed to the separator rule, which is what it is.
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
