// Package relayacp carries ACP over a relay connection, so a remote client is
// just another ACP client of this runtime: the same agent factory serves it,
// over the same protocol, with nothing about it visible to the sessions it
// drives.
//
// # Why it is neither of the packages it joins
//
// internal/relaylink owns the connection and must never link libacp — it is a
// transport, and a transport that parses its cargo is a transport a payload can
// break. libacp takes an arbitrary io.ReadWriteCloser and must never learn what
// a relay is; that is exactly why it takes one. This package is the adapter
// between the two and exists so that neither has to know the other, which is
// also why it is not a file in either of them.
//
// # Attachments
//
// One remote client is one attachment. Several may be attached to the same
// instance at once — two tabs, a phone and a laptop — so an answer must reach
// the client that asked for it. A relay cannot tell them apart by reading the
// traffic, because parsing ACP is precisely what it must not do, so it assigns
// each attachment a fresh identifier and carries it in
// [librelay.Frame.Session].
//
// INVARIANT, and it is a two-end contract: every frame this package emits for
// an attachment echoes that Session unchanged. A frame that arrives back at the
// relay with an empty or altered Session names no attachment, so it is dropped
// there — a client would then wait forever on an answer that was produced and
// discarded.
//
// Each attachment runs its own [libacp.AgentSideConnection], so two attachments
// are two independent ACP sessions sharing nothing but the factory that built
// them.
//
// # One frame, one message
//
// A frame payload is exactly one ACP JSON-RPC message, and one message is
// exactly one frame. Nothing is batched, nothing is split and no length prefix
// is added; [stream] holds the newline bookkeeping that makes it true in both
// directions.
//
// # Only Type, Instance, Session and the payload
//
// The tunnel reads and writes no other envelope field, and that is a
// compatibility requirement rather than taste. A relay forwards a frame by
// decoding and re-encoding it, so a field its own librelay build does not know
// is not ignored — it is destroyed in transit, silently, with nothing on either
// side able to observe that it went missing.
//
// Resumption ([librelay.Frame.Seq], [librelay.TypeResume],
// [librelay.TypeResumed]) is therefore NOT a prerequisite here and is not used:
// those fields postdate the librelay a deployed relay may be built against, and
// a tunnel that needed them would fail as a hang rather than as an error.
// Resumption added later must be strictly additive, and this tunnel must keep
// working against a relay that erases every trace of it.
//
// # Backpressure, and the leak it is really about
//
// [Tunnel.Handle] runs on the connector's read loop, which must never block: a
// wait there stops the connector answering heartbeats, which the relay reads as
// a dead instance. So every queue here is bounded and a full one drops its
// attachment instead of waiting, the same policy the relay applies to a wedged
// client. Growing the queue would only convert the problem into memory a
// remote peer chooses, and dropping the oldest message would hand a client a
// JSON-RPC stream with a hole in it — worse than no stream, because the client
// waits forever on a response that was silently discarded.
//
// # Endings
//
// [librelay.TypeACPDetach] is the ordinary ending: it names one attachment and
// tears it down on the spot, so a client that closed its socket stops costing
// an agent and a goroutine as soon as one frame arrives.
//
// It is a hint and never a prerequisite. A relay whose librelay predates that
// constant emits none, and a client that lost its network sends nothing at all,
// so the cap remains the backstop: reaching it evicts the least recently
// addressed attachment rather than refusing the new one. That reclaims
// abandoned attachments exactly when reclamation is needed and never on a
// clock, which is why this package owns no timer at all.
//
// Both are needed. Eviction alone is correct but blunt — a live attachment that
// has merely been quiet is the least recently addressed one, so new arrivals
// can take it — and a detach alone would be a bound a peer could decline to
// honour by never sending one.
package relayacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/librelay"
	"github.com/contenox/contenox/libtracker"
)

// Bounds [Config] leaves zero. Both are ceilings on a remote peer's appetite:
// the relay is configuration, not a trusted caller, and an unbounded one of
// these is a way for it to consume this process.
const (
	// DefaultQueue is how many inbound messages may await one attachment's
	// ACP connection before it is judged wedged. See [Tunnel.Handle].
	DefaultQueue = 64
	// DefaultMaxAttachments bounds the attachments one tunnel serves at once.
	// Two tabs and a phone is three; this ceiling is sized for a peer that
	// mints identifiers, not for a user who opens windows.
	DefaultMaxAttachments = 64
)

// SendFunc puts one frame on the relay. It is the connector's outbound edge
// narrowed to the single call a tunnel makes, so this package dials nothing,
// holds no connection, and is testable without either; *relaylink.Connector's
// Send satisfies it.
//
// It must not block. The connector queues and fails fast by design, and a
// SendFunc that waited would put relay backpressure on an ACP handler.
type SendFunc func(librelay.Frame) error

// Config is everything a tunnel needs. It names no endpoint and no credential:
// establishing the connection belongs to the connector, and a tunnel that could
// see either would be a second place for them to be logged.
type Config struct {
	// Instance is this runtime's identity at the relay, stamped on every
	// outbound frame. Required: [librelay.Frame.Validate] rejects a Session
	// without an Instance, so a tunnel without one could emit nothing.
	Instance string
	// Send is the outbound edge. Required.
	Send SendFunc
	// Factory builds the agent serving one attachment. Required. It is
	// invoked once per attachment and must be safe for concurrent use, since
	// attachments are concurrent by construction.
	Factory libacp.AgentFactory
	// Queue is the per-attachment inbound depth; zero means [DefaultQueue].
	Queue int
	// MaxAttachments caps concurrent attachments; zero means
	// [DefaultMaxAttachments].
	MaxAttachments int
	// Tracker instruments each attachment's lifetime. Nil degrades to
	// [libtracker.NoopTracker]. It is the only instrumentation seam here, and
	// deliberately so: a tracker redacts by field name before writing, and no
	// path in this package may report a payload — the traffic is the user's
	// conversation.
	Tracker libtracker.ActivityTracker
}

// Tunnel routes relay cargo to per-attachment ACP connections. The zero value
// is unusable; call [New]. It is safe for concurrent use.
type Tunnel struct {
	cfg     Config
	tracker libtracker.ActivityTracker

	// ctx is cancelled by Close, ending every attachment's ACP handlers
	// rather than only closing their streams. It is rooted at Background
	// because Close is the tunnel's single lifecycle end; a second way to die
	// is a second teardown ordering to get wrong.
	ctx    context.Context
	cancel context.CancelFunc

	// clock orders attachments by last addressed, for eviction. A counter
	// rather than a timestamp: eviction must not depend on a wall clock that
	// a test would have to fake or a suspended laptop can move backwards.
	clock atomic.Uint64

	wg sync.WaitGroup

	mu     sync.Mutex
	live   map[string]*attachment
	closed bool
}

// New validates cfg and returns a tunnel that has attached nothing. It performs
// no I/O and starts no goroutine, so a runtime that never receives a frame pays
// nothing for having built one.
//
// Instance is checked against the envelope's own validity rule rather than a
// local one, so an unroutable identity is a configuration error the caller sees
// here instead of one that surfaces later as every outbound frame failing to
// validate inside a goroutine nobody is watching.
func New(cfg Config) (*Tunnel, error) {
	if cfg.Instance == "" {
		return nil, errors.New("relayacp: Instance is required")
	}
	if cfg.Send == nil {
		return nil, errors.New("relayacp: Send is required")
	}
	if cfg.Factory == nil {
		return nil, errors.New("relayacp: Factory is required")
	}
	probe := librelay.Frame{Type: librelay.TypeACPMessage, Instance: cfg.Instance, Session: "probe"}
	if err := probe.Validate(); err != nil {
		return nil, fmt.Errorf("relayacp: Instance: %w", err)
	}
	if cfg.Queue <= 0 {
		cfg.Queue = DefaultQueue
	}
	if cfg.MaxAttachments <= 0 {
		cfg.MaxAttachments = DefaultMaxAttachments
	}
	if cfg.Tracker == nil {
		cfg.Tracker = libtracker.NoopTracker{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Tunnel{
		cfg:     cfg,
		tracker: cfg.Tracker,
		ctx:     ctx,
		cancel:  cancel,
		live:    map[string]*attachment{},
	}, nil
}

// Handle takes one routed frame off the connector and applies it to the
// attachment named by [librelay.Frame.Session]. It is the [relaylink.Handler]
// this package registers.
//
// It never blocks and never fails: it runs on the connector's read loop, so
// everything it cannot place is dropped. Dropping is right in every case it
// covers — a closed tunnel, an attachment whose queue is full and which is
// therefore closed on the spot, and two filters worth stating:
//
// Only [librelay.TypeACPMessage] and [librelay.TypeACPDetach] cross this seam.
// Resumption and any future cargo type are dropped rather than misread as a
// message, which is what keeps the tunnel working against a relay whose
// librelay predates them — and is equally what lets an older relay ignore the
// detach type it does not know.
//
// A frame naming another instance cannot have been meant for this runtime. An
// empty Instance is accepted, because the routing key is already implied by the
// connection the frame arrived on.
//
// A message creates the attachment it names on first sight; a detach never
// does. Attaching in order to detach would let a peer mint an attachment per
// stale identifier it still holds, which is the appetite the cap exists to
// bound.
func (t *Tunnel) Handle(f librelay.Frame) {
	if f.Session == "" {
		return
	}
	if f.Instance != "" && f.Instance != t.cfg.Instance {
		return
	}
	switch f.Type {
	case librelay.TypeACPMessage:
		if len(f.Payload) == 0 {
			return
		}
		if a := t.attachmentFor(f.Session); a != nil {
			a.deliver(f.Payload)
		}
	case librelay.TypeACPDetach:
		t.detachSession(f.Session)
	}
}

// Len reports how many attachments are live right now. Diagnostics only: it
// describes this process's attachments and says nothing about what the relay
// believes.
func (t *Tunnel) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.live)
}

// Close drops every attachment and returns once each one's ACP connection and
// goroutine has exited. It is idempotent and safe on a tunnel that never
// attached anything.
//
// Waiting is the point: an attachment holds an agent, and returning before
// those have unwound would let a shutdown race the teardown of whatever the
// agent spawned.
//
// Attachments are closed after the map's lock is released, because closing
// takes each attachment's own path and one that deregisters itself while the
// lock was held would deadlock against it.
func (t *Tunnel) Close() {
	t.mu.Lock()
	first := !t.closed
	t.closed = true
	live := make([]*attachment, 0, len(t.live))
	for _, a := range t.live {
		live = append(live, a)
	}
	t.mu.Unlock()
	if first {
		t.cancel()
	}
	for _, a := range live {
		a.close()
	}
	t.wg.Wait()
}

// detachSession tears down the attachment named by session, if one is live. It
// is idempotent, and a session naming no live attachment is not an error: a
// detach for an attachment already evicted, already ended by its own client, or
// never created here is exactly the state the frame is asking for.
//
// The attachment is deregistered before it is closed, so [Tunnel.run]'s own
// deregistration finds it already gone and its identity check keeps it from
// removing whatever took the session's place afterwards. Closing under the lock
// is safe for the same reason eviction may: closing an attachment only closes a
// channel, so it takes no lock and cannot block the connector's read loop.
func (t *Tunnel) detachSession(session string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.live[session]
	if a == nil {
		return
	}
	delete(t.live, session)
	a.close()
}

// attachmentFor returns the attachment for session, creating one on first
// sight, or nil when the tunnel is closed. It is the only place attachments are
// created, so it is also the only place the cap is enforced.
//
// At the cap an attachment is evicted rather than the new one refused. A client
// that lost its network sends no [librelay.TypeACPDetach], and a relay pinned to
// an older librelay emits none at all, so an abandoned attachment can still be
// indistinguishable from a silent one; refusing would let dead attachments
// accumulate until no live client could attach, which is the failure the cap
// exists to prevent rather than to cause.
//
// Eviction closes the loser inline and under the lock, which is safe because
// closing an attachment only closes a channel: it takes no lock of its own and
// cannot block the connector's read loop.
func (t *Tunnel) attachmentFor(session string) *attachment {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	if a := t.live[session]; a != nil {
		a.touch(t.clock.Add(1))
		return a
	}
	for len(t.live) >= t.cfg.MaxAttachments {
		oldest := t.oldestLocked()
		if oldest == nil {
			return nil
		}
		delete(t.live, oldest.session)
		oldest.close()
	}
	a := newAttachment(session, t.cfg.Instance, t.cfg.Queue, t.cfg.Send)
	a.touch(t.clock.Add(1))
	t.live[session] = a
	t.wg.Add(1)
	go t.run(a)
	return a
}

// oldestLocked returns the least recently addressed attachment. Least recently
// addressed, not least recently active: an attachment nobody has spoken to for
// longest is the best available evidence of a client that has gone away, and
// outbound traffic is a consequence of inbound traffic rather than independent
// of it.
func (t *Tunnel) oldestLocked() *attachment {
	var oldest *attachment
	for _, a := range t.live {
		if oldest == nil || a.seen() < oldest.seen() {
			oldest = a
		}
	}
	return oldest
}

// run serves one attachment's ACP connection until it ends, then deregisters
// it. The connection is built here rather than in [Tunnel.attachmentFor]
// because the factory is caller-supplied and must not run on the connector's
// read loop; the first payload is already queued on the stream by then, so
// nothing is lost by the handover.
//
// A client that hung up and a tunnel that is shutting down are the ordinary
// endings and are not reported: recording them would bury the failures worth
// reading.
func (t *Tunnel) run(a *attachment) {
	defer t.wg.Done()
	defer t.detach(a)
	reportErr, _, end := t.tracker.Start(t.ctx, "hold", "relay_attachment", "session", a.session)
	defer end()

	conn := libacp.NewAgentSideConnection(a.stream, t.cfg.Factory)
	err := conn.Run(t.ctx)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, io.ErrClosedPipe) {
		reportErr(err)
	}
	a.close()
}

// detach removes a only if a is still the registered attachment. The identity
// check is what keeps an eviction that already replaced this session's entry
// from deregistering the live attachment that took its place.
func (t *Tunnel) detach(a *attachment) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.live[a.session] == a {
		delete(t.live, a.session)
	}
}

// attachment is one remote ACP client: a bounded inbound queue, the stream that
// drains it, and the single goroutine running the ACP connection on top.
//
// Exactly one goroutine belongs to an attachment ([Tunnel.run]), and it exits
// when the stream closes. [Tunnel.Close] joins them all, so a client that drops
// — which a phone does constantly — leaks neither goroutine nor timer.
type attachment struct {
	session string
	stream  *stream
	// lastSeen is the tunnel clock reading when this attachment was last
	// addressed, for eviction ordering. Atomic because it is written under
	// the tunnel's lock and read from the same place, and making it atomic
	// keeps that true if a future reader arrives from elsewhere.
	lastSeen atomic.Uint64
}

func newAttachment(session, instance string, queue int, send SendFunc) *attachment {
	return &attachment{
		session: session,
		stream:  newStream(session, instance, queue, send),
	}
}

// touch records that this attachment was addressed at the given clock reading.
func (a *attachment) touch(at uint64) { a.lastSeen.Store(at) }

// seen returns the clock reading from the last [attachment.touch].
func (a *attachment) seen() uint64 { return a.lastSeen.Load() }

// close ends the attachment. It is idempotent, does no I/O and takes no lock.
func (a *attachment) close() { a.stream.Close() }

// deliver queues one inbound ACP message. It never blocks, and a full queue
// closes the attachment rather than waiting on it — see the package doc for why
// that is the only defensible policy here.
func (a *attachment) deliver(payload json.RawMessage) {
	if !a.stream.offer(payload) {
		a.close()
	}
}
