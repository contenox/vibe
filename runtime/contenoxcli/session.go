// session.go — session-related constants and helpers for the contenoxcli package.
package contenoxcli

import (
	"context"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtime/sessionservice"
)

const localIdentity = "local-user"

// ensureDefaultSession creates the "default" session if no active session exists,
// sets it as active, and returns the session ID to use for this invocation.
func ensureDefaultSession(ctx context.Context, db libdb.DBManager) (string, error) {
	return sessionservice.New(db).EnsureDefault(ctx, localIdentity)
}
