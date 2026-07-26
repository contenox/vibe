package acpsvc

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/contenox/beam/internal/services/shellsession"
	"github.com/contenox/beam/internal/store/runtimetypes"
	libacp "github.com/contenox/beam/libacp"
)

// This file implements the terminal/* client-callback family for the external-
// agent bridge (externalBridge, external.go). When a DOWNSTREAM agent (e.g.
// claude-code-acp) runs a shell command, it calls terminal/create + terminal/*
// on its client — which here is the runtime. Instead of the downstream agent
// running the command inside its own process (surfacing only as a raw tool_call
// card, with beam's terminal panel left empty), the bridge routes the command
// through the RUNTIME's own shell-session machinery (runtime/shellsession) — the
// exact surface the `!` passthrough and the native shell_session tools use. The
// command's output therefore also streams live into beam's terminal panel via
// the contenox.terminalOutput session/update path (see terminal.go), so the user
// watches a foreign agent's shell activity exactly as they watch a native turn's.
//
// Unlike the `!` passthrough — which forwards the shell's RAW scrollback verbatim
// (the user typed that line, so its echo and prompt belong on screen) — the panel
// feed for an agent's terminal is FILTERED per-terminal by bt.watch: only the
// bytes between the bridge's START/END markers reach the panel, so the wrapper
// line, the markers, and their erase sequences (internal bookkeeping) are never
// shown. beam's panel is an append-only log view that does not process cursor
// controls, so relying on the `\033[2K\r` erase to hide the markers would leak
// them; the markers are instead used only to slice, never forwarded.
//
// # Mapping onto the shared session shell
//
// runtime/shellsession gives ONE persistent PTY-backed shell per chat session
// (keyed by the internal session id) and a line-oriented Run — it has no native
// per-command exit code and no per-terminal isolation. So each terminal/create
// submits one wrapped line to the session's shell and a bridgeTerminal tracks
// just that command's slice of the shared scrollback:
//
//   - a START marker (printed, then erased from the VT-rendered panel) precedes
//     the command so its output is isolated from the echoed input line regardless
//     of PTY line-wrapping;
//   - the command runs in a subshell so `$?` is its own exit status;
//   - an END marker carries that exit code.
//
// Both markers embed a runtime value (`printf '…%d' "$?"`) so the format string
// as it appears in the ECHOED input line never matches the OUTPUT regex
// (`marker <digits>`) — only the printed marker does. A per-terminal scrollback
// watcher (event-driven off shellsession subscriptions, never a timer poll)
// detects the END marker to resolve WaitForTerminalExit and recover the exit
// code. Because the shell is shared and persistent, concurrent terminals in one
// session serialize through the same PTY — the same property the `!` passthrough
// and native shell_session tools already have; it is a documented consequence of
// the shell-session design, not a shortcut here.
//
// # Gating
//
// A foreign agent's terminal command runs on the runtime's shell exactly like
// the `!` passthrough (direct shellsession.Run, no additional contenox HITL gate).
// The external path never runs through contenox's chain engine or hitlservice, so
// there is no native HITL machinery to invoke; the authorization for the agent's
// tool USE is the downstream agent's own session/request_permission, which the
// bridge already forwards to the upstream user (RequestPermission, external.go) —
// the external analogue of the native tool-call HITL approval. Adding a second,
// contenox-side gate here would be inventing new policy, so it is deliberately not
// done. This mirrors the existing native gating precisely.

// terminalEraseSeq clears the current line and returns the cursor to column 0.
// Emitted around each marker so a VT-rendering panel (beam) shows nothing while
// the raw scrollback still carries the marker bytes for the bridge to parse.
const terminalEraseSeq = "\x1b[2K\r"

// bridgeTerminal tracks one downstream-created terminal: its slice of the shared
// session scrollback and its resolved exit status. Its watch goroutine owns the
// scrollback subscription and lives until the terminal exits, is killed/released,
// or the connection tears down.
type bridgeTerminal struct {
	id          string
	internalID  string // runtime shell-session id (the upstream session's internal id)
	startOffset int64  // scrollback end offset captured before the command was submitted
	startRe     *regexp.Regexp
	endRe       *regexp.Regexp
	byteLimit   int64 // OutputByteLimit from the request; 0 = unlimited
	panelGuard  int   // trailing bytes held back from the panel while the END marker may be partial

	mu       sync.Mutex
	exited   bool
	exitCode *int
	signal   *string

	done     chan struct{}
	doneOnce sync.Once
}

// finish records the terminal's terminal status once (first writer wins) and
// closes done, unblocking WaitForTerminalExit and stopping the watch goroutine.
func (bt *bridgeTerminal) finish(code *int, signal *string) {
	bt.mu.Lock()
	if !bt.exited {
		bt.exited = true
		bt.exitCode = code
		bt.signal = signal
	}
	bt.mu.Unlock()
	bt.doneOnce.Do(func() { close(bt.done) })
}

func (bt *bridgeTerminal) isExited() bool {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	return bt.exited
}

// locate slices the command's output out of the raw scrollback (read since
// startOffset) using the START/END markers, and, when the END marker is present,
// parses the exit code. sawStart is false when the START marker has aged out of
// the bounded scrollback ring (a > ring-capacity command) — the output is then
// truncated. sawEnd is false while the command is still running.
func (bt *bridgeTerminal) locate(raw string) (out string, sawStart, sawEnd bool, code *int) {
	lo := 0
	if m := bt.startRe.FindStringIndex(raw); m != nil {
		sawStart = true
		lo = m[1]
	}
	hi := len(raw)
	if m := bt.endRe.FindStringSubmatchIndex(raw); m != nil {
		sawEnd = true
		hi = m[0]
		if v, err := strconv.Atoi(raw[m[2]:m[3]]); err == nil {
			code = &v
		}
	}
	if hi < lo {
		hi = lo
	}
	out = raw[lo:hi]
	// Strip the START marker's own trailing erase sequence and the newline the END
	// printf injects ahead of its marker, leaving just the command's real output.
	out = strings.TrimPrefix(out, terminalEraseSeq)
	out = strings.TrimSuffix(out, "\n")
	return out, sawStart, sawEnd, code
}

// watch resolves the terminal's exit AND feeds the upstream client's terminal
// panel with the command's REAL output, both driven off the same START/END marker
// locate. It is event-driven: a shellsession subscription pokes signal on each
// output flush and the loop rescans the shared scrollback since startOffset, so
// there is no timer poll.
//
// Panel feed: only the bytes BETWEEN the markers are forwarded (via panel), so the
// echoed wrapper line, the START/END markers, and their `\033[2K\r` erase
// sequences — the bridge's internal framing — never reach beam's append-only
// terminal panel; it shows just what the command printed. Because locate is rerun
// against the WHOLE accumulated scrollback each poke (not per-chunk), a marker
// split across two flushes is handled naturally, and output streams incrementally
// as it arrives (only the newly-located bytes are sent). While the END marker is
// not yet fully present a trailing window (panelGuard) is held back so a
// half-written marker is never forwarded — it flushes the instant the marker
// completes.
//
// It exits when the END marker appears (recording the exit code AFTER forwarding
// the last of the output), when done is closed by another lifecycle path
// (kill/release/teardown), or when the connection ends.
func (bt *bridgeTerminal) watch(mgr shellsession.Manager, connDone <-chan struct{}, panel func(string)) {
	signal := make(chan struct{}, 1)
	cancel := mgr.Subscribe(bt.internalID, func(shellsession.Chunk) {
		select {
		case signal <- struct{}{}:
		default:
		}
	})
	defer cancel()

	forwarded := 0 // bytes of the located output already sent to the panel
	for {
		raw := mgr.Read(bt.internalID, bt.startOffset, 0).Content
		out, sawStart, sawEnd, code := bt.locate(raw)
		if panel != nil && sawStart {
			end := len(out)
			if !sawEnd {
				// The END marker may be only partially in the scrollback; hold back a
				// trailing window that could be a nascent marker so framing never leaks.
				if end -= bt.panelGuard; end < 0 {
					end = 0
				}
			}
			if end > forwarded {
				panel(out[forwarded:end])
				forwarded = end
			}
		}
		if sawEnd {
			bt.finish(code, nil)
			return
		}
		select {
		case <-signal:
		case <-bt.done:
			return
		case <-connDone:
			sig := "SIGHUP"
			bt.finish(nil, &sig)
			return
		}
	}
}

// CreateTerminal spawns the downstream agent's command in the RUNTIME's session
// shell (rooted at the session workspace via the same cwd resolver the native
// tools use) and returns a terminal id the agent tracks with the other terminal/*
// calls. The command's REAL output also streams live to the upstream client's
// terminal panel, but through the per-terminal FILTERED forwarder in bt.watch (not
// the raw session subscription): only the bytes between the START/END markers are
// forwarded, so the bridge's wrapper line and framing markers never reach the
// append-only panel. A clean `$ <command>` header is emitted first so the panel
// reads like a terminal session.
func (b *externalBridge) CreateTerminal(_ context.Context, req libacp.CreateTerminalRequest) (libacp.CreateTerminalResponse, error) {
	// A downstream agent only calls terminal/* while running a prompt turn, which is
	// driven by an attached upstream transport — so the relay target is the connection
	// hosting this turn. Detached (no turn in flight) there is nothing to run against.
	t := b.transport()
	if t == nil {
		return libacp.CreateTerminalResponse{}, libacp.MethodNotFound(libacp.MethodTerminalCreate)
	}
	mgr := t.deps.ShellSessions
	if mgr == nil {
		return libacp.CreateTerminalResponse{}, libacp.MethodNotFound(libacp.MethodTerminalCreate)
	}
	entry, ok := t.sessionFor(b.upstreamID)
	if !ok || entry.InternalSessionID == "" {
		return libacp.CreateTerminalResponse{}, libacp.NewError(libacp.ErrInvalidParams, "acpsvc terminal: session is not open")
	}
	internalID := entry.InternalSessionID

	// Drop any RAW session-scoped panel subscription so it cannot forward this
	// terminal's wrapper line and framing markers verbatim. An external session
	// never subscribes at session/new, but session/load and session/resume do
	// (unconditionally, even for external sessions), so a reopened session can carry
	// a raw subscription; remove it and let the per-terminal FILTERED forwarder in
	// bt.watch (below) be the sole feed for the panel. The `!` passthrough
	// re-subscribes on its own next run, unaffected.
	t.unsubscribeTerminal(b.upstreamID)

	nonce := strings.ReplaceAll(uuid.NewString(), "-", "")
	startTok := "CTXS" + nonce
	endTok := "CTXE" + nonce

	// Capture the scrollback boundary BEFORE submitting, so the START marker (and
	// hence this command's output) is always at or after it.
	startOffset := mgr.Read(internalID, 0, 0).NextOffset

	var byteLimit int64
	if req.OutputByteLimit != nil && *req.OutputByteLimit > 0 {
		byteLimit = *req.OutputByteLimit
	}

	line := composeTerminalCommand(req, startTok, endTok)
	// shellsession.Run reads the internal session id from ctx to root a fresh shell
	// at the session workspace (exactly like handleTerminalRun); bind it to connCtx
	// so its brief capture window respects connection lifetime.
	runCtx := context.WithValue(t.connCtx, runtimetypes.SessionIDContextKey, internalID)
	if _, err := mgr.Run(runCtx, internalID, line); err != nil {
		return libacp.CreateTerminalResponse{}, libacp.InternalError("acpsvc terminal: run: " + err.Error())
	}

	bt := &bridgeTerminal{
		id:          "ext-term-" + nonce,
		internalID:  internalID,
		startOffset: startOffset,
		startRe:     startMarkerRegexp(startTok),
		endRe:       endMarkerRegexp(endTok),
		byteLimit:   byteLimit,
		// While the END marker is not yet fully in the scrollback its longest
		// unmatched printed prefix is "\n" + endTok + " " (token + separating space,
		// before the exit-code digit that completes the regex). Hold back at least
		// that many trailing bytes from the panel so a half-written marker never
		// leaks; the slack keeps it safe against the leading newline and any digits.
		panelGuard: len(endTok) + 16,
		done:       make(chan struct{}),
	}
	b.termMu.Lock()
	if b.terminals == nil {
		b.terminals = make(map[string]*bridgeTerminal)
	}
	b.terminals[bt.id] = bt
	b.termMu.Unlock()

	// Emit a clean `$ <command>` header (the AGENT's requested command, never the
	// bridge's bash -c wrapper) before the output flows, so the panel shows what ran.
	// Sent here, on the request goroutine, strictly before bt.watch starts forwarding
	// output, so the header always precedes the command's output on the wire.
	display := req.Command
	if len(req.Args) > 0 {
		display = req.Command + " " + strings.Join(req.Args, " ")
	}
	t.sendTerminalChunk(b.upstreamID, shellsession.Chunk{Data: "$ " + display + "\n"})

	go bt.watch(mgr, t.connCtx.Done(), func(chunk string) {
		t.sendTerminalChunk(b.upstreamID, shellsession.Chunk{Data: chunk})
	})

	return libacp.CreateTerminalResponse{TerminalID: bt.id}, nil
}

// TerminalOutput returns the terminal's current output and, once known, its exit
// status. Output is sliced from the shared scrollback by the terminal's markers
// and truncated (tail-kept) to the request's byte limit when one was set; a START
// marker aged out of the bounded ring also reports truncated.
func (b *externalBridge) TerminalOutput(_ context.Context, req libacp.TerminalOutputRequest) (libacp.TerminalOutputResponse, error) {
	t := b.transport()
	if t == nil {
		return libacp.TerminalOutputResponse{}, libacp.MethodNotFound(libacp.MethodTerminalOutput)
	}
	mgr := t.deps.ShellSessions
	if mgr == nil {
		return libacp.TerminalOutputResponse{}, libacp.MethodNotFound(libacp.MethodTerminalOutput)
	}
	bt, err := b.lookupTerminal(req.TerminalID)
	if err != nil {
		return libacp.TerminalOutputResponse{}, err
	}

	raw := mgr.Read(bt.internalID, bt.startOffset, 0).Content
	out, sawStart, _, code := bt.locate(raw)
	truncated := !sawStart
	if bt.byteLimit > 0 && int64(len(out)) > bt.byteLimit {
		out = out[int64(len(out))-bt.byteLimit:]
		truncated = true
	}

	resp := libacp.TerminalOutputResponse{Output: out, Truncated: truncated}
	bt.mu.Lock()
	switch {
	case bt.exited:
		resp.ExitStatus = &libacp.TerminalExitStatus{ExitCode: bt.exitCode, Signal: bt.signal}
	case code != nil:
		// The END marker is on the wire but the watcher has not recorded it yet.
		resp.ExitStatus = &libacp.TerminalExitStatus{ExitCode: code}
	}
	bt.mu.Unlock()
	return resp, nil
}

// WaitForTerminalExit blocks until the command exits (its END marker is observed),
// the terminal is killed/released, or the connection tears down, then returns the
// exit code or signal. A downstream $/cancel_request (or connection drop) cancels
// ctx and the wait returns that error, leaving the command untouched.
func (b *externalBridge) WaitForTerminalExit(ctx context.Context, req libacp.WaitForTerminalExitRequest) (libacp.WaitForTerminalExitResponse, error) {
	bt, err := b.lookupTerminal(req.TerminalID)
	if err != nil {
		return libacp.WaitForTerminalExitResponse{}, err
	}
	select {
	case <-bt.done:
	case <-ctx.Done():
		return libacp.WaitForTerminalExitResponse{}, ctx.Err()
	}
	bt.mu.Lock()
	defer bt.mu.Unlock()
	return libacp.WaitForTerminalExitResponse{ExitCode: bt.exitCode, Signal: bt.signal}, nil
}

// KillTerminal interrupts the running command. The session shell is shared and
// persistent, so the only per-command lever is Ctrl-C (SIGINT) typed into the PTY
// — never a whole-shell kill, which would take down the panel and any sibling
// terminals. A command that ignores SIGINT keeps running in the shell; this is a
// documented limit of the shared-shell model. The terminal is then resolved with
// a SIGINT signal so WaitForTerminalExit returns.
func (b *externalBridge) KillTerminal(_ context.Context, req libacp.KillTerminalRequest) (libacp.KillTerminalResponse, error) {
	bt, err := b.lookupTerminal(req.TerminalID)
	if err != nil {
		return libacp.KillTerminalResponse{}, err
	}
	if !bt.isExited() {
		b.interrupt(bt)
		sig := "SIGINT"
		bt.finish(nil, &sig)
	}
	return libacp.KillTerminalResponse{}, nil
}

// ReleaseTerminal drops the terminal and frees its watcher. Per spec a still-
// running command is killed on release, so an unresolved terminal is interrupted
// (Ctrl-C, as in KillTerminal) before its handle is forgotten.
func (b *externalBridge) ReleaseTerminal(_ context.Context, req libacp.ReleaseTerminalRequest) (libacp.ReleaseTerminalResponse, error) {
	bt, ok := b.removeTerminal(req.TerminalID)
	if !ok {
		return libacp.ReleaseTerminalResponse{}, libacp.NewErrorf(libacp.ErrInvalidParams, "acpsvc terminal: unknown terminal %q", req.TerminalID)
	}
	if !bt.isExited() {
		b.interrupt(bt)
		sig := "SIGINT"
		bt.finish(nil, &sig)
	} else {
		// Already exited: finish is a no-op, but close done idempotently so the
		// watcher goroutine has certainly stopped.
		bt.finish(bt.exitCode, bt.signal)
	}
	return libacp.ReleaseTerminalResponse{}, nil
}

// closeAllTerminals tears down every live terminal for this bridge. Called from
// externalDriver.Close (explicit session/close or stdio Transport.Close); the
// serve WebSocket path, which never calls Close, is covered by each watcher's
// connCtx.Done() branch instead.
func (b *externalBridge) closeAllTerminals() {
	b.termMu.Lock()
	terms := make([]*bridgeTerminal, 0, len(b.terminals))
	for _, bt := range b.terminals {
		terms = append(terms, bt)
	}
	b.terminals = nil
	b.termMu.Unlock()
	sig := "SIGHUP"
	for _, bt := range terms {
		bt.finish(nil, &sig)
	}
}

func (b *externalBridge) lookupTerminal(id string) (*bridgeTerminal, error) {
	b.termMu.Lock()
	defer b.termMu.Unlock()
	if bt, ok := b.terminals[id]; ok {
		return bt, nil
	}
	return nil, libacp.NewErrorf(libacp.ErrInvalidParams, "acpsvc terminal: unknown terminal %q", id)
}

func (b *externalBridge) removeTerminal(id string) (*bridgeTerminal, bool) {
	b.termMu.Lock()
	defer b.termMu.Unlock()
	bt, ok := b.terminals[id]
	if ok {
		delete(b.terminals, id)
	}
	return bt, ok
}

// interrupt types Ctrl-C into the session shell to SIGINT the foreground command,
// but only when the shell still exists — Run would otherwise recreate a shell just
// to signal into it.
func (b *externalBridge) interrupt(bt *bridgeTerminal) {
	t := b.transport()
	if t == nil {
		return
	}
	mgr := t.deps.ShellSessions
	if mgr == nil || !mgr.Read(bt.internalID, bt.startOffset, 0).Exists {
		return
	}
	runCtx := context.WithValue(t.connCtx, runtimetypes.SessionIDContextKey, bt.internalID)
	_, _ = mgr.Run(runCtx, bt.internalID, "\x03")
}

// composeTerminalCommand builds the single shell line submitted for a terminal:
// START marker, the command (in a subshell so `$?` is the command's exit, under
// `env` for the request env and a `cd` for its cwd so neither leaks into the
// persistent shell), then the exit-code END marker. The markers are wrapped in
// erase sequences so a VT-rendered panel hides them; their bytes remain in the
// raw scrollback for the watcher to parse.
//
// The command has two request shapes, per the ACP terminal spec and how real
// adapters use it:
//
//   - Args EMPTY: req.Command is a full SHELL COMMAND LINE. This is what
//     claude-code-acp sends — e.g. "echo hello", "git status -s | head" — the
//     `command` field carries a bash line, not an execvp program. It MUST run
//     through a shell so word-splitting, pipes, and redirects work; quoting it as
//     a single execvp atom (`'echo hello'`) makes env/exec look for a program
//     literally named "echo hello" and fail with exit 127. So it runs as
//     `bash -c <line>`, with the whole subshell capturing its exit code.
//   - Args NON-EMPTY: execvp-style program + argv — the command and each arg are
//     quoted separately and run directly, no shell.
//
// env (when present) applies to the whole invocation in both shapes; cwd is
// entered inside the subshell so neither leaks into the persistent session shell.
func composeTerminalCommand(req libacp.CreateTerminalRequest, startTok, endTok string) string {
	var exec string
	if len(req.Args) == 0 {
		// A shell command line: hand it to bash verbatim so pipes/redirects/word-
		// splitting all work.
		exec = "bash -c " + shellQuoteArg(req.Command)
	} else {
		// execvp-style: program + argv, each a separate quoted atom.
		parts := make([]string, 0, 1+len(req.Args))
		parts = append(parts, shellQuoteArg(req.Command))
		for _, a := range req.Args {
			parts = append(parts, shellQuoteArg(a))
		}
		exec = strings.Join(parts, " ")
	}
	if len(req.Env) > 0 {
		env := make([]string, 0, len(req.Env)+1)
		env = append(env, "env")
		for _, e := range req.Env {
			env = append(env, shellQuoteArg(e.Name)+"="+shellQuoteArg(e.Value))
		}
		exec = strings.Join(env, " ") + " " + exec
	}
	if req.Cwd != "" {
		exec = "cd " + shellQuoteArg(req.Cwd) + " && " + exec
	}

	var sb strings.Builder
	// printf 'START%d<erase>' 0
	sb.WriteString("printf '")
	sb.WriteString(startTok)
	sb.WriteString(`%d\033[2K\r' 0;`)
	// ( <command> ); capture exit
	sb.WriteString("( ")
	sb.WriteString(exec)
	sb.WriteString(" );__ce=$?;")
	// printf '\nEND %d<erase>' "$__ce"
	sb.WriteString(`printf '\n`)
	sb.WriteString(endTok)
	sb.WriteString(` %d\033[2K\r' "$__ce"`)
	return sb.String()
}

// shellQuoteArg single-quotes s for safe interpolation into a POSIX shell line,
// escaping embedded single quotes.
func shellQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// startMarkerRegexp / endMarkerRegexp compile the regexes that locate a marker's
// PRINTED form in the scrollback. Both require a digit immediately after the token
// (the `%d`-substituted value), so the marker's format string as it appears in the
// echoed input line — which carries a literal `%d`, not a digit — never matches;
// only the printed output does. The END regex captures the exit code.
func startMarkerRegexp(tok string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(tok) + `(\d)`)
}

func endMarkerRegexp(tok string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(tok) + ` (\d+)`)
}
