package contenoxcli

import (
	"context"

	"github.com/contenox/contenox/internal/services/sessionservice"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
)

const localIdentity = "local-user"

// ensureDefaultSession creates and activates the "default" session if no active
// session exists, and returns the session ID for this invocation.
func ensureDefaultSession(ctx context.Context, db libdb.DBManager, workspaceID string) (string, error) {
	return sessionservice.New(db, workspaceID, libtracker.NoopTracker{}).EnsureDefault(ctx, localIdentity)
}
