// Package agenthost is the runtime's client/host-role primitive for driving
// another ACP agent over stdio, the way an editor spawns an agent binary and
// drives it as a client. The harness (the libacp.Client callback surface) is
// always supplied by the caller and never assembled inside this package.
package agenthost

import (
	"context"
	"sync"

	"github.com/contenox/libacp"
)

// Agent drives another ACP agent: Connect establishes a live connection
// using the supplied harness and returns a Handle wired to it.
type Agent interface {
	// Connect spawns or attaches to the agent and returns a Handle wired to
	// harness. harness is always supplied by the caller; Connect never
	// builds one itself.
	Connect(ctx context.Context, harness libacp.Client) (*Handle, error)
}

// Handle is a live connection to an agent, returned by Agent.Connect. Close
// tears down the transport and waits for the read loop to exit, so a caller
// that has called Close knows the agent is fully torn down.
type Handle struct {
	// Conn is the live ACP client-side connection; callers issue calls
	// (Initialize, NewSession, Prompt) directly against it.
	Conn *libacp.ClientSideConnection

	closeFn   func() error
	closeOnce sync.Once
	closeErr  error
}

// Close tears down the transport and waits for Conn's read loop to return.
// Idempotent: repeated calls return the same result.
func (h *Handle) Close() error {
	h.closeOnce.Do(func() {
		h.closeErr = h.closeFn()
	})
	return h.closeErr
}
