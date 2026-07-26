//go:build windows

package terminalservice

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"time"
	"unsafe"

	"github.com/contenox/beam/internal/services/terminalstore"
	"golang.org/x/sys/windows"
)

var peekNamedPipe = windows.NewLazySystemDLL("kernel32.dll").NewProc("PeekNamedPipe")

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

	input := sess.input
	output := sess.output
	if input == nil || output == nil {
		return ErrSessionNotFound
	}

	ctx, cancel, release := sess.acquireAttach(ctx)
	defer release()

	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		defer cancel()
		buf := make([]byte, 32*1024)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			ready, err := windowsPipeHasData(output)
			if err != nil {
				slog.Debug("attach: conpty output readiness done", "error", err)
				return
			}
			if !ready {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			n, err := output.Read(buf)
			if n > 0 {
				if _, werr := conn.Write(buf[:n]); werr != nil {
					slog.Debug("attach: conpty->ws write error", "error", werr)
					return
				}
			}
			if err != nil {
				slog.Debug("attach: conpty read done", "error", err)
				return
			}
		}
	}()

	go func() {
		defer cancel()
		n, err := io.Copy(input, conn)
		slog.Debug("attach: ws->conpty copy done", "bytes", n, "error", err)
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
						s.resizeLocalPTY(id, msg.Cols, msg.Rows)
					}
				}
			}
		}()
	}

	<-ctx.Done()
	// This attachment is over (client gone, preempted, or shutdown). Close the
	// transport first: a conpty->ws writer stalled on a dead or slow client can
	// only be unblocked by closing conn.
	_ = conn.Close()
	<-outputDone
	return nil
}

func windowsPipeHasData(file *os.File) (bool, error) {
	var available uint32
	r1, _, err := peekNamedPipe.Call(
		file.Fd(),
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&available)),
		0,
	)
	if r1 != 0 {
		return available > 0, nil
	}
	if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		return false, err
	}
	return false, err
}
