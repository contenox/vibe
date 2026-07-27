//go:build !windows

package terminalservice

import (
	"context"
	"fmt"

	"github.com/creack/pty"
)

func (s *service) resizeLocalPTY(ctx context.Context, id string, cols, rows int) {
	sess := s.localByID(id)
	if sess == nil || sess.tty == nil {
		return
	}
	if err := pty.Setsize(sess.tty, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}); err != nil {
		reportErr, _, end := s.tracker.Start(ctx, "resize", "terminal_pty", "session", id, "backend", "pty", "cols", cols, "rows", rows)
		reportErr(fmt.Errorf("terminal pty resize: %w", err))
		end()
	}
}
