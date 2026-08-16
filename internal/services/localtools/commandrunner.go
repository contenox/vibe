package localtools

import (
	"context"
	"errors"
	"io"
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

var ErrNoTerminal = errors.New("no terminal: the connected client provides none and contenox does not spawn on the host")
