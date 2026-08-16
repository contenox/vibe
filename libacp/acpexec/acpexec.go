// Package acpexec spawns a subprocess and wires its stdin/stdout together as a
// single io.ReadWriteCloser, the transport shape libacp's connections expect.
package acpexec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

const defaultKillGrace = 5 * time.Second

// Option configures Spawn. See WithStderr and WithKillGrace.
type Option func(*config)

type config struct {
	stderr    io.Writer
	killGrace time.Duration
}

// WithStderr forwards the subprocess's stderr to w as it's written, instead
// of the default (io.Discard); use a *LockedBuffer if the caller reads w's
// contents from a different goroutine than the one writing it.
func WithStderr(w io.Writer) Option {
	return func(c *config) { c.stderr = w }
}

// WithKillGrace overrides how long Close waits for the subprocess to exit on
// its own (after closing its stdin) before it escalates to Process.Kill; the
// default is 5 seconds.
func WithKillGrace(d time.Duration) Option {
	return func(c *config) { c.killGrace = d }
}

// Process is a spawned subprocess wired up as an io.ReadWriteCloser: Read pulls
// from its stdout, Write pushes to its stdin, Close begins shutdown.
type Process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	grace  time.Duration

	waitDone chan struct{}
	waitErr  error

	closeOnce sync.Once
	closeErr  error
}

var _ io.ReadWriteCloser = (*Process)(nil)

// Spawn starts cmd (claiming its Stdin/Stdout via
// exec.Cmd.StdinPipe/StdoutPipe, which callers must not set already) and
// returns it as a Process, tearing it down exactly as Close would if ctx is
// cancelled before the subprocess exits.
func Spawn(ctx context.Context, cmd *exec.Cmd, opts ...Option) (*Process, error) {
	cfg := config{stderr: io.Discard, killGrace: defaultKillGrace}
	for _, opt := range opts {
		opt(&cfg)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acpexec: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acpexec: stdout pipe: %w", err)
	}
	cmd.Stderr = cfg.stderr

	// Own process group (unix) so Close's kill escalation takes down forked
	// children too; a surviving grandchild would hold our pipes open.
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acpexec: start %s: %w", cmd.Path, err)
	}

	p := &Process{
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		grace:    cfg.killGrace,
		waitDone: make(chan struct{}),
	}

	// This goroutine owns the only call to cmd.Wait, which closes the pipes on
	// exit.
	go func() {
		p.waitErr = cmd.Wait()
		close(p.waitDone)
	}()

	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = p.Close()
			case <-p.waitDone:
			}
		}()
	}

	return p, nil
}

// Read reads from the subprocess's stdout, returning io.EOF like any closed
// pipe once the subprocess exits (on its own, or via Close/ctx cancellation).
func (p *Process) Read(b []byte) (int, error) { return p.stdout.Read(b) }

// Write writes to the subprocess's stdin.
func (p *Process) Write(b []byte) (int, error) { return p.stdin.Write(b) }

const killReapTimeout = 5 * time.Second

func (p *Process) Close() error {
	p.closeOnce.Do(func() {
		_ = p.stdin.Close()

		killed := false
		select {
		case <-p.waitDone:
		case <-time.After(p.grace):
			killed = true
			killProcessTree(p.cmd)
			select {
			case <-p.waitDone:
			case <-time.After(killReapTimeout):
				_ = p.stdout.Close()
				p.closeErr = fmt.Errorf("acpexec: subprocess %s not reaped %s after kill (a descendant outside its process group may be holding its pipes)", p.cmd.Path, killReapTimeout)
				return
			}
		}

		_ = p.stdout.Close()
		p.closeErr = p.waitErr

		// Only an exit status caused by this method's own kill is suppressed.
		if killed && exitFromKill(p.waitErr) {
			p.closeErr = nil
		}
	})
	return p.closeErr
}

// LockedBuffer is a concurrency-safe io.Writer around a bytes.Buffer, for use
// with WithStderr when a caller's String() read races the subprocess reader
// goroutine's writes.
type LockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *LockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns everything written so far.
func (b *LockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
