// Package relaylink is the runtime half of the relay connection: it dials a
// relay, completes the [librelay] handshake, holds the connection, proves the
// peer is alive with heartbeats, and redials with backoff when it is not.
//
// # The property this package exists to protect
//
// A relay that is unreachable must be invisible to the rest of the runtime.
// Nothing here blocks a caller on a connection that is not there: [New] does no
// I/O, [Connector.Start] returns before the first dial is attempted, and
// [Connector.Send] fails immediately with [ErrNotConnected] rather than waiting
// for a link to come back. Reconnection happens on one background goroutine and
// is not an error anybody is told about — a relay outage degrades the runtime,
// it never stops it. Every exported call on this package returns in bounded
// time regardless of the relay's state, and that is the invariant to preserve
// when changing anything in it.
//
// # Never a listening socket
//
// The connector opens outbound connections and nothing else. There is no
// address to bind, no accept loop and no API that could grow one; the endpoint
// is configuration and the only network primitive reachable from here is a
// dialer. TestUnit_ConnectorNeverListens enforces it against the package source
// so the property survives a future edit that did not read this comment.
//
// # Layering
//
// This package is deliberately not part of librelay. librelay is the wire
// contract both ends compile against; a connector is one end's implementation
// of it, and putting it in the shared module would make a relay link code it
// must never run.
package relaylink

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/contenox/contenox/librelay"
	"github.com/contenox/contenox/libtracker"
)

// MinProtocolVersion is the oldest envelope version this build will speak. A
// relay selects min(peer, self); anything below this floor — or above what this
// build can possibly speak — means the negotiation produced a version nobody
// can honour, and the connector refuses rather than holding a connection open
// that looks healthy and is not.
const MinProtocolVersion = 1

// Connector failures a caller can act on. Everything else is internal to the
// retry loop and never reaches an API surface.
var (
	// ErrNotConnected is returned by Send when no link is currently held.
	// It is the normal, expected answer while a relay is down and is not
	// worth logging as an error.
	ErrNotConnected = errors.New("relaylink: not connected to a relay")
	// ErrBacklogFull is returned by Send when the outbound queue is full,
	// which means the relay is accepting the connection but not draining
	// it. Failing fast is the point: blocking here would put relay
	// backpressure on a mission's call path.
	ErrBacklogFull = errors.New("relaylink: outbound queue is full")
	// ErrClosed is returned by Start or Send after Close.
	ErrClosed = errors.New("relaylink: connector is closed")
	// ErrVersionNegotiation is the handshake refusing a version outside
	// [MinProtocolVersion]..[librelay.ProtocolVersion]. It is fatal: a
	// relay that answered once with an unspeakable version will answer the
	// same way to a retry, so retrying is a hot loop with no outcome.
	ErrVersionNegotiation = errors.New("relaylink: relay selected an unsupported protocol version")
	// ErrUnauthorized is a relay refusing the credentials. Fatal for the
	// same reason: credentials do not repair themselves.
	ErrUnauthorized = errors.New("relaylink: relay refused the credentials")
	// ErrRelayIdentity is the peer failing to prove it is the relay this
	// instance paired with: no signature on welcome, or one that does not
	// verify against [Credentials.RelayPublicKey]. It sits beside
	// ErrVersionNegotiation and ErrUnauthorized in the fatal set for the
	// same reason both of those are there — a peer that cannot prove itself
	// on this dial will not start being able to on the next one, so
	// retrying is a hot loop against something that is either misconfigured
	// or hostile.
	ErrRelayIdentity = errors.New("relaylink: relay did not prove its identity")
	// ErrHeartbeatTimeout ends a connection whose peer stopped answering
	// probes. It is the only way to notice a relay that died without
	// closing its TCP connection.
	ErrHeartbeatTimeout = errors.New("relaylink: peer did not answer a heartbeat")
)

// State is where the connector is in its lifecycle. It exists for diagnosis
// only; no runtime behavior may branch on it, because "the relay is down" must
// never be a case anything else has to handle.
type State int

// Connector states.
const (
	// StateIdle is before Start and after a Close that never connected.
	StateIdle State = iota
	// StateConnecting is dialing, handshaking, or waiting out a backoff.
	// A relay that is simply unreachable sits here forever, quietly.
	StateConnecting
	// StateConnected means a handshake completed and the link is held.
	StateConnected
	// StateFatal means the retry loop gave up: negotiation or credentials
	// failed, and retrying cannot change the answer. The runtime is
	// unaffected; only relay features are.
	StateFatal
	// StateClosed is after Close.
	StateClosed
)

// String renders s for logs.
func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateFatal:
		return "fatal"
	case StateClosed:
		return "closed"
	}
	return fmt.Sprintf("state(%d)", int(s))
}

// Status is a snapshot for operators. Reading it never blocks and never waits
// on the relay.
type Status struct {
	// State is the connector's lifecycle position.
	State State
	// Attempts is the number of consecutive failed dial-or-handshake
	// cycles; it returns to zero once a link has been held long enough to
	// count as healthy (see [Backoff.ResetAfter]).
	Attempts int
	// LastError is why the last connection ended or the last dial failed.
	// It is diagnostic: a non-nil value with State StateConnecting is the
	// ordinary state of a runtime whose relay is switched off.
	LastError error
	// ConnectedSince is when the current link completed its handshake,
	// zero when there is no link.
	ConnectedSince time.Time
	// Connections counts completed handshakes over the connector's life,
	// so a test or an operator can tell a reconnect from a link that never
	// dropped.
	Connections int
	// BadFrames counts frames rejected as malformed without ending the
	// connection. A rising count on a healthy link is a peer bug, not a
	// transport failure.
	BadFrames int
}

// Handler receives every non-control frame the relay sends. It is called on the
// read loop and must not block: a handler that waits turns relay backpressure
// into a stalled heartbeat and then into a reconnect. Carrying real sessions
// through it is a later step; a nil Handler drops routed frames.
type Handler func(librelay.Frame)

// Credentials are what the relay authenticates the instance with. They are
// opaque here: the connector never inspects them and never puts them in a
// frame, it only hands them to [DialFunc], because how a credential is
// presented is a property of the transport that pairing chooses.
type Credentials struct {
	// Token is the paired instance's secret. [DialTLS] presents it as a
	// bearer credential on the upgrade request; it never enters a frame.
	Token string
	// RelayPublicKey is the relay's long-lived Ed25519 key, pinned at
	// pairing time, in the encoding [librelay.ParsePublicKey] reads. When
	// it is set the handshake refuses any peer that cannot sign the
	// connector's nonce with it, fatally. When it is empty no relay
	// identity was pinned and the handshake proceeds unverified, which is
	// exactly how an unpaired runtime behaved before pairing existed.
	//
	// This pins the relay's application-layer identity, not its TLS leaf:
	// the certificate rotates roughly every 90 days and a binary pinning it
	// would break itself in the field.
	RelayPublicKey string
}

// Config is everything the connector needs. Endpoint and Credentials arrive
// from configuration already established; obtaining them is not this package's
// concern.
type Config struct {
	// Endpoint is the relay address to dial, in whatever form DialFunc
	// understands. It is never parsed here and no default exists: a
	// connector with no endpoint is not constructible.
	Endpoint string
	// Instance identifies this runtime to the relay. It is the routing key
	// on every frame, so it is required.
	Instance string
	// Credentials authenticate the instance. Empty is legal — a relay may
	// refuse, which is its decision to make.
	Credentials Credentials
	// Agent names this build for operator diagnosis. Nothing branches on
	// it.
	Agent string
	// Dial opens one connection. Defaults to [DialTLS]. Tests replace it,
	// which is also what keeps this package free of anything that listens.
	Dial DialFunc
	// Handler receives routed frames; nil drops them.
	Handler Handler
	// Backoff governs redial timing; the zero value means
	// [DefaultBackoff].
	Backoff Backoff
	// Heartbeat governs liveness probing; the zero value means
	// [DefaultHeartbeat].
	Heartbeat Heartbeat
	// HandshakeTimeout bounds hello→welcome. Zero means
	// DefaultHandshakeTimeout. It must be bounded: a relay that accepts a
	// TCP connection and then says nothing is the failure mode an
	// unbounded handshake never recovers from.
	HandshakeTimeout time.Duration
	// Backlog is the outbound queue depth. Zero means DefaultBacklog. It
	// is bounded so Send can fail instead of block.
	Backlog int
	// Tracker instruments connect, disconnect and refusals. Nil degrades
	// to [libtracker.NoopTracker]. It is the only instrumentation seam
	// here: values reported through a tracker are redacted by field name
	// before they are written, and an endpoint or a credential going
	// straight to a log file is exactly what that redaction exists to
	// stop.
	Tracker libtracker.ActivityTracker
}

// Defaults for the durations and sizes Config leaves zero.
const (
	DefaultHandshakeTimeout = 10 * time.Second
	DefaultBacklog          = 256
)

// Connector holds at most one relay connection and rebuilds it when it breaks.
// The zero value is unusable; call [New]. It is safe for concurrent use.
type Connector struct {
	cfg     Config
	tracker libtracker.ActivityTracker
	// relayKey is Credentials.RelayPublicKey decoded once in New, or nil
	// when nothing was pinned. Decoding at construction rather than per
	// handshake means a malformed key is a configuration error the caller
	// sees immediately, instead of a fatal state the retry loop discovers
	// on the first dial and reports as the relay's fault.
	relayKey ed25519.PublicKey

	// cur is the live session, or nil. Send reads it without a lock so a
	// caller on a mission path never contends with the retry loop.
	cur atomic.Pointer[session]

	startOnce sync.Once
	closeOnce sync.Once
	cancel    context.CancelFunc
	stopped   chan struct{}
	closed    atomic.Bool

	mu     sync.Mutex
	status Status
}

// New validates cfg and returns a connector that has not dialed anything. It
// performs no I/O, so it cannot fail because a relay is down.
func New(cfg Config) (*Connector, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("relaylink: Endpoint is required")
	}
	if cfg.Instance == "" {
		return nil, errors.New("relaylink: Instance is required")
	}
	if cfg.Dial == nil {
		cfg.Dial = DialTLS
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if cfg.Backlog <= 0 {
		cfg.Backlog = DefaultBacklog
	}
	cfg.Backoff = cfg.Backoff.withDefaults()
	cfg.Heartbeat = cfg.Heartbeat.withDefaults()
	if err := cfg.Backoff.validate(); err != nil {
		return nil, err
	}
	if err := cfg.Heartbeat.validate(); err != nil {
		return nil, err
	}
	if cfg.Tracker == nil {
		cfg.Tracker = libtracker.NoopTracker{}
	}
	var relayKey ed25519.PublicKey
	if cfg.Credentials.RelayPublicKey != "" {
		k, err := librelay.ParsePublicKey(cfg.Credentials.RelayPublicKey)
		if err != nil {
			return nil, fmt.Errorf("relaylink: RelayPublicKey: %w", err)
		}
		relayKey = k
	}
	return &Connector{
		cfg:      cfg,
		tracker:  cfg.Tracker,
		relayKey: relayKey,
		stopped:  make(chan struct{}),
		status:   Status{State: StateIdle},
	}, nil
}

// Start begins dialing in the background and returns immediately, before the
// first connection attempt has been made. It never waits on the relay; a caller
// that treats a returned nil as "connected" has misread the contract, which is
// the whole point — nothing in the runtime may sequence on a relay being up.
//
// Calling Start more than once returns an error rather than starting a second
// loop.
func (c *Connector) Start(ctx context.Context) error {
	if c.closed.Load() {
		return ErrClosed
	}
	mine := false
	c.startOnce.Do(func() { mine = true })
	if !mine {
		return errors.New("relaylink: already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
	c.setState(StateConnecting)
	go c.run(runCtx)
	// A Close that raced this Start may have found c.cancel still nil and
	// returned without waiting for a loop that did not exist yet. Re-reading
	// the flag after the loop is launched is what makes that ordering safe:
	// whoever is second cleans up.
	if c.closed.Load() {
		cancel()
		<-c.stopped
		return ErrClosed
	}
	return nil
}

// Close stops the retry loop, drops any held connection and waits for every
// goroutine this connector owns to exit. It is idempotent and safe to call on a
// connector that was never started.
func (c *Connector) Close() error {
	c.closed.Store(true)
	c.closeOnce.Do(func() {
		c.mu.Lock()
		cancel := c.cancel
		c.mu.Unlock()
		if cancel != nil {
			cancel()
			<-c.stopped
		}
		if s := c.cur.Swap(nil); s != nil {
			s.stop(ErrClosed)
		}
		c.setState(StateClosed)
	})
	return nil
}

// Status returns a snapshot. It never blocks on the relay.
func (c *Connector) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Send puts f on the relay if a link is currently held, and reports
// [ErrNotConnected] immediately if it is not. It never blocks on the network:
// the frame is queued for the writer goroutine, and a queue that is full is an
// error rather than a wait, so relay trouble can never become a mission's
// trouble.
func (c *Connector) Send(f librelay.Frame) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if err := f.Validate(); err != nil {
		return err
	}
	s := c.cur.Load()
	if s == nil {
		return ErrNotConnected
	}
	return s.enqueue(f)
}

// run is the supervisor: dial, serve, back off, repeat, until ctx is cancelled
// or a fatal answer makes retrying pointless.
func (c *Connector) run(ctx context.Context) {
	defer close(c.stopped)
	b := newBackoffState(c.cfg.Backoff)
	timer := time.NewTimer(time.Hour)
	// The timer is created stopped and drained: reusing one timer for
	// every backoff is what keeps a long-lived connector from accumulating
	// runtime timers, which is a leak that only shows up after days.
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		if ctx.Err() != nil {
			return
		}
		c.setState(StateConnecting)
		healthy, err := c.cycle(ctx)
		if ctx.Err() != nil {
			return
		}
		if healthy {
			// Reset only after the link proved it could stay up.
			// Resetting on a completed handshake alone would let a
			// relay that accepts and immediately drops pull the
			// connector into an unthrottled redial loop.
			b.reset()
		}
		if isFatal(err) {
			// Reported once, on the way out. A retryable failure is
			// deliberately not reported again here: it was already
			// attributed to the attempt that produced it, and a
			// second record per retry is how an unreachable relay
			// buries a real incident.
			reportErr, _, end := c.tracker.Start(ctx, "abandon", "relay", "endpoint", c.cfg.Endpoint)
			reportErr(err)
			end()
			c.setFatal(err)
			return
		}
		d := b.next()
		timer.Reset(d)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

// cycle runs one dial-and-serve. It reports whether the link stayed up long
// enough to count as healthy, and why the attempt ended; success is impossible,
// since a connection that never ends never returns.
func (c *Connector) cycle(ctx context.Context) (healthy bool, err error) {
	// One tracked attempt per cycle, ending when the link is established or
	// the attempt has failed. The lifetime of the resulting connection is a
	// separate operation (see hold), so this span measures "how long did it
	// take to get up" and that one measures "how long did it stay up".
	//
	// Argument order is (operation, subject) — the verb, then the thing it
	// acts on — matching every other caller in this repo ("publish",
	// "status_changed_event"; "sweep", "abandoned_missions").
	reportErr, reportChange, end := c.tracker.Start(ctx, "connect", "relay", "endpoint", c.cfg.Endpoint)
	conn, err := c.cfg.Dial(ctx, c.cfg.Endpoint, c.cfg.Credentials)
	if err != nil {
		reportErr(err)
		end()
		c.noteError(err)
		return false, err
	}
	rd, w, err := c.handshake(conn)
	if err != nil {
		reportErr(err)
		end()
		_ = conn.Close()
		c.noteError(err)
		return false, err
	}
	reportChange(c.cfg.Instance, "connected")
	end()

	started := time.Now()
	// hold owns its own tracker operation: its duration IS how long the link
	// stayed up, which is what tells a stable relay from a flapping one, and
	// is the same quantity ResetAfter below is judged against.
	err = c.hold(ctx, conn, rd, w)
	c.noteError(err)
	return time.Since(started) >= c.cfg.Backoff.ResetAfter, err
}

// hold runs a handshaken connection until it ends, and closes it on every
// path. It returns the reason the connection ended, which is always non-nil:
// a link that has not ended has not returned.
func (c *Connector) hold(ctx context.Context, conn net.Conn, rd *librelay.Reader, w *librelay.Writer) error {
	defer func() { _ = conn.Close() }()
	s := newSession(conn, rd, w, c.cfg.Backlog)

	// The held connection is the operation; everything notable that happens
	// while it is up is a change on it. reportErr carries the reason it ended
	// — always non-nil — so a relay restart is distinguishable from a link
	// that was never up by the span's duration alone.
	reportErr, reportChange, end := c.tracker.Start(ctx, "hold", "relay", "endpoint", c.cfg.Endpoint)
	s.note = reportChange
	defer end()

	c.cur.Store(s)
	c.markConnected()
	defer func() {
		c.cur.CompareAndSwap(s, nil)
		c.markDisconnected()
	}()

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); c.readLoop(ctx, s) }()
	go func() { defer wg.Done(); s.writeLoop() }()
	go func() { defer wg.Done(); c.heartbeatLoop(s) }()

	select {
	case <-ctx.Done():
		s.stop(ctx.Err())
	case <-s.done:
	}
	wg.Wait()
	err := s.reason()
	if err != nil {
		reportErr(err)
	}
	return err
}

// setState records a lifecycle transition.
func (c *Connector) setState(s State) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status.State == StateClosed && s != StateClosed {
		return
	}
	c.status.State = s
}

func (c *Connector) setFatal(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.State = StateFatal
	c.status.LastError = err
}

func (c *Connector) noteError(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.LastError = err
	c.status.Attempts++
}

func (c *Connector) markConnected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.State = StateConnected
	c.status.ConnectedSince = time.Now()
	c.status.Connections++
	c.status.Attempts = 0
}

func (c *Connector) markDisconnected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.ConnectedSince = time.Time{}
	if c.status.State == StateConnected {
		c.status.State = StateConnecting
	}
}

func (c *Connector) noteBadFrame() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.BadFrames++
}

// handshake sends hello and waits for welcome, refusing anything else.
//
// It returns the reader and writer it used rather than fresh ones, because the
// reader may already hold buffered bytes of the frames that followed welcome;
// building a second reader on the same connection would silently discard them.
func (c *Connector) handshake(conn net.Conn) (*librelay.Reader, *librelay.Writer, error) {
	// One deadline covers the whole exchange and is cleared afterwards:
	// a live connection must not inherit a handshake's deadline, or it
	// dies on a schedule that has nothing to do with liveness.
	if err := conn.SetDeadline(time.Now().Add(c.cfg.HandshakeTimeout)); err != nil {
		return nil, nil, fmt.Errorf("relaylink: set handshake deadline: %w", err)
	}
	rd, w := librelay.NewReader(conn), librelay.NewWriter(conn)
	const helloID = "hello"
	// A fresh nonce per handshake, always — including when nothing is
	// pinned, so a relay's answer never depends on whether this connector
	// intends to check it.
	nonce, err := librelay.NewNonce()
	if err != nil {
		return nil, nil, err
	}
	hello, err := librelay.Frame{Type: librelay.TypeHello, Instance: c.cfg.Instance, ID: helloID}.
		WithPayload(librelay.Hello{
			ProtocolVersion: librelay.ProtocolVersion,
			Instance:        c.cfg.Instance,
			Agent:           c.cfg.Agent,
			Nonce:           nonce,
		})
	if err != nil {
		return nil, nil, err
	}
	if err := w.WriteFrame(hello); err != nil {
		return nil, nil, fmt.Errorf("relaylink: send hello: %w", err)
	}
	for {
		f, err := rd.ReadFrame()
		if err != nil {
			if isMalformed(err) {
				// Per-frame, as the codec defines it: a peer
				// that garbled one line has not proved it
				// cannot answer the handshake.
				c.noteBadFrame()
				continue
			}
			return nil, nil, fmt.Errorf("relaylink: await welcome: %w", err)
		}
		if f.ReplyTo != helloID {
			// Traffic that arrives before welcome is not ours to
			// route yet. An unknown control request still gets its
			// owed answer so the peer is not left waiting.
			if reply, owed := librelay.Unsupported(f); owed && librelay.IsControl(f.Type) {
				_ = w.WriteFrame(reply)
			}
			continue
		}
		switch f.Type {
		case librelay.TypeWelcome:
			var wel librelay.Welcome
			if err := f.DecodePayload(&wel); err != nil {
				return nil, nil, err
			}
			if wel.ProtocolVersion < MinProtocolVersion || wel.ProtocolVersion > librelay.ProtocolVersion {
				return nil, nil, fmt.Errorf("%w: relay chose %d, this build speaks %d..%d",
					ErrVersionNegotiation, wel.ProtocolVersion, MinProtocolVersion, librelay.ProtocolVersion)
			}
			// The signature is checked over the version the relay
			// selected, after that version has been accepted: a
			// signature is only meaningful once it is known which
			// negotiation it belongs to.
			if c.relayKey != nil {
				if err := librelay.VerifyWelcome(c.relayKey, nonce, wel.ProtocolVersion, c.cfg.Instance, wel.Signature); err != nil {
					return nil, nil, fmt.Errorf("%w: %w", ErrRelayIdentity, err)
				}
			}
			if err := conn.SetDeadline(time.Time{}); err != nil {
				return nil, nil, fmt.Errorf("relaylink: clear handshake deadline: %w", err)
			}
			return rd, w, nil
		case librelay.TypeError:
			var e librelay.Error
			_ = f.DecodePayload(&e)
			switch e.Code {
			case librelay.CodeUnauthorized:
				return nil, nil, fmt.Errorf("%w: %s", ErrUnauthorized, e.Message)
			case librelay.CodeVersion:
				return nil, nil, fmt.Errorf("%w: %s", ErrVersionNegotiation, e.Message)
			}
			return nil, nil, fmt.Errorf("relaylink: relay refused hello: %s: %s", e.Code, e.Message)
		default:
			return nil, nil, fmt.Errorf("relaylink: relay answered hello with %q", f.Type)
		}
	}
}

// readLoop consumes frames until the connection ends, honouring the codec's
// error split: a malformed frame is counted and skipped, and only a framing or
// I/O failure ends the connection. One garbled message must not drop a link
// that other sessions are riding on.
func (c *Connector) readLoop(ctx context.Context, s *session) {
	for {
		f, err := s.rd.ReadFrame()
		if err != nil {
			if isMalformed(err) {
				c.noteBadFrame()
				continue
			}
			s.stop(err)
			return
		}
		c.dispatch(ctx, s, f)
	}
}

// dispatch routes one inbound frame: control traffic is answered here, and
// everything else — including types this build has never heard of — goes to the
// handler as opaque cargo.
func (c *Connector) dispatch(ctx context.Context, s *session, f librelay.Frame) {
	if !librelay.IsControl(f.Type) {
		if c.cfg.Handler != nil {
			c.cfg.Handler(f)
		}
		return
	}
	switch f.Type {
	case librelay.TypeHeartbeat:
		if f.IsRequest() {
			_ = s.enqueue(librelay.Frame{Type: librelay.TypeAck, Instance: f.Instance, ReplyTo: f.ID})
		}
	case librelay.TypeAck:
		s.ack(f.ReplyTo)
	case librelay.TypeError:
		// An error frame on a held connection concerns something this
		// end sent, not the connection: it is recorded and the link
		// carries on.
		var e librelay.Error
		_ = f.DecodePayload(&e)
		// Not fatal to the connection, so it is a change on the hold
		// operation rather than an error that ends it.
		s.note0("peer_error", map[string]string{
			"code":    e.Code,
			"message": e.Message,
			"re":      f.ReplyTo,
		})
	case librelay.TypeWelcome:
		// A second welcome is not owed an answer; the handshake is
		// already settled and re-negotiating mid-connection is not a
		// thing the protocol has.
	default:
		// Includes an inbound hello, which a relay has no business
		// sending: the rule is librelay's, not this package's.
		if reply, owed := librelay.Unsupported(f); owed {
			_ = s.enqueue(reply)
		}
	}
}

// heartbeatLoop probes the peer and ends the connection when a probe goes
// unanswered. It is the only detector for a relay that died without closing its
// TCP connection — the failure a read loop waits out forever.
func (c *Connector) heartbeatLoop(s *session) {
	hb := c.cfg.Heartbeat
	tick := time.NewTicker(hb.Interval)
	defer tick.Stop()
	timeout := time.NewTimer(time.Hour)
	if !timeout.Stop() {
		<-timeout.C
	}
	defer timeout.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
		}
		id, waitCh := s.arm()
		probe := librelay.Frame{Type: librelay.TypeHeartbeat, Instance: c.cfg.Instance, ID: id}
		if err := s.enqueue(probe); err != nil {
			// A full backlog means the peer is not draining the
			// connection, which is the same liveness failure a
			// missing ack reports.
			s.stop(fmt.Errorf("%w: %w", ErrHeartbeatTimeout, err))
			return
		}
		timeout.Reset(hb.Timeout)
		select {
		case <-waitCh:
			if !timeout.Stop() {
				<-timeout.C
			}
		case <-timeout.C:
			s.stop(fmt.Errorf("%w after %s", ErrHeartbeatTimeout, hb.Timeout))
			return
		case <-s.done:
			if !timeout.Stop() {
				<-timeout.C
			}
			return
		}
	}
}

// isFatal reports whether retrying can possibly produce a different answer. It
// is deliberately a short list: everything unrecognized is retryable, because
// the cost of retrying a permanent failure is a capped backoff and the cost of
// giving up on a transient one is a runtime that stays dark until it restarts.
func isFatal(err error) bool {
	return errors.Is(err, ErrVersionNegotiation) || errors.Is(err, ErrUnauthorized) ||
		errors.Is(err, ErrRelayIdentity)
}

// isMalformed reports whether err is the codec's per-frame failure, after which
// the reader is still usable. It answers false for anything it does not
// recognize, including [librelay.ErrFrameTooLarge]: an oversized line is not
// consumed and must not be resynchronized past, so the connection ends.
func isMalformed(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, librelay.ErrFrameTooLarge) || errors.Is(err, librelay.ErrReaderClosed) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return false
	}
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	if errors.As(err, &syn) || errors.As(err, &typ) {
		return true
	}
	for _, sentinel := range []error{
		librelay.ErrEmptyType, librelay.ErrTypeTooLong, librelay.ErrIDTooLong,
		librelay.ErrControlChar, librelay.ErrBothIDs, librelay.ErrSessionAlone,
		librelay.ErrNotUTF8,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}
