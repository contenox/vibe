package libacp

import "context"

// FilterSessionUpdates wraps a Client so session/update notifications for any
// session other than live are dropped before reaching inner.SessionUpdate;
// every other method passes through. ClientSideConnection forwards updates
// regardless of session id (session bookkeeping is the app's job), so a
// driver that reconnects or swaps sessions needs this guard or a
// just-abandoned session's chunks leak into the new turn's UI.
//
// Wrap the Client the ClientFactory returns; construct a new wrapper with the
// updated live id whenever the driver's active session changes.
func FilterSessionUpdates(live SessionID, inner Client) Client {
	return sessionUpdateFilter{Client: inner, live: live}
}

// sessionUpdateFilter embeds Client so all non-overridden methods (permission,
// fs, terminal) forward unchanged; only SessionUpdate gains the session-id gate.
type sessionUpdateFilter struct {
	Client
	live SessionID
}

func (f sessionUpdateFilter) SessionUpdate(ctx context.Context, n SessionNotification) error {
	if n.SessionID != f.live {
		return nil
	}
	return f.Client.SessionUpdate(ctx, n)
}
