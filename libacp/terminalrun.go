package libacp

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const terminalDetachedTimeout = 5 * time.Second

// TerminalPeer is the subset of the ACP client side that RunTerminal drives;
// *AgentSideConnection satisfies it.
type TerminalPeer interface {
	CreateTerminal(context.Context, CreateTerminalRequest) (CreateTerminalResponse, error)
	TerminalOutput(context.Context, TerminalOutputRequest) (TerminalOutputResponse, error)
	WaitForTerminalExit(context.Context, WaitForTerminalExitRequest) (WaitForTerminalExitResponse, error)
	KillTerminal(context.Context, KillTerminalRequest) (KillTerminalResponse, error)
	ReleaseTerminal(context.Context, ReleaseTerminalRequest) (ReleaseTerminalResponse, error)
}

// Compile-time assertion: the real agent-side connection is a TerminalPeer.
var _ TerminalPeer = (*AgentSideConnection)(nil)

// TerminalResult is the reconciled outcome of one command run over a peer
// terminal; Cancelled and TimedOut are kept distinct (deadline vs. an
// external stop, usually session/cancel), and either way the terminal was
// killed before the result was read.
type TerminalResult struct {
	Output    string
	Truncated bool
	ExitCode  int
	Signal    *string
	Cancelled bool // ctx was cancelled; the terminal was killed
	TimedOut  bool // ctx hit its deadline; the terminal was killed
}

// RunTerminal creates a terminal on the peer, waits for it to exit, collects
// its output, and always releases it before returning, invoking onCreated
// (if non-nil) once the terminal exists but before the wait begins.
func RunTerminal(ctx context.Context, p TerminalPeer, req CreateTerminalRequest, onCreated func(terminalID string)) (TerminalResult, error) {
	createResp, err := p.CreateTerminal(ctx, req)
	if err != nil {
		return TerminalResult{ExitCode: -1}, fmt.Errorf("libacp terminal: create: %w", err)
	}
	termID := createResp.TerminalID

	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalDetachedTimeout)
		defer cancel()
		_, _ = p.ReleaseTerminal(rctx, ReleaseTerminalRequest{SessionID: req.SessionID, TerminalID: termID})
	}()

	if onCreated != nil {
		onCreated(termID)
	}

	exitResp, waitErr := p.WaitForTerminalExit(ctx, WaitForTerminalExitRequest{SessionID: req.SessionID, TerminalID: termID})

	var res TerminalResult
	if waitErr != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			res.TimedOut = true
		case ctx.Err() != nil:
			res.Cancelled = true
		default:
			res.ExitCode = -1
			return res, fmt.Errorf("libacp terminal: wait: %w", waitErr)
		}
		// Wait ended early; the process is still running — kill it before
		// reading what it produced.
		kctx, kcancel := context.WithTimeout(context.WithoutCancel(ctx), terminalDetachedTimeout)
		_, _ = p.KillTerminal(kctx, KillTerminalRequest{SessionID: req.SessionID, TerminalID: termID})
		kcancel()
	}

	octx, ocancel := context.WithTimeout(context.WithoutCancel(ctx), terminalDetachedTimeout)
	outputResp, oerr := p.TerminalOutput(octx, TerminalOutputRequest{SessionID: req.SessionID, TerminalID: termID})
	ocancel()
	if oerr != nil {
		res.ExitCode = -1
		return res, fmt.Errorf("libacp terminal: output: %w", oerr)
	}

	res.Output = outputResp.Output
	res.Truncated = outputResp.Truncated
	res.Signal = exitResp.Signal

	// Peers populate either the wait response's exit code or the one attached
	// to the output, not necessarily both — fall back rather than assume.
	switch {
	case exitResp.ExitCode != nil:
		res.ExitCode = *exitResp.ExitCode
	case outputResp.ExitStatus != nil && outputResp.ExitStatus.ExitCode != nil:
		res.ExitCode = *outputResp.ExitStatus.ExitCode
	}
	if res.Signal == nil && outputResp.ExitStatus != nil {
		res.Signal = outputResp.ExitStatus.Signal
	}
	// A signalled process did not exit cleanly even if no code was reported.
	if res.Signal != nil && res.ExitCode == 0 {
		res.ExitCode = -1
	}
	if res.Cancelled || res.TimedOut {
		res.ExitCode = -1
	}
	return res, nil
}
