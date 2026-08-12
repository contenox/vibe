package localtools

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

var ErrOutputBudgetExceeded = errors.New("local_shell: command output exceeded the remaining context budget")

type CommandSpec struct {
	Command  string
	Args     []string
	Cwd      string
	Timeout  time.Duration
	UseShell bool
	Shell    PlatformShell
	Stdin    string
}

type CommandRunner interface {
	Run(ctx context.Context, spec CommandSpec, stdout, stderr io.Writer) (exitCode int, err error)
}

func NewOSCommandRunner() CommandRunner {
	return NewOSCommandRunnerWithShell(DetectPlatformShell())
}

func NewOSCommandRunnerWithShell(shell PlatformShell) CommandRunner {
	return osCommandRunner{shell: shell.WithDefaults()}
}

// NewOSCommandRunnerWithShellAndScrub is NewOSCommandRunnerWithShell plus an environment scrub applied in Run when set: scrub maps the parent environment to the one the spawned command inherits, so serve's own credentials do not ride into an LLM-driven shell.
func NewOSCommandRunnerWithShellAndScrub(shell PlatformShell, scrub func([]string) []string) CommandRunner {
	return osCommandRunner{shell: shell.WithDefaults(), scrub: scrub}
}

type osCommandRunner struct {
	shell PlatformShell
	scrub func([]string) []string
}

func (r osCommandRunner) Run(ctx context.Context, spec CommandSpec, stdout, stderr io.Writer) (int, error) {
	var cmd *exec.Cmd
	if spec.UseShell {
		shell := spec.Shell
		if !shell.IsSet() {
			shell = r.shell
		}
		program, args, _ := shell.WrapCommand(spec.Command, spec.Args)
		cmd = exec.CommandContext(ctx, program, args...)
	} else {
		cmd = exec.CommandContext(ctx, spec.Command, spec.Args...)
	}
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	// cmd.Env nil means "inherit os.Environ()", handing the child every secret in serve's environment; the scrub replaces it to close that leak.
	if r.scrub != nil {
		cmd.Env = r.scrub(os.Environ())
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if spec.Stdin != "" {
		cmd.Stdin = strings.NewReader(spec.Stdin)
	}
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), err
		}
		return -1, err
	}
	return 0, nil
}
