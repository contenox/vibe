package terminalservice

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

type session struct {
	id                    string
	tty                   *os.File
	input                 *os.File
	output                *os.File
	cmd                   *exec.Cmd
	pseudoConsole         uintptr
	processHandle         uintptr
	waitOwnsProcessHandle bool
	busy                  atomic.Bool
	attachMu              sync.Mutex
	attachCancel          context.CancelFunc
	attachGen             atomic.Uint64
	lastActivityNanos     atomic.Int64
}

// acquireAttach marks the session busy and returns a context for this attachment.
// If another client is already attached, the prior attachment is cancelled so the
// new connection can take over (handles React Strict Mode remounts and tab refresh).
func (s *session) acquireAttach(parent context.Context) (context.Context, context.CancelFunc, func()) {
	s.attachMu.Lock()
	if s.attachCancel != nil {
		s.attachCancel()
		s.attachCancel = nil
	}
	ctx, cancel := context.WithCancel(parent)
	gen := s.attachGen.Add(1)
	s.attachCancel = cancel
	s.busy.Store(true)
	s.touch()
	s.attachMu.Unlock()

	release := func() {
		s.touch()
		s.attachMu.Lock()
		if s.attachGen.Load() == gen {
			s.attachCancel = nil
			s.busy.Store(false)
		}
		s.attachMu.Unlock()
		cancel()
	}
	return ctx, cancel, release
}

func (s *session) touch() {
	s.lastActivityNanos.Store(time.Now().UnixNano())
}

func (s *session) lastActivity() time.Time {
	return time.Unix(0, s.lastActivityNanos.Load())
}

func (s *session) shutdown(_ context.Context) error {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	if s.processHandle != 0 {
		_ = terminateProcessHandle(s.processHandle)
		if !s.waitOwnsProcessHandle {
			_ = closePlatformHandle(s.processHandle)
		}
		s.processHandle = 0
	}
	if s.pseudoConsole != 0 {
		closePseudoConsoleHandle(s.pseudoConsole)
		s.pseudoConsole = 0
	}
	if s.tty != nil {
		_ = s.tty.Close()
	}
	if s.input != nil {
		_ = s.input.Close()
	}
	if s.output != nil {
		_ = s.output.Close()
	}
	return nil
}
