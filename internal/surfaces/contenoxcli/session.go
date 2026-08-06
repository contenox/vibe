// session.go — session-related constants and helpers for the contenoxcli package.
package contenoxcli

import (
	"context"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/sessionservice"
	"github.com/contenox/contenox/libtracker"
)

const localIdentity = "local-user"

// ensureDefaultSession creates the "default" session if no active session exists,
// sets it as active, and returns the session ID to use for this invocation.
func ensureDefaultSession(ctx context.Context, db libdb.DBManager, workspaceID string) (string, error) {
	return sessionservice.New(db, workspaceID, libtracker.NoopTracker{}).EnsureDefault(ctx, localIdentity)
}
