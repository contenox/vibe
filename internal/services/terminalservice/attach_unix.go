//go:build !windows

package terminalservice

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/contenox/beam/internal/services/terminalstore"
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

	sess := s.localByID(id)
	if sess == nil {
		_ = ts.Delete(ctx, id)
		return ErrSessionNotFound
	}
	if s.localByID(id) != sess {
		return ErrSessionNotFound
	}

	tty := sess.tty
	if tty == nil {
		return ErrSessionNotFound
	}

	ctx, cancel, release := sess.acquireAttach(ctx)
	defer release()

	ptyDone := make(chan struct{})
	go func() {
		defer close(ptyDone)
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

	go func() {
		defer cancel()
		n, err := io.Copy(tty, conn)
		slog.Debug("attach: ws->pty copy done", "bytes", n, "error", err)
	}()

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
						if err := pty.Setsize(tty, &pty.Winsize{Rows: uint16(msg.Rows), Cols: uint16(msg.Cols)}); err != nil {
							slog.Debug("terminal pty resize", "error", err)
						}
					}
				}
			}
		}()
	}

	<-ctx.Done()
	// This attachment is over (client gone, preempted, or shutdown). Close the
	// transport first: a pty->ws writer stalled on a dead or slow client never
	// sees the tty read deadline, so only closing conn can unblock it.
	_ = conn.Close()
	_ = tty.SetReadDeadline(time.Unix(1, 0))
	<-ptyDone
	_ = tty.SetReadDeadline(time.Time{})
	return nil
}
