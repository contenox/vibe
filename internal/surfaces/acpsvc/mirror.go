package acpsvc

import (
	"context"

	"github.com/contenox/contenox/libacp"
)

// mirrorQueueDepth is how many session updates may be waiting on one mirroring
// connection before the next is dropped.
//
// It is a backlog, not a buffer to be sized precisely: a screen keeping up
// never fills it, and a screen not keeping up is re-synced by reattaching
// rather than by a deeper queue. Deep enough to absorb a burst of tool-call
// chunks, shallow enough that a dead connection's backlog is bounded memory.
const mirrorQueueDepth = 256

// mirrorItem is one already-normalized notification bound for one connection.
type mirrorItem struct {
	notif libacp.SessionNotification
}

// startMirrorPump creates the queue and the goroutine draining it. Called
// through mirrorOnce, so mirrorCh is non-nil for every caller that follows.
//
// The pump ends with the connection: connCtx is cancelled on the Closed signal,
// which every caller reaches — serve and the relay tunnel never call
// [Transport.Close]. Whatever is still queued then is dropped, which is correct
// for a connection that is going away.
func (t *Transport) startMirrorPump() {
	t.mirrorCh = make(chan mirrorItem, mirrorQueueDepth)
	ctx := t.connCtx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case item := <-t.mirrorCh:
				t.writeUpdate(ctx, item.notif)
			}
		}
	}()
}

// mirrorUpdate queues notif for this connection, rewritten to the session id
// this connection knows the session by.
//
// It never blocks and never fails. A full queue drops the update: the transcript
// is durable and session/load replays it, so a surface that falls behind
// re-syncs by reattaching — whereas blocking here would stall the turn that is
// producing the updates, since ndjsonWriter.Write holds a mutex around a write
// straight to the socket.
//
// A transport that does not have this session open is skipped rather than being
// told about a session it never asked for.
//
// INVARIANT: notif arrives already normalized by the originating transport and
// is written verbatim. Tool-call wire ids are minted per connection from that
// transport's own counters (Transport.toolCallWireID), so re-normalizing here
// would renumber cards the origin has already sent under different ids.
func (t *Transport) mirrorUpdate(contenoxSessionID string, notif libacp.SessionNotification) {
	if t == nil {
		return
	}
	sid, ok := t.acpSessionForContenoxID(contenoxSessionID)
	if !ok {
		return
	}
	notif.SessionID = sid
	t.mirrorOnce.Do(t.startMirrorPump)
	select {
	case t.mirrorCh <- mirrorItem{notif: notif}:
	default:
	}
}
