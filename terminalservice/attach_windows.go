//go:build windows

package terminalservice

import (
	"context"

	"github.com/coder/websocket"
)

func (s *service) Attach(_ context.Context, _, _ string, _ *websocket.Conn) error {
	return ErrNotImplemented
}
