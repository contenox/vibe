//go:build !windows

package terminalservice

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	apiframework "github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/terminalstore"
	"github.com/creack/pty"
	"github.com/google/uuid"
)

func (s *service) Create(ctx context.Context, principal string, req CreateRequest) (*CreateResponse, error) {
	if req.CWD == "" {
		req.CWD = s.cfg.AllowedRoot
	}
	if err := CwdUnderRoot(s.cfg.AllowedRoot, req.CWD); err != nil {
		return nil, apiframework.BadRequest(err.Error())
	}
	shell := req.Shell
	if shell == "" {
		shell = s.cfg.DefaultShell
	}
	if err := ValidateShell(shell); err != nil {
		return nil, apiframework.BadRequest(err.Error())
	}
	cols, rows := req.Cols, req.Rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	s.mu.Lock()
	if len(s.sessions) >= s.cfg.MaxSessions {
		s.mu.Unlock()
		return nil, ErrTooManySessions
	}
	s.mu.Unlock()

	var cmd *exec.Cmd
	switch shell {
	case "/bin/bash", "/usr/bin/bash":
		cmd = exec.CommandContext(ctx, shell, "-i")
	case "/bin/zsh", "/usr/bin/zsh":
		cmd = exec.CommandContext(ctx, shell, "-i")
	default:
		cmd = exec.CommandContext(ctx, shell)
	}
	cmd.Dir = req.CWD
	cmd.Env = append(execEnv(), "TERM=xterm-256color")

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
		CWD:            req.CWD,
		Shell:          shell,
		Cols:           cols,
		Rows:           rows,
		Status:         terminalstore.SessionStatusActive,
		NodeInstanceID: s.nodeInstanceID,
		WorkspaceID:    "",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.store().Insert(ctx, sessRow); err != nil {
		_ = tty.Close()
		_ = cmd.Process.Kill()
		return nil, err
	}

	sess := &session{id: id, tty: tty, cmd: cmd}
	s.mu.Lock()
	if len(s.sessions) >= s.cfg.MaxSessions {
		s.mu.Unlock()
		_ = sess.shutdown(ctx)
		_ = s.store().Delete(ctx, id)
		return nil, ErrTooManySessions
	}
	s.sessions[id] = sess
	s.mu.Unlock()

	return &CreateResponse{ID: id}, nil
}

func execEnv() []string { return os.Environ() }
