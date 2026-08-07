package relaylink_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/relaylink"
	"github.com/contenox/contenox/internal/relaytest"
	"github.com/contenox/contenox/librelay"
)

// testTimeout keeps a failing assertion a failure rather than a hung `go test`.
const testTimeout = 10 * time.Second

// fastBackoff makes the retry loop observable inside a test without changing
// its shape: the same exponential-with-jitter policy, three orders of magnitude
// down.
var fastBackoff = relaylink.Backoff{
	Initial:    2 * time.Millisecond,
	Max:        20 * time.Millisecond,
	Factor:     2,
	ResetAfter: 5 * time.Millisecond,
}

// fastHeartbeat probes often enough that a silent peer is detected within a
// test's patience.
var fastHeartbeat = relaylink.Heartbeat{
	Interval: 20 * time.Millisecond,
	Timeout:  50 * time.Millisecond,
}

func baseConfig() relaylink.Config {
	return relaylink.Config{
		Endpoint:         "relay.invalid:443",
		Instance:         "inst-a",
		Agent:            "contenox/test",
		Backoff:          fastBackoff,
		Heartbeat:        fastHeartbeat,
		HandshakeTimeout: 2 * time.Second,
	}
}

// waitFor polls cond until it holds or the deadline passes. Polling rather than
// signalling is deliberate: the connector exposes no "wait until connected"
// call, because nothing in the runtime is allowed to have one.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// relayDialer wires a connector to a relaytest double and records every link it
// hands out.
type relayDialer struct {
	relay *relaytest.Relay

	mu    sync.Mutex
	links []*relaytest.Link
}

func (d *relayDialer) dial(context.Context, string, relaylink.Credentials) (net.Conn, error) {
	l := d.relay.Dial()
	d.mu.Lock()
	d.links = append(d.links, l)
	d.mu.Unlock()
	return l.Conn(), nil
}

func (d *relayDialer) link(i int) *relaytest.Link {
	d.mu.Lock()
	defer d.mu.Unlock()
	if i >= len(d.links) {
		return nil
	}
	return d.links[i]
}

// TestUnit_HandshakeNegotiatesAndHoldsTheConnection is the happy path: hello is
// sent with this build's version, welcome is accepted, and the relay considers
// the instance bound.
func TestUnit_HandshakeNegotiatesAndHoldsTheConnection(t *testing.T) {
	t.Parallel()
	r := relaytest.New()
	defer r.Close()
	d := &relayDialer{relay: r}

	cfg := baseConfig()
	cfg.Dial = d.dial
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, "connected", func() bool { return c.Status().State == relaylink.StateConnected })
	waitFor(t, "instance bound", func() bool {
		l := d.link(0)
		return l != nil && l.Instance() == "inst-a"
	})

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	hello, err := d.link(0).Recv(ctx)
	if err != nil {
		t.Fatalf("Recv hello: %v", err)
	}
	if hello.Type != librelay.TypeHello || !hello.IsRequest() || hello.Instance != "inst-a" {
		t.Fatalf("first frame = %+v, want a hello request for inst-a", hello)
	}
	var h librelay.Hello
	if err := hello.DecodePayload(&h); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if h.ProtocolVersion != librelay.ProtocolVersion || h.Instance != "inst-a" {
		t.Fatalf("hello payload = %+v", h)
	}
	if got := c.Status().Connections; got != 1 {
		t.Fatalf("Connections = %d, want 1", got)
	}
}

// TestUnit_HeartbeatProbesArePairedWithAcks proves the liveness probe is on the
// wire and correlated, which is what distinguishes a live peer from one a round
// behind.
func TestUnit_HeartbeatProbesArePairedWithAcks(t *testing.T) {
	t.Parallel()
	r := relaytest.New()
	defer r.Close()
	d := &relayDialer{relay: r}
	cfg := baseConfig()
	cfg.Dial = d.dial
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "connected", func() bool { return c.Status().State == relaylink.StateConnected })

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	seen := map[string]bool{}
	for range 20 {
		f, err := d.link(0).Recv(ctx)
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if f.Type != librelay.TypeHeartbeat {
			continue
		}
		if !f.IsRequest() {
			t.Fatalf("heartbeat without an id: %+v", f)
		}
		if seen[f.ID] {
			t.Fatalf("heartbeat id %q reused", f.ID)
		}
		seen[f.ID] = true
		if len(seen) == 2 {
			break
		}
	}
	if len(seen) < 2 {
		t.Fatalf("saw %d heartbeats, want at least 2", len(seen))
	}
	// The relay double answered every probe; the connection must still be
	// the one it started on.
	if got := c.Status().Connections; got != 1 {
		t.Fatalf("Connections = %d after healthy heartbeats, want 1", got)
	}
}

// TestUnit_ReconnectsAfterTheRelayIsKilled covers a relay restart: the link is
// slammed shut with no close frame and the connector comes back on its own,
// without anybody being told.
func TestUnit_ReconnectsAfterTheRelayIsKilled(t *testing.T) {
	t.Parallel()
	r := relaytest.New()
	defer r.Close()
	d := &relayDialer{relay: r}
	cfg := baseConfig()
	cfg.Dial = d.dial
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "first connection", func() bool { return c.Status().Connections == 1 })

	d.link(0).Drop()

	waitFor(t, "reconnection", func() bool { return c.Status().Connections >= 2 })
	waitFor(t, "connected again", func() bool { return c.Status().State == relaylink.StateConnected })
	waitFor(t, "second link bound", func() bool {
		l := d.link(1)
		return l != nil && l.Instance() == "inst-a"
	})
	if c.Status().State == relaylink.StateFatal {
		t.Fatal("a dropped relay must never be fatal")
	}
}

// rawRelay is a hand-driven peer for the cases relaytest deliberately cannot
// produce: raw bytes on the wire, and a peer that completes a handshake and
// then goes silent without hanging up.
type rawRelay struct {
	mu    sync.Mutex
	conns []net.Conn
	dials int
	ready chan net.Conn
}

func newRawRelay() *rawRelay { return &rawRelay{ready: make(chan net.Conn, 8)} }

func (p *rawRelay) dial(context.Context, string, relaylink.Credentials) (net.Conn, error) {
	peer, connector := net.Pipe()
	p.mu.Lock()
	p.conns = append(p.conns, peer, connector)
	p.dials++
	p.mu.Unlock()
	p.ready <- peer
	return connector, nil
}

func (p *rawRelay) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dials
}

func (p *rawRelay) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
}

// accept waits for the next dial and returns the peer side.
func (p *rawRelay) accept(t *testing.T) net.Conn {
	t.Helper()
	select {
	case c := <-p.ready:
		return c
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a dial")
		return nil
	}
}

// welcome reads the connector's hello and answers with version. It returns the
// reader and writer so the caller can keep driving the same connection.
func welcome(t *testing.T, conn net.Conn, version int) (*librelay.Reader, *librelay.Writer) {
	t.Helper()
	rd, w := librelay.NewReader(conn), librelay.NewWriter(conn)
	_ = conn.SetDeadline(time.Now().Add(testTimeout))
	f, err := rd.ReadFrame()
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if f.Type != librelay.TypeHello {
		t.Fatalf("first frame = %q, want hello", f.Type)
	}
	reply, err := librelay.Frame{Type: librelay.TypeWelcome, Instance: f.Instance, ReplyTo: f.ID}.
		WithPayload(librelay.Welcome{ProtocolVersion: version, Relay: "rawrelay"})
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	if err := w.WriteFrame(reply); err != nil {
		t.Fatalf("write welcome: %v", err)
	}
	_ = conn.SetDeadline(time.Time{})
	return rd, w
}

// TestUnit_SilentRelayIsDetectedByHeartbeat is the failure a disconnect test
// misses: the peer holds the TCP connection open and answers nothing. Only the
// heartbeat can notice, and the connector must redial rather than sit on a dead
// link that looks healthy.
func TestUnit_SilentRelayIsDetectedByHeartbeat(t *testing.T) {
	t.Parallel()
	p := newRawRelay()
	defer p.closeAll()
	cfg := baseConfig()
	cfg.Dial = p.dial
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	conn := p.accept(t)
	welcome(t, conn, librelay.ProtocolVersion)
	waitFor(t, "connected", func() bool { return c.Status().Connections == 1 })

	// From here the peer never reads and never writes. Draining is
	// required only so the connector's writes are not what blocks; the
	// point is that nothing is ever answered.
	go func() { _, _ = io.Copy(io.Discard, conn) }()

	waitFor(t, "the silent peer to be redialed", func() bool { return p.count() >= 2 })
	if err := c.Status().LastError; !errors.Is(err, relaylink.ErrHeartbeatTimeout) {
		t.Fatalf("LastError = %v, want a heartbeat timeout", err)
	}

	// And it keeps trying: a silent relay is not fatal.
	conn2 := p.accept(t)
	welcome(t, conn2, librelay.ProtocolVersion)
	waitFor(t, "reconnection", func() bool { return c.Status().Connections >= 2 })
}

// TestUnit_MalformedFrameDoesNotDropTheConnection holds the codec's error
// split: one garbled line is per-frame, and the link keeps carrying everything
// else. Dropping the connection here would take down every session riding it.
func TestUnit_MalformedFrameDoesNotDropTheConnection(t *testing.T) {
	t.Parallel()
	p := newRawRelay()
	defer p.closeAll()

	routed := make(chan librelay.Frame, 4)
	cfg := baseConfig()
	cfg.Dial = p.dial
	cfg.Handler = func(f librelay.Frame) { routed <- f }
	// Liveness is not what is under test, and a probe arriving mid-assertion
	// would be a second reason for the connection to end.
	cfg.Heartbeat = relaylink.Heartbeat{Interval: time.Minute, Timeout: time.Minute}
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	conn := p.accept(t)
	_, w := welcome(t, conn, librelay.ProtocolVersion)
	waitFor(t, "connected", func() bool { return c.Status().Connections == 1 })

	// Three kinds of bad frame, each of which the reader survives: broken
	// JSON, a valid JSON object that is not an addressable frame, and a
	// frame whose session names no instance.
	for _, bad := range []string{
		`{"type":`,
		`{"type":""}`,
		`{"type":"acp.message","session":"s1"}`,
	} {
		if _, err := conn.Write([]byte(bad + "\n")); err != nil {
			t.Fatalf("write %q: %v", bad, err)
		}
	}
	good, err := librelay.Frame{Type: librelay.TypeACPMessage, Instance: "inst-a", Session: "s1"}.
		WithPayload(json.RawMessage(`{"jsonrpc":"2.0"}`))
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	if err := w.WriteFrame(good); err != nil {
		t.Fatalf("write good frame: %v", err)
	}

	select {
	case f := <-routed:
		if f.Session != "s1" {
			t.Fatalf("routed frame = %+v", f)
		}
	case <-time.After(testTimeout):
		t.Fatal("the frame after three malformed ones never arrived")
	}
	if got := c.Status().BadFrames; got != 3 {
		t.Fatalf("BadFrames = %d, want 3", got)
	}
	if got := c.Status().Connections; got != 1 {
		t.Fatalf("Connections = %d: a malformed frame must not end the connection", got)
	}
	if got := p.count(); got != 1 {
		t.Fatalf("dials = %d: a malformed frame must not cause a redial", got)
	}
}

// TestUnit_OversizedFrameEndsTheConnection is the other half of the split.
// [librelay.ErrFrameTooLarge] leaves the offending line unconsumed, so there is
// no safe place to resume: the connector must hang up and redial rather than
// resynchronize on a newline the peer chose.
func TestUnit_OversizedFrameEndsTheConnection(t *testing.T) {
	t.Parallel()
	p := newRawRelay()
	defer p.closeAll()
	cfg := baseConfig()
	cfg.Dial = p.dial
	// The heartbeat is disarmed so that ending the connection can only be
	// the oversized frame's doing, not a probe that went unanswered while
	// the peer was busy writing it.
	cfg.Heartbeat = relaylink.Heartbeat{Interval: time.Minute, Timeout: time.Minute}
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	conn := p.accept(t)
	welcome(t, conn, librelay.ProtocolVersion)
	waitFor(t, "connected", func() bool { return c.Status().Connections == 1 })

	go func() {
		chunk := make([]byte, 64<<10)
		for i := range chunk {
			chunk[i] = 'a'
		}
		for written := 0; written <= librelay.MaxFrameBytes; written += len(chunk) {
			if _, err := conn.Write(chunk); err != nil {
				return // the connector hung up, which is the point
			}
		}
	}()

	waitFor(t, "the connection to be abandoned and redialed", func() bool { return p.count() >= 2 })
	if got := c.Status().BadFrames; got != 0 {
		t.Fatalf("BadFrames = %d: an oversized frame is terminal, not skippable", got)
	}
	if err := c.Status().LastError; !errors.Is(err, librelay.ErrFrameTooLarge) {
		t.Fatalf("LastError = %v, want ErrFrameTooLarge", err)
	}
}

// TestUnit_UnsupportedVersionIsRefusedAndFatal proves the connector will not
// hold a connection on a version it cannot speak. A link that looks healthy and
// is not is worse than no link.
func TestUnit_UnsupportedVersionIsRefusedAndFatal(t *testing.T) {
	t.Parallel()
	p := newRawRelay()
	defer p.closeAll()
	cfg := baseConfig()
	cfg.Dial = p.dial
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	conn := p.accept(t)
	welcome(t, conn, librelay.ProtocolVersion+1)

	waitFor(t, "the negotiation to be refused", func() bool { return c.Status().State == relaylink.StateFatal })
	if err := c.Status().LastError; !errors.Is(err, relaylink.ErrVersionNegotiation) {
		t.Fatalf("LastError = %v, want a version negotiation failure", err)
	}
	if got := c.Status().Connections; got != 0 {
		t.Fatalf("Connections = %d: a refused negotiation is not a connection", got)
	}
	// Fatal means the loop stopped; it must not keep hammering the relay.
	time.Sleep(50 * time.Millisecond)
	if got := p.count(); got != 1 {
		t.Fatalf("dials = %d after a fatal refusal, want 1", got)
	}
}

// TestUnit_UnknownControlTypeIsAnsweredWithUnsupported checks the connector
// applies librelay's compatibility rule rather than inventing its own: an
// unknown control request gets exactly one error reply, and an unknown control
// notification gets none.
func TestUnit_UnknownControlTypeIsAnsweredWithUnsupported(t *testing.T) {
	t.Parallel()
	r := relaytest.New()
	defer r.Close()
	d := &relayDialer{relay: r}
	cfg := baseConfig()
	cfg.Dial = d.dial
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "connected", func() bool { return c.Status().State == relaylink.StateConnected })

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	// A notification first, then a request: if the connector wrongly
	// answered the notification, its reply would arrive before the one
	// owed to the request and the assertion below would catch it.
	if err := r.Route(ctx, librelay.Frame{Type: "relay.from-the-future", Instance: "inst-a"}); err != nil {
		t.Fatalf("Route notification: %v", err)
	}
	if err := r.Route(ctx, librelay.Frame{Type: "relay.from-the-future", Instance: "inst-a", ID: "q1"}); err != nil {
		t.Fatalf("Route request: %v", err)
	}

	for range 20 {
		f, err := d.link(0).Recv(ctx)
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if !f.IsResponse() || strings.HasPrefix(f.ReplyTo, "hb-") {
			continue
		}
		if f.ReplyTo != "q1" || f.Type != librelay.TypeError {
			t.Fatalf("unexpected response %+v; only the request is owed one", f)
		}
		var e librelay.Error
		if err := f.DecodePayload(&e); err != nil {
			t.Fatalf("DecodePayload: %v", err)
		}
		if e.Code != librelay.CodeUnsupportedType {
			t.Fatalf("error code = %q, want %q", e.Code, librelay.CodeUnsupportedType)
		}
		return
	}
	t.Fatal("no unsupported-type reply")
}

// TestUnit_SendReachesTheRelayAndFailsFastWithout is the caller-facing
// contract: Send works when a link is held and refuses immediately when it is
// not.
func TestUnit_SendReachesTheRelayAndFailsFastWithout(t *testing.T) {
	t.Parallel()
	r := relaytest.New()
	defer r.Close()
	d := &relayDialer{relay: r}
	cfg := baseConfig()
	cfg.Dial = d.dial
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	// Before Start there is no link, and asking is not an error worth
	// waiting for.
	if err := c.Send(librelay.Frame{Type: librelay.TypeACPMessage, Instance: "inst-a"}); !errors.Is(err, relaylink.ErrNotConnected) {
		t.Fatalf("Send before Start = %v, want ErrNotConnected", err)
	}
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "connected", func() bool { return c.Status().State == relaylink.StateConnected })

	cargo, err := librelay.Frame{Type: librelay.TypeACPMessage, Instance: "inst-a", Session: "s1"}.
		WithPayload(json.RawMessage(`{"jsonrpc":"2.0"}`))
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	if err := c.Send(cargo); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	for range 20 {
		f, err := d.link(0).Recv(ctx)
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if f.Type == librelay.TypeACPMessage && f.Session == "s1" {
			if err := c.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if err := c.Send(cargo); !errors.Is(err, relaylink.ErrClosed) {
				t.Fatalf("Send after Close = %v, want ErrClosed", err)
			}
			return
		}
	}
	t.Fatal("the sent frame never reached the relay")
}

// TestUnit_LifecycleIsCleanAndLeaksNothing is the -race companion to the leak
// requirement: everything the connector started is gone once Close returns, and
// Close is idempotent.
func TestUnit_LifecycleIsCleanAndLeaksNothing(t *testing.T) {
	r := relaytest.New()
	d := &relayDialer{relay: r}
	cfg := baseConfig()
	cfg.Dial = d.dial

	before := runtime.NumGoroutine()
	for range 5 {
		c, err := relaylink.New(cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := c.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitFor(t, "connected", func() bool { return c.Status().State == relaylink.StateConnected })
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		if got := c.Status().State; got != relaylink.StateClosed {
			t.Fatalf("State after Close = %v", got)
		}
	}
	r.Close()

	// Goroutines exit asynchronously; the assertion is that the count
	// settles, not that it is instantaneous.
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutines: %d before, %d after five connect/close cycles", before, runtime.NumGoroutine())
}

// TestUnit_CloseWithoutStartIsSafe covers the connector a runtime constructs
// and never starts, which is what a configuration with no relay looks like.
func TestUnit_CloseWithoutStartIsSafe(t *testing.T) {
	t.Parallel()
	c, err := relaylink.New(baseConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Start(t.Context()); !errors.Is(err, relaylink.ErrClosed) {
		t.Fatalf("Start after Close = %v, want ErrClosed", err)
	}
}

// TestUnit_NewRequiresAnEndpointAndInstance keeps configuration failures at
// construction, where a caller can see them, rather than in a background loop
// where they would be invisible.
func TestUnit_NewRequiresAnEndpointAndInstance(t *testing.T) {
	t.Parallel()
	if _, err := relaylink.New(relaylink.Config{Instance: "i"}); err == nil {
		t.Fatal("New with no endpoint succeeded")
	}
	if _, err := relaylink.New(relaylink.Config{Endpoint: "e"}); err == nil {
		t.Fatal("New with no instance succeeded")
	}
	c, err := relaylink.New(relaylink.Config{Endpoint: "e", Instance: "i"})
	if err != nil {
		t.Fatalf("New with defaults: %v", err)
	}
	if got := c.Status().State; got != relaylink.StateIdle {
		t.Fatalf("State before Start = %v, want idle", got)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestUnit_RuntimeIsUnaffectedWhenTheRelayIsDown is the property the whole
// package is judged on. With a relay that never answers a dial, a caller that
// knows nothing about relays must be unaffected: constructing, starting,
// sending and querying all return promptly, work in flight completes on time,
// and the connector keeps retrying in the background.
func TestUnit_RuntimeIsUnaffectedWhenTheRelayIsDown(t *testing.T) {
	t.Parallel()
	var dials atomic.Int64
	cfg := baseConfig()
	cfg.Dial = func(context.Context, string, relaylink.Credentials) (net.Conn, error) {
		dials.Add(1)
		return nil, fmt.Errorf("relay is not there")
	}
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	// A "mission": work with a deadline of its own that must not slip
	// because a relay is missing.
	mission := make(chan int, 1)
	go func() {
		n := 0
		for range 1000 {
			n++
			// The mission touches the connector the way real code
			// would, and must never be parked by it.
			_ = c.Send(librelay.Frame{Type: librelay.TypeACPMessage, Instance: "inst-a"})
			_ = c.Status()
		}
		mission <- n
	}()

	start := time.Now()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Start blocked for %v with the relay down", elapsed)
	}

	select {
	case n := <-mission:
		if n != 1000 {
			t.Fatalf("mission completed %d of 1000 units", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a mission was blocked by an unreachable relay")
	}

	// Meanwhile the connector is quietly still trying, and says so only if
	// asked.
	//
	// Gate on the counter this then asserts, not on the dial count. A dial is
	// counted when it is ENTERED and an attempt is recorded when it FAILS, so
	// waiting for dials >= 3 admits a state where the third dial is still in
	// flight and Attempts is 2 — a flake that needs a loaded machine to show
	// itself, which means CI and not here.
	waitFor(t, "retries in the background", func() bool { return c.Status().Attempts >= 3 })
	st := c.Status()
	if st.State != relaylink.StateConnecting {
		t.Fatalf("State = %v with the relay down, want connecting", st.State)
	}
	if st.Attempts < 3 || st.LastError == nil {
		t.Fatalf("Status = %+v, want a retry count and the last dial error", st)
	}
	if st.Connections != 0 {
		t.Fatalf("Connections = %d, want 0", st.Connections)
	}
}

// TestUnit_HangingDialDoesNotBlockTheRuntime covers the dial that neither fails
// nor succeeds — a relay whose host accepts packets and answers nothing. Close
// must still return promptly, since a shutdown that waits on a hung relay is
// the same liability as a call path that does.
func TestUnit_HangingDialDoesNotBlockTheRuntime(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 1)
	cfg := baseConfig()
	cfg.Dial = func(ctx context.Context, _ string, _ relaylink.Credentials) (net.Conn, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done() // the relay is there and says nothing, forever
		return nil, ctx.Err()
	}
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-entered

	if err := c.Send(librelay.Frame{Type: librelay.TypeACPMessage, Instance: "inst-a"}); !errors.Is(err, relaylink.ErrNotConnected) {
		t.Fatalf("Send during a hanging dial = %v, want ErrNotConnected", err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Close blocked on a hanging dial")
	}
}

// TestUnit_HangingHandshakeIsBoundedAndRetried covers a peer that completes the
// TCP connection and never answers hello. The handshake deadline is what turns
// that from a permanent silent failure into another retry.
func TestUnit_HangingHandshakeIsBoundedAndRetried(t *testing.T) {
	t.Parallel()
	p := newRawRelay()
	defer p.closeAll()
	cfg := baseConfig()
	cfg.Dial = p.dial
	cfg.HandshakeTimeout = 50 * time.Millisecond
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	conn := p.accept(t)
	go func() { _, _ = io.Copy(io.Discard, conn) }() // read hello, answer nothing

	waitFor(t, "the stalled handshake to be abandoned and redialed", func() bool { return p.count() >= 2 })
	if got := c.Status().Connections; got != 0 {
		t.Fatalf("Connections = %d, want 0", got)
	}
	if c.Status().State == relaylink.StateFatal {
		t.Fatal("a stalled handshake must not be fatal")
	}
	// And a relay that eventually starts answering is picked up.
	conn2 := p.accept(t)
	welcome(t, conn2, librelay.ProtocolVersion)
	waitFor(t, "connection once the relay answers", func() bool { return c.Status().Connections == 1 })
}

// TestUnit_UnauthorizedIsFatal proves a refusal the connector cannot repair
// stops the retry loop instead of hammering the relay with a credential it has
// already been told is no good.
func TestUnit_UnauthorizedIsFatal(t *testing.T) {
	t.Parallel()
	p := newRawRelay()
	defer p.closeAll()
	cfg := baseConfig()
	cfg.Dial = p.dial
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	conn := p.accept(t)
	rd, w := librelay.NewReader(conn), librelay.NewWriter(conn)
	hello, err := rd.ReadFrame()
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if err := w.WriteFrame(librelay.NewError(hello, librelay.CodeUnauthorized, "device revoked")); err != nil {
		t.Fatalf("write error: %v", err)
	}

	waitFor(t, "the refusal to be final", func() bool { return c.Status().State == relaylink.StateFatal })
	if err := c.Status().LastError; !errors.Is(err, relaylink.ErrUnauthorized) {
		t.Fatalf("LastError = %v, want ErrUnauthorized", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := p.count(); got != 1 {
		t.Fatalf("dials = %d after an unauthorized refusal, want 1", got)
	}
}
