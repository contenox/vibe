// Package relaylink is the runtime half of the relay connection: it dials a
// relay, completes the [librelay] handshake, holds the connection, proves the
// peer is alive with heartbeats, and redials with backoff when it is not.
// Every exported call returns in bounded time regardless of the relay's
// state, and [Connector.Send] fails immediately with [ErrNotConnected]
// instead of waiting for a link to come back. The connector only ever opens
// outbound connections; it never listens.
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

// MinProtocolVersion is the oldest envelope version this build will speak; the
// connector refuses a negotiated version outside what it can honour.
const MinProtocolVersion = 1

// Connector failures a caller can act on; everything else stays internal to
// the retry loop.
var (
	// ErrNotConnected is returned by Send when no link is currently held.
	ErrNotConnected = errors.New("relaylink: not connected to a relay")
	// ErrBacklogFull is returned by Send when the outbound queue is full.
	ErrBacklogFull = errors.New("relaylink: outbound queue is full")
	// ErrClosed is returned by Start or Send after Close.
	ErrClosed = errors.New("relaylink: connector is closed")
	// ErrVersionNegotiation is the handshake refusing a version outside
	// [MinProtocolVersion]..[librelay.ProtocolVersion]; fatal, not retried.
	ErrVersionNegotiation = errors.New("relaylink: relay selected an unsupported protocol version")
	// ErrUnauthorized is a relay refusing the credentials; fatal.
	ErrUnauthorized = errors.New("relaylink: relay refused the credentials")
	// ErrRelayIdentity is the peer failing to prove it is the paired relay
	// against [Credentials.RelayPublicKey]; fatal.
	ErrRelayIdentity = errors.New("relaylink: relay did not prove its identity")
	// ErrHeartbeatTimeout ends a connection whose peer stopped answering a
	// heartbeat probe.
	ErrHeartbeatTimeout = errors.New("relaylink: peer did not answer a heartbeat")
)

// State is where the connector is in its lifecycle; diagnostic only, nothing
// may branch on it.
type State int

// Connector states.
const (
	// StateIdle is before Start and after a Close that never connected.
	StateIdle State = iota
	// StateConnecting is dialing, handshaking, or waiting out a backoff.
	StateConnecting
	// StateConnected means a handshake completed and the link is held.
	StateConnected
	// StateFatal means the retry loop gave up; negotiation or credentials
	// failing cannot be fixed by retrying.
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

// Status is a snapshot for operators; reading it never blocks on the relay.
type Status struct {
	// State is the connector's lifecycle position.
	State State
	// Attempts is the number of consecutive failed dial-or-handshake
	// cycles; it returns to zero once a link has been held long enough to
	// count as healthy (see [Backoff.ResetAfter]).
	Attempts int
	// LastError is why the last connection ended or the last dial failed.
	LastError error
	// ConnectedSince is when the current link completed its handshake,
	// zero when there is no link.
	ConnectedSince time.Time
	// Connections counts completed handshakes over the connector's life.
	Connections int
	// BadFrames counts frames rejected as malformed without ending the
	// connection.
	BadFrames int
}

// Handler receives every non-control frame the relay sends; it must not
// block, since it runs on the read loop.
type Handler func(context.Context, librelay.Frame)

// Credentials are what the relay authenticates the instance with; opaque
// here, only passed to [DialFunc].
type Credentials struct {
	// Token is the paired instance's secret, presented by [DialTLS] as a
	// bearer credential and never placed in a frame.
	Token string
	// RelayPublicKey is the relay's long-lived Ed25519 key pinned at
	// pairing time; when set, the handshake fatally refuses any peer that
	// cannot sign the connector's nonce with it.
	RelayPublicKey string
}

// Config is everything the connector needs; Endpoint and Credentials are
// assumed already established.
type Config struct {
	// Endpoint is the relay address to dial, in whatever form Dial
	// understands; required.
	Endpoint string
	// Instance identifies this runtime to the relay and is the routing key
	// on every frame; required.
	Instance string
	// Credentials authenticate the instance; empty is legal, refusal is
	// the relay's decision.
	Credentials Credentials
	// Agent names this build for operator diagnosis.
	Agent string
	// Dial opens one connection; defaults to [DialTLS].
	Dial DialFunc
	// Handler receives routed frames; nil drops them.
	Handler Handler
	// Backoff governs redial timing; the zero value means
	// [DefaultBackoff].
	Backoff Backoff
	// Heartbeat governs liveness probing; the zero value means
	// [DefaultHeartbeat].
	Heartbeat Heartbeat
	// HandshakeTimeout bounds hello→welcome; zero means
	// DefaultHandshakeTimeout.
	HandshakeTimeout time.Duration
	// Backlog is the outbound queue depth; zero means DefaultBacklog.
	Backlog int
	// Tracker instruments connect, disconnect and refusals; nil degrades
	// to [libtracker.NoopTracker].
	Tracker libtracker.ActivityTracker
}

// Defaults for the durations and sizes Config leaves zero.
const (
	DefaultHandshakeTimeout = 10 * time.Second
	DefaultBacklog          = 256
)

// Connector holds at most one relay connection and rebuilds it when it
// breaks; the zero value is unusable — call [New] — and it is safe for
// concurrent use.
type Connector struct {
	cfg      Config
	tracker  libtracker.ActivityTracker
	relayKey ed25519.PublicKey

	cur atomic.Pointer[session]

	retryHint atomic.Int64

	startOnce sync.Once
	closeOnce sync.Once
	cancel    context.CancelFunc
	stopped   chan struct{}
	closed    atomic.Bool

	mu     sync.Mutex
	status Status
}

// New validates cfg and returns a connector that has not dialed anything; it
// performs no I/O.
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

// Start begins dialing in the background and returns immediately; calling it
// more than once returns an error rather than starting a second loop.
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
	// Re-read after launch: a Close racing Start may have seen c.cancel nil
	// and returned early, so whoever observes closed second cleans up.
	if c.closed.Load() {
		cancel()
		<-c.stopped
		return ErrClosed
	}
	return nil
}

// Close stops the retry loop, drops any held connection, and waits for every
// goroutine this connector owns to exit; idempotent.
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

// Status returns a snapshot; it never blocks on the relay.
func (c *Connector) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Send puts f on the relay if a link is currently held, reporting
// [ErrNotConnected] immediately if not; it never blocks on the network.
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

func (c *Connector) run(ctx context.Context) {
	defer close(c.stopped)
	b := newBackoffState(c.cfg.Backoff)
	timer := time.NewTimer(time.Hour)
	// Created stopped and drained; reused across every backoff so a
	// long-lived connector does not accumulate timers.
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
			// Reset only once the link proved healthy, not merely handshaken.
			b.reset()
		}
		if isFatal(err) {
			reportErr, _, end := c.tracker.Start(ctx, "abandon", "relay", "endpoint", c.cfg.Endpoint)
			reportErr(err)
			end()
			c.setFatal(err)
			return
		}
		// Consumed once: a stale hint must not govern a dial the relay
		// never sent it to.
		d := b.nextHinted(time.Duration(c.retryHint.Swap(0)))
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

func (c *Connector) cycle(ctx context.Context) (healthy bool, err error) {
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
	err = c.hold(ctx, conn, rd, w)
	c.noteError(err)
	return time.Since(started) >= c.cfg.Backoff.ResetAfter, err
}

func (c *Connector) hold(ctx context.Context, conn net.Conn, rd *librelay.Reader, w *librelay.Writer) error {
	defer func() { _ = conn.Close() }()
	s := newSession(conn, rd, w, c.cfg.Backlog)

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

func (c *Connector) handshake(conn net.Conn) (*librelay.Reader, *librelay.Writer, error) {
	// Cleared afterwards: a live connection must not inherit the handshake
	// deadline.
	if err := conn.SetDeadline(time.Now().Add(c.cfg.HandshakeTimeout)); err != nil {
		return nil, nil, fmt.Errorf("relaylink: set handshake deadline: %w", err)
	}
	rd, w := librelay.NewReader(conn), librelay.NewWriter(conn)
	const helloID = "hello"
	// Fresh nonce every handshake, even when nothing is pinned, so the
	// relay's answer never varies with local verification intent.
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
				// A garbled line does not prove the peer cannot
				// answer the handshake.
				c.noteBadFrame()
				continue
			}
			return nil, nil, fmt.Errorf("relaylink: await welcome: %w", err)
		}
		if f.ReplyTo != helloID {
			// Not yet ours to route, but an owed control reply still
			// must be sent.
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
			// Checked against the version the relay selected, after
			// that version is accepted — meaningless before then.
			if c.relayKey != nil {
				if err := librelay.VerifyWelcome(c.relayKey, nonce, wel.ProtocolVersion, c.cfg.Instance, wel.Signature); err != nil {
					return nil, nil, fmt.Errorf("%w: %w", ErrRelayIdentity, err)
				}
			}
			// Recorded for the NEXT dial; this connection has only just
			// been established.
			c.retryHint.Store(int64(time.Duration(wel.RetryAfterSeconds) * time.Second))
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

func (c *Connector) dispatch(ctx context.Context, s *session, f librelay.Frame) {
	if !librelay.IsControl(f.Type) {
		if c.cfg.Handler != nil {
			c.cfg.Handler(traceContext(ctx, f), f)
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
		// Concerns something this end sent, not the connection; the link
		// carries on.
		var e librelay.Error
		_ = f.DecodePayload(&e)
		s.note0("peer_error", map[string]string{
			"code":    e.Code,
			"message": e.Message,
			"re":      f.ReplyTo,
		})
	case librelay.TypeWelcome:
		// A second welcome is not owed an answer; mid-connection
		// renegotiation is not a protocol thing.
	default:
		// Includes an inbound hello, which librelay's rules — not this
		// package's — forbid a relay from sending.
		if reply, owed := librelay.Unsupported(f); owed {
			_ = s.enqueue(reply)
		}
	}
}

func traceContext(ctx context.Context, f librelay.Frame) context.Context {
	if f.Trace == "" || !librelay.ValidTraceID(f.Trace) {
		return ctx
	}
	return context.WithValue(ctx, libtracker.ContextKeyTraceID, f.Trace)
}

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
			// A full backlog is the same liveness failure as a missing
			// ack.
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

func isFatal(err error) bool {
	return errors.Is(err, ErrVersionNegotiation) || errors.Is(err, ErrUnauthorized) ||
		errors.Is(err, ErrRelayIdentity)
}

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
		librelay.ErrTraceTooLong, librelay.ErrTraceCharset,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}
