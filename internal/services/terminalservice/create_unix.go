//go:build !windows

package terminalservice

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/contenox/beam/internal/errdefs"
	"github.com/contenox/beam/internal/services/terminalstore"
	"github.com/creack/pty"
	"github.com/google/uuid"
)

func (s *service) Create(ctx context.Context, principal string, req CreateRequest) (*CreateResponse, error) {
	if req.CWD == "" {
		req.CWD = s.cfg.AllowedRoot
	}
	cwd, err := ResolveCwdUnderRoot(s.cfg.AllowedRoot, req.CWD)
	if err != nil {
		return nil, errdefs.BadRequest(err.Error())
	}
	shell := req.Shell
	if shell == "" {
		shell = s.cfg.DefaultShell
	}
	resolvedShell, err := resolveShell(shell)
	if err != nil {
		return nil, errdefs.BadRequest(err.Error())
	}
	shell = resolvedShell
	cols, rows := req.Cols, req.Rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	if s.atSessionCapacity() {
		return nil, ErrTooManySessions
	}

	var cmd *exec.Cmd
	switch shell {
	case "/bin/bash", "/usr/bin/bash", "/bin/zsh", "/usr/bin/zsh":
		cmd = exec.Command(shell, "-i")
	default:
		cmd = exec.Command(shell)
	}
	cmd.Dir = cwd
	// Scrub serve's credentials out of the terminal's environment when configured
	// (default off — the terminal is the operator's own shell); TERM is appended
	// last so it wins regardless of the policy.
	parent := os.Environ()
	if s.cfg.ScrubEnv != nil {
		parent = s.cfg.ScrubEnv(parent)
	}
	cmd.Env = append(parent, "TERM=xterm-256color")

	tty, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("terminalservice: pty start: %w", err)
	}
	if err := pty.Setsize(tty, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}); err != nil {
		_ = tty.Close()
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("terminalservice: pty resize: %w", err)
	}

	id := uuid.NewString()
	now := time.Now().UTC()
	sessRow := &terminalstore.Session{
		ID:             id,
		Principal:      principal,
		CWD:            cwd,
		Shell:          shell,
		Cols:           cols,
		Rows:           rows,
		Status:         terminalstore.SessionStatusActive,
		NodeInstanceID: s.nodeInstanceID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.store().Insert(ctx, sessRow); err != nil {
		_ = tty.Close()
		_ = cmd.Process.Kill()
		return nil, err
	}

	sess := &session{id: id, tty: tty, cmd: cmd}
	sess.touch()
	s.putSession(sess)

	go func() {
		_ = cmd.Wait()
		_ = s.closeByID(context.Background(), id)
	}()

	return &CreateResponse{ID: id}, nil
}
