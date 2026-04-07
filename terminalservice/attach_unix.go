//go:build !windows

package terminalservice

import (
	"context"
	"errors"
	"io"
	"log/slog"

	apiframework "github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/terminalstore"
	"github.com/creack/pty"
)

func (s *service) Attach(ctx context.Context, principal, id string, conn io.ReadWriteCloser, resizeCh <-chan ResizeMsg) error {
	ts := s.store()
	row, err := ts.GetByIDAndPrincipal(ctx, id, principal)
	if err != nil {
		if errors.Is(err, terminalstore.ErrNotFound) {
			return ErrSessionNotFound
		}
		return err
	}
	if row.Status != terminalstore.SessionStatusActive {
		return ErrSessionNotFound
	}

	sess := s.getSession(id)
	if sess == nil {
		_ = ts.Delete(ctx, id)
		return ErrSessionNotFound
	}
	if !sess.busy.CompareAndSwap(false, true) {
		return apiframework.BadRequest("session already has an active connection")
	}
	defer sess.busy.Store(false)
	defer func() { _ = s.Close(context.Background(), principal, id) }()

	tty := sess.tty
	if tty == nil {
		return ErrSessionNotFound
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// PTY → WebSocket (stdout)
	go func() {
		defer cancel()
		buf := make([]byte, 32*1024)
		for {
			n, err := tty.Read(buf)
			if n > 0 {
				if _, werr := conn.Write(buf[:n]); werr != nil {
					slog.Debug("attach: pty->ws write error", "error", werr)
					return
				}
			}
			if err != nil {
				slog.Debug("attach: pty read done", "error", err)
				return
			}
		}
	}()

	// WebSocket → PTY (stdin)
	go func() {
		defer cancel()
		n, err := io.Copy(tty, conn)
		slog.Debug("attach: ws->pty copy done", "bytes", n, "error", err)
	}()

	// Resize handler
	if resizeCh != nil {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-resizeCh:
					if !ok {
						return
					}
					if msg.Cols > 0 && msg.Rows > 0 {
						if err := pty.Setsize(tty, &pty.Winsize{
							Rows: uint16(msg.Rows),
							Cols: uint16(msg.Cols),
						}); err != nil {
							slog.Debug("terminal pty resize", "error", err)
						}
					}
				}
			}
		}()
	}

	// Block until session ends.
	<-ctx.Done()
	return nil
}
