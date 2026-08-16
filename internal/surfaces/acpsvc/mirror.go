package acpsvc

import (
	"context"

	"github.com/contenox/contenox/libacp"
)

// mirrorQueueDepth is how many session updates may be waiting on one mirroring
// connection before the next is dropped.
const mirrorQueueDepth = 256

type mirrorItem struct {
	notif libacp.SessionNotification
}

// startMirrorPump creates the queue and the goroutine draining it; the pump ends
// with the connection.
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

// mirrorUpdate queues notif for this connection, rewritten to the session id this
// connection knows the session by. It never blocks: a full queue drops the
// update, and the client re-syncs by reattaching. notif must arrive already
// normalized by the originating transport — re-normalizing here would renumber
// tool-call cards the origin already sent under other ids.
func (t *Transport) mirrorUpdate(contenoxSessionID string, notif libacp.SessionNotification) {
	if t == nil {
		return
	}
	sid, ok := t.acpSessionForContenoxID(contenoxSessionID)
	if !ok {
		return
	}
	// An attached viewer is already this connection's delivery path.
	if t.isNativeViewing(sid) {
		return
	}
	notif.SessionID = sid
	t.mirrorOnce.Do(t.startMirrorPump)
	select {
	case t.mirrorCh <- mirrorItem{notif: notif}:
	default:
	}
}
