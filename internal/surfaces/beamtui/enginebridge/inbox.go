package enginebridge

import (
	"context"
	"encoding/json"
	"fmt"
)

// inboxAddedSubject is the bus subject the operator inbox announces a stored
// item on. It follows this codebase's "<package>.events[.<verb>]" convention
// (missionservice.ReportAddedSubject = "missionservice.events.report_added"),
// and the literal is DUPLICATED here for the same reason the mission `_meta`
// keys are: the wire is the contract, this package is a client of it, and a
// round-trip test over a real bus is what pins the two together.
//
// The payload is the operator inbox's own Item as JSON
// (internal/services/operatorinbox/operatorinbox.go, type Item).
const inboxAddedSubject = "operatorinbox.events.added"

// inboxQueueDepth buffers the bus->bridge hand-off. It is a smoothing buffer,
// not a bound on what the Bridge will deliver: the consumer goroutine moves
// every payload straight onto the Bridge's own unbounded FIFO, so this only
// keeps the bus's poll loop from parking on the hand-off between two of them.
const inboxQueueDepth = 16

// inboxItem mirrors operatorinbox.Item — the durable row the inbox stores and
// publishes — down to the fields a one-line notice needs. It is a MIRROR, not
// an import: enginebridge decodes the wire shape rather than binding to the
// service's Go type, exactly as it does for the report router's `_meta`
// envelopes, so a field added there cannot break decoding here.
//
// The embedded report is the canonical missionservice.Report; only its kind and
// summary are lifted, because that is what "something came back and nobody was
// watching" needs to say.
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

// startInboxWatch subscribes to the operator inbox and pumps its items onto the
// Bridge's event stream as InboxItemAdded.
//
// A nil Deps.Bus is NOT an error: it means this process wired no bus, so there
// are no inbox events to relay and the Bridge simply never emits one. Everything
// else about the Bridge is unaffected — the inbox is an extra source, not a
// dependency of the ACP loopback.
//
// A subscribe that FAILS is a different thing entirely and fails New: the caller
// handed over a bus, which is a statement that this process has one, and a
// Bridge that silently relayed nothing would leave every unsupervised mission
// report invisible with no way to tell that from an empty inbox.
//
// The goroutine is registered on the Bridge's WaitGroup like the run loops, so
// Close joins it rather than hoping; it ends on the run context (the
// cancellation path) or on done (the Close path), and never on the channel,
// which the bus does not close.
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

// stopInboxWatch ends the subscription. It runs FIRST in Close, ahead of
// stopQueue, and that order is the whole point: the SQLite bus hands over
// everything published before Unsubscribe as a bounded final drain, and a drain
// into a consumer that has already been torn down is a second of Close spent
// delivering events to nobody. Unsubscribing while the consumer is still live
// makes those last items real — an inbox notice that landed as the operator quit
// is exactly the one they most need to have seen.
//
// It is nil-safe (no bus, or New failed before subscribing) and idempotent, so
// Close's sync.Once is not the only thing keeping it honest.
func (b *Bridge) stopInboxWatch() {
	if b.inboxSub == nil {
		return
	}
	_ = b.inboxSub.Unsubscribe()
}

// decodeInboxItem projects one published inbox item onto the event. A payload
// that will not decode is DROPPED rather than surfaced as an empty notice: unlike
// a session/update, there is no raw-passthrough event for a bus message and no
// consumer that could do anything with one — an InboxItemAdded with no id and no
// summary would ring a bell and put an empty row on the status bar, which is a
// worse answer than nothing. The same reasoning rejects an item with no id at
// all: a notice that cannot name the row it stands for is not a notice.
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
