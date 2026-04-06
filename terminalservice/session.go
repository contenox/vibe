package terminalservice

import (
	"context"
	"os"
	"os/exec"
	"sync/atomic"
)

// session holds a PTY master and child process (Unix). On Windows, Create does not run.
type session struct {
	id   string
	tty  *os.File
	cmd  *exec.Cmd
	busy atomic.Bool // at most one WebSocket attachment
}

func (s *session) shutdown(_ context.Context) error {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	if s.tty != nil {
		_ = s.tty.Close()
	}
	return nil
}
