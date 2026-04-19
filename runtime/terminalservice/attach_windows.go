//go:build windows

package terminalservice

import (
	"context"
	"io"
)

func (s *service) Attach(_ context.Context, _, _ string, _ io.ReadWriteCloser, _ <-chan ResizeMsg) error {
	return ErrNotImplemented
}
