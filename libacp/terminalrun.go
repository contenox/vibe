package libacp

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// terminalDetachedTimeout bounds the out-of-band calls (release, kill, output)
// RunTerminal makes on a context deliberately not the caller's, since those
// calls happen precisely when the caller's context is already dead.
const terminalDetachedTimeout = 5 * time.Second

// TerminalPeer is the subset of the ACP client side that RunTerminal drives.
// *AgentSideConnection satisfies it; tests and alternative transports can supply
// their own implementation.
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
// terminal. Cancelled and TimedOut are kept distinct: a deadline means the
// command ran out of budget, a cancellation means something (usually
// session/cancel) stopped the turn. Either way the terminal was killed before
// the result was read.
type TerminalResult struct {
	Output    string
	Truncated bool
	ExitCode  int
	Signal    *string
	Cancelled bool // ctx was cancelled; the terminal was killed
	TimedOut  bool // ctx hit its deadline; the terminal was killed
}

// RunTerminal creates a terminal on the peer, waits for it to exit, collects
// its output and releases it, returning the reconciled result.
//
// onCreated, when non-nil, is invoked after the terminal exists but before
// the wait begins — the seam for callers that need to surface the live
// terminal (e.g. attaching it to a tool call in a UI).
//
// ctx governs only create and wait; release, kill and the output fetch run on
// detached contexts since they matter most exactly when ctx is already dead.
// The terminal is always released before returning.
//
// A non-nil error means the protocol exchange itself failed (create, a
// non-ctx wait failure, or output read); the result still carries the
// Cancelled/TimedOut flags established so far. Policy decisions (Truncated as
// a budget error, banners, exit-status mapping) belong to the caller.
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
