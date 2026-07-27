//go:build windows

package terminalservice

import (
	"context"
	"fmt"

	"golang.org/x/sys/windows"
)

func (s *service) resizeLocalPTY(ctx context.Context, id string, cols, rows int) {
	sess := s.localByID(id)
	if sess == nil || sess.pseudoConsole == 0 {
		return
	}
	size := windows.Coord{X: int16(cols), Y: int16(rows)}
	if err := windows.ResizePseudoConsole(windows.Handle(sess.pseudoConsole), size); err != nil {
		reportErr, _, end := s.tracker.Start(ctx, "resize", "terminal_pty", "session", id, "backend", "conpty", "cols", cols, "rows", rows)
		reportErr(fmt.Errorf("terminal conpty resize: %w", err))
		end()
	}
}
