//go:build !windows

package terminalservice

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"syscall"

	"github.com/coder/websocket"
	apiframework "github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/terminalstore"
	"github.com/creack/pty"
	"golang.org/x/sync/errgroup"
)

func (s *service) Attach(ctx context.Context, principal, id string, conn *websocket.Conn) error {
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
	defer func() { _ = s.Close(ctx, principal, id) }()

	tty := sess.tty
	if tty == nil {
		return ErrSessionNotFound
	}

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		// Closing the PTY master unblocks the reader goroutine.
		defer func() {
			if sess.tty != nil {
				_ = sess.tty.Close()
			}
		}()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				closeSt := websocket.CloseStatus(err)
				if closeSt == websocket.StatusNormalClosure || closeSt == -1 {
					return nil
				}
				return err
			}
			switch typ {
			case websocket.MessageText:
				var msg struct {
					Type string `json:"type"`
					Cols int    `json:"cols"`
					Rows int    `json:"rows"`
				}
				if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" && msg.Cols > 0 && msg.Rows > 0 {
					if err := pty.Setsize(tty, &pty.Winsize{
						Rows: uint16(msg.Rows),
						Cols: uint16(msg.Cols),
					}); err != nil {
						slog.Debug("terminal pty resize", "error", err)
					}
				}
			case websocket.MessageBinary:
				if _, err := tty.Write(data); err != nil {
					return err
				}
			}
		}
	})

	g.Go(func() error {
		buf := make([]byte, 32*1024)
		for {
			n, err := tty.Read(buf)
			if n > 0 {
				werr := conn.Write(ctx, websocket.MessageBinary, buf[:n])
				if werr != nil {
					return werr
				}
			}
			if err != nil {
				if err == io.EOF || errors.Is(err, syscall.EIO) {
					return nil
				}
				return err
			}
		}
	})

	err = g.Wait()
	if err != nil && errors.Is(err, context.Canceled) {
		err = nil
	}
	return err
}
