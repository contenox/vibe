package libacp

import "context"

// FilterSessionUpdates wraps a Client so session/update notifications for any
// session other than live are dropped before reaching inner.SessionUpdate,
// every other method passing through unchanged; construct a new wrapper with
// the updated live id whenever the driver's active session changes.
func FilterSessionUpdates(live SessionID, inner Client) Client {
	return sessionUpdateFilter{Client: inner, live: live}
}

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
