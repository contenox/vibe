//go:build !windows

package terminalservice

import (
	"log/slog"

	"github.com/creack/pty"
)

func (s *service) resizeLocalPTY(id string, cols, rows int) {
	sess := s.localByID(id)
	if sess == nil || sess.tty == nil {
		return
	}
	if err := pty.Setsize(sess.tty, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}); err != nil {
		slog.Debug("terminal pty resize", "session", id, "error", err)
	}
}
