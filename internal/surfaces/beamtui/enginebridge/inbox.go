package enginebridge

import (
	"context"
	"encoding/json"
	"fmt"
)

// inboxAddedSubject is the bus subject the operator inbox announces a stored
// item on (payload is operatorinbox.Item as JSON). Duplicated here rather
// than imported, like the mission `_meta` keys, since the wire is the
// contract and a round-trip test over a real bus pins the two together.
const inboxAddedSubject = "operatorinbox.events.added"

// inboxQueueDepth smooths the bus->bridge hand-off; it is not a bound on
// delivery, since the consumer moves every payload onto the Bridge's own
// unbounded FIFO.
const inboxQueueDepth = 16

// inboxItem mirrors operatorinbox.Item down to the fields a one-line notice
// needs — a decoded mirror, not an import, so a field added there cannot
// break decoding here. Kind and Summary are lifted from the embedded report.
type inboxItem struct {
	ID              string `json:"id"`
	MissionID       string `json:"missionId"`
	AgentName       string `json:"agentName,omitempty"`
	Intent          string `json:"intent,omitempty"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	Reason          string `json:"reason"`
	Report          struct {
		Kind    string `json:"kind"`
		Summary string `json:"summary"`
	} `json:"report"`
}

// startInboxWatch subscribes to the operator inbox and pumps its items onto
// the Bridge's event stream as InboxItemAdded. A nil Deps.Bus is not an
// error — there are simply no inbox events to relay. A subscribe that fails
// does fail New: a caller that handed over a bus expects inbox events, and
// silently relaying nothing would hide every unsupervised mission report.
//
// The goroutine is registered on the Bridge's WaitGroup so Close joins it;
// it ends on ctx or on done, never on the channel, which the bus never closes.
func (b *Bridge) startInboxWatch(ctx context.Context) error {
	if b.deps.Bus == nil {
		return nil
	}
	ch := make(chan []byte, inboxQueueDepth)
	sub, err := b.deps.Bus.Stream(ctx, inboxAddedSubject, ch)
	if err != nil {
		return fmt.Errorf("enginebridge: subscribe %q: %w", inboxAddedSubject, err)
	}
	b.inboxSub = sub

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			select {
			case raw, ok := <-ch:
				if !ok {
					return
				}
				if ev, ok := decodeInboxItem(raw); ok {
					b.emit(ev)
				}
			case <-ctx.Done():
				return
			case <-b.done:
				return
			}
		}
	}()
	return nil
}

// stopInboxWatch ends the subscription. It runs first in Close, ahead of
// stopQueue: the SQLite bus hands over everything published before
// Unsubscribe as a bounded final drain, which only reaches the operator if
// the consumer is still live. Nil-safe and idempotent.
func (b *Bridge) stopInboxWatch() {
	if b.inboxSub == nil {
		return
	}
	_ = b.inboxSub.Unsubscribe()
}

// decodeInboxItem projects one published inbox item onto the event. A
// payload that fails to decode, or an item with no id, is dropped rather
// than surfaced as an empty notice — there is no consumer that could act on one.
func decodeInboxItem(raw []byte) (InboxItemAdded, bool) {
	var it inboxItem
	if err := json.Unmarshal(raw, &it); err != nil {
		return InboxItemAdded{}, false
	}
	if it.ID == "" {
		return InboxItemAdded{}, false
	}
	return InboxItemAdded{
		ID:        it.ID,
		MissionID: it.MissionID,
		AgentName: it.AgentName,
		Intent:    it.Intent,
		Reason:    it.Reason,
		Kind:      it.Report.Kind,
		Summary:   it.Report.Summary,
	}, true
}
