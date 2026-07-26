package acpsvc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/contenox/beam/internal/services/localtools"
	libacp "github.com/contenox/beam/libacp"
)

var acpTerminalOutputByteLimit int64 = 1 * 1024 * 1024

type acpCommandRunner struct {
	transport func() *Transport
	shell     localtools.PlatformShell
}

func NewACPCommandRunner(transport func() *Transport) localtools.CommandRunner {
	return NewACPCommandRunnerWithShell(transport, localtools.DetectPlatformShell())
}

func NewACPCommandRunnerWithShell(transport func() *Transport, shell localtools.PlatformShell) localtools.CommandRunner {
	return &acpCommandRunner{transport: transport, shell: shell.WithDefaults()}
}

func (a *acpCommandRunner) Run(ctx context.Context, spec localtools.CommandSpec, stdout, stderr io.Writer) (int, error) {
	t := a.transport()
	if t == nil {
		return -1, errors.New("acpsvc: no active ACP transport")
	}
	if !t.getClientCaps().Terminal {
		return localtools.NewOSCommandRunnerWithShell(a.shell).Run(ctx, spec, stdout, stderr)
	}

	command, cmdArgs, titleCmd := a.terminalCommand(spec)
	const titleMax = 80
	if len(titleCmd) > titleMax {
		titleCmd = titleCmd[:titleMax-3] + "..."
	}
	title := "local_shell: " + titleCmd

	req := libacp.CreateTerminalRequest{
		Command:         command,
		Args:            cmdArgs,
		OutputByteLimit: &acpTerminalOutputByteLimit,
	}
	sid := resolveACPSessionID(ctx, t)
	if sid != "" {
		req.SessionID = sid
	}
	if spec.Cwd != "" {
		req.Cwd = spec.Cwd
	} else if sid != "" {
		internal := sessionIDFromCtx(ctx)
		t.sessionMu.Lock()
		for _, entry := range t.sessions {
			if entry.InternalSessionID == internal && entry.Cwd != "" {
				req.Cwd = entry.Cwd
				break
			}
		}
		t.sessionMu.Unlock()
	}

	// The protocol dance — create, wait, kill-on-cancel, fetch output on a
	// detached context, release, reconcile the exit code from both sources —
	// is generic and lives in libacp. Everything below it is service policy:
	// which terminal to surface upstream, how to render the outcome for beam,
	// and how a truncated output maps onto the tool budget.
	res, err := libacp.RunTerminal(ctx, t.conn, req, func(terminalID string) {
		if sid == "" {
			return
		}
		if tcID := toolCallIDFromCtx(ctx); tcID != "" {
			t.sendUpdate(ctx, terminalAttachNotification(sid, tcID, terminalID, title))
		}
	})
	if err != nil && !res.Cancelled && !res.TimedOut {
		return -1, fmt.Errorf("acpsvc terminal: %w", err)
	}

	// A cancelled or timed-out command still reports why it stopped, even when
	// the output fetch itself failed — the cause is more useful than the fetch
	// error, and the model must not be told a user cancellation was a timeout.
	if err != nil {
		if res.Cancelled {
			return -1, fmt.Errorf("acpsvc terminal: command cancelled: %w", context.Canceled)
		}
		return -1, fmt.Errorf("acpsvc terminal: command timed out")
	}

	if res.Truncated {
		return -1, localtools.ErrOutputBudgetExceeded
	}

	if res.Output != "" {
		// Trim excessive trailing newlines from terminal output to avoid UI padding.
		// Preserve at most 2 trailing newlines (common for command output).
		output := res.Output
		for strings.HasSuffix(output, "\n\n\n") {
			output = strings.TrimSuffix(output, "\n")
		}
		_, _ = io.WriteString(stdout, output)
	}

	if res.Cancelled {
		_, _ = io.WriteString(stdout, "\n[command killed: cancelled by user]")
		return -1, fmt.Errorf("acpsvc terminal: command cancelled: %w", context.Canceled)
	}
	if res.TimedOut {
		_, _ = io.WriteString(stdout, "\n[command killed: timeout exceeded]")
		return -1, fmt.Errorf("acpsvc terminal: command timed out")
	}

	if res.Signal != nil {
		_, _ = io.WriteString(stdout, fmt.Sprintf("\n[terminated by signal %s]", *res.Signal))
	}
	return res.ExitCode, nil
}

func (a *acpCommandRunner) terminalCommand(spec localtools.CommandSpec) (command string, args []string, title string) {
	title = spec.Command
	if len(spec.Args) > 0 {
		title += " " + strings.Join(spec.Args, " ")
	}
	if !spec.UseShell {
		return spec.Command, spec.Args, title
	}
	shell := spec.Shell
	if !shell.IsSet() {
		shell = a.shell
	}
	command, args, _ = shell.WrapCommand(spec.Command, spec.Args)
	return command, args, title
}
