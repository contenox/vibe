//go:build !windows

package terminalservice

import (
	"context"
	"errors"
	"fmt"
	"io"
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
					reportErr, _, end := s.tracker.Start(ctx, "stream_output", "terminal_pty", "sessionID", id, "backend", "pty")
					reportErr(fmt.Errorf("attach: pty->ws write error: %w", werr))
					end()
					return
				}
			}
			if err != nil {
				reportErr, _, end := s.tracker.Start(ctx, "stream_output", "terminal_pty", "sessionID", id, "backend", "pty")
				reportErr(fmt.Errorf("attach: pty read done: %w", err))
				end()
				return
			}
		}
	}()

	go func() {
		defer cancel()
		n, err := io.Copy(tty, conn)
		reportErr, _, end := s.tracker.Start(ctx, "stream_input", "terminal_pty", "sessionID", id, "backend", "pty", "bytes", n)
		if err != nil {
			reportErr(fmt.Errorf("attach: ws->pty copy done: %w", err))
		}
		end()
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
							reportErr, _, end := s.tracker.Start(ctx, "resize", "terminal_pty", "session", id, "backend", "pty", "cols", msg.Cols, "rows", msg.Rows)
							reportErr(fmt.Errorf("terminal pty resize: %w", err))
							end()
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
