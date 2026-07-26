//go:build windows

package terminalservice

import (
	"log/slog"

	"golang.org/x/sys/windows"
)

func (s *service) resizeLocalPTY(id string, cols, rows int) {
	sess := s.localByID(id)
	if sess == nil || sess.pseudoConsole == 0 {
		return
	}
	size := windows.Coord{X: int16(cols), Y: int16(rows)}
	if err := windows.ResizePseudoConsole(windows.Handle(sess.pseudoConsole), size); err != nil {
		slog.Debug("terminal conpty resize", "session", id, "error", err)
	}
}
