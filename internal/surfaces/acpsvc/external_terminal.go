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
// agent bridge: a downstream agent's shell command routes through the
// runtime's own shell-session machinery (the same surface the `!` passthrough
// uses) instead of running inside the downstream process, streaming live to
// the upstream panel via contenox.terminalOutput with the bridge's internal
// START/END framing stripped.
//
// The session shell is one persistent PTY per chat session, so concurrent
// terminals in one session serialize through it. No additional contenox HITL
// gate applies here: authorization is the downstream agent's own
// session/request_permission, already forwarded upstream (external.go).

// terminalEraseSeq clears the current line so a VT panel shows nothing for a
// marker while its bytes remain in the raw scrollback for the bridge to parse.
const terminalEraseSeq = "\x1b[2K\r"

// bridgeTerminal tracks one downstream-created terminal: its slice of the
// shared scrollback and resolved exit status. Its watch goroutine owns the
// scrollback subscription for the terminal's lifetime.
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

// locate slices the command's output from raw using the START/END markers,
// parsing the exit code when END is present. sawStart is false once START has
// aged out of the scrollback ring; sawEnd is false while still running.
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
	// Strip the START erase sequence and the newline END's printf injects ahead
	// of its marker.
	out = strings.TrimPrefix(out, terminalEraseSeq)
	out = strings.TrimSuffix(out, "\n")
	return out, sawStart, sawEnd, code
}

// watch resolves the terminal's exit and streams its real output to the
// upstream panel, both driven off locate. It is event-driven: a shellsession
// subscription wakes it on each output flush and it rescans the full
// scrollback, so a marker split across flushes is handled naturally.
// panelGuard holds back a trailing window so a half-written END marker is
// never forwarded. It exits on the END marker, on done closing (kill/release/
// teardown), or when the connection ends.
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
				// Hold back panelGuard bytes: END may still be partially written.
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

// CreateTerminal spawns the downstream agent's command in the runtime's session
// shell and returns a terminal id for the other terminal/* calls. Real output
// streams live to the upstream terminal panel through the per-terminal filtered
// forwarder in bt.watch, not the raw session subscription, so the bridge's
// wrapper line and framing markers never reach it.
func (b *externalBridge) CreateTerminal(_ context.Context, req libacp.CreateTerminalRequest) (libacp.CreateTerminalResponse, error) {
	// terminal/* only arrives mid-turn, when an upstream transport is attached;
	// detached, there is nothing to run the command against.
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

	// session/load and session/resume subscribe a raw panel feed even for
	// external sessions, which would leak this terminal's wrapper line; drop it
	// so bt.watch's filtered forwarder is the sole panel feed.
	t.unsubscribeTerminal(b.upstreamID)

	nonce := strings.ReplaceAll(uuid.NewString(), "-", "")
	startTok := "CTXS" + nonce
	endTok := "CTXE" + nonce

	// Capture the scrollback boundary before submitting so this command's output
	// is always at or after it.
	startOffset := mgr.Read(internalID, 0, 0).NextOffset

	var byteLimit int64
	if req.OutputByteLimit != nil && *req.OutputByteLimit > 0 {
		byteLimit = *req.OutputByteLimit
	}

	line := composeTerminalCommand(req, startTok, endTok)
	// shellsession.Run reads the session id from ctx to root the shell at the
	// workspace; bind to connCtx so the capture window respects connection lifetime.
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
		// panelGuard must cover the END marker's longest unmatched printed prefix
		// ("\n" + endTok + " ") plus slack for the exit-code digits.
		panelGuard: len(endTok) + 16,
		done:       make(chan struct{}),
	}
	b.termMu.Lock()
	if b.terminals == nil {
		b.terminals = make(map[string]*bridgeTerminal)
	}
	b.terminals[bt.id] = bt
	b.termMu.Unlock()

	// Sent on the request goroutine, strictly before bt.watch starts forwarding
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
// status. Output is truncated (tail-kept) to the request's byte limit, and a
// START marker aged out of the scrollback ring also reports truncated.
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

// WaitForTerminalExit blocks until the command exits, is killed/released, or the
// connection tears down, then returns the exit code or signal. A cancelled ctx
// returns that error, leaving the command untouched.
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

// KillTerminal interrupts the running command via Ctrl-C typed into the shared
// PTY, never a whole-shell kill (which would take down sibling terminals). A
// command that ignores SIGINT keeps running; this is a shared-shell limit.
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

// ReleaseTerminal drops the terminal and frees its watcher. A still-running
// command is killed (Ctrl-C, as in KillTerminal) before its handle is forgotten.
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
		// finish is a no-op here; this just ensures done is closed.
		bt.finish(bt.exitCode, bt.signal)
	}
	return libacp.ReleaseTerminalResponse{}, nil
}

// closeAllTerminals tears down every live terminal for this bridge. Called from
// externalDriver.Close; the serve WebSocket path (which never calls Close) is
// covered instead by each watcher's connCtx.Done() branch.
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

// composeTerminalCommand builds the shell line for a terminal: an erase-wrapped
// START marker, the command in a subshell (under `env`/`cd` so neither leaks
// into the persistent shell), then the exit-code END marker.
//
// With Args empty, req.Command is a full shell command line and must run via
// `bash -c` so pipes/redirects/word-splitting work — quoting it as a single
// execvp atom fails with exit 127. With Args non-empty it is execvp-style,
// each atom quoted separately, no shell.
func composeTerminalCommand(req libacp.CreateTerminalRequest, startTok, endTok string) string {
	var exec string
	if len(req.Args) == 0 {
		exec = "bash -c " + shellQuoteArg(req.Command)
	} else {
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

// startMarkerRegexp / endMarkerRegexp locate a marker's printed form: both
// require a digit right after the token, so the `%d` in the echoed format
// string never matches — only the printed output does. The END regex captures
// the exit code.
func startMarkerRegexp(tok string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(tok) + `(\d)`)
}

func endMarkerRegexp(tok string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(tok) + ` (\d+)`)
}
