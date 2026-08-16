package acpsvc

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/contenox/contenox/internal/services/localtools"
	libacp "github.com/contenox/contenox/libacp"
)

var acpTerminalOutputByteLimit int64 = 1 * 1024 * 1024

type acpCommandRunner struct {
	transport TransportResolver
	shell     localtools.PlatformShell
}

func NewACPCommandRunner(transport TransportResolver) localtools.CommandRunner {
	return NewACPCommandRunnerWithShell(transport, localtools.DetectPlatformShell())
}

func NewACPCommandRunnerWithShell(transport TransportResolver, shell localtools.PlatformShell) localtools.CommandRunner {
	return &acpCommandRunner{transport: transport, shell: shell.WithDefaults()}
}

func (a *acpCommandRunner) Run(ctx context.Context, spec localtools.CommandSpec, stdout, stderr io.Writer) (int, error) {
	var t *Transport
	if a.transport != nil {
		t = a.transport(ctx)
	}
	if t == nil || !t.getClientCaps().Terminal {
		return 0, localtools.ErrNoTerminal
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

	// A cancelled or timed-out command reports why it stopped, even when the
	// output fetch itself failed.
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
