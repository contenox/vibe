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

const testTimeout = 10 * time.Second

var fastBackoff = relaylink.Backoff{
	Initial:    2 * time.Millisecond,
	Max:        20 * time.Millisecond,
	Factor:     2,
	ResetAfter: 5 * time.Millisecond,
}

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

// TestUnit_HandshakeNegotiatesAndHoldsTheConnection checks hello is sent with
// this build's version and an accepted welcome binds the instance.
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

// TestUnit_HeartbeatProbesArePairedWithAcks checks heartbeat probes are on
// the wire and correlated by ID.
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
	if got := c.Status().Connections; got != 1 {
		t.Fatalf("Connections = %d after healthy heartbeats, want 1", got)
	}
}

// TestUnit_ReconnectsAfterTheRelayIsKilled checks the connector redials on
// its own after the link is dropped with no close frame.
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

// TestUnit_SilentRelayIsDetectedByHeartbeat checks a peer that holds the
// connection open but answers nothing is redialed via heartbeat timeout.
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

	// Drained so the connector's own writes never block; the peer answers
	// nothing regardless.
	go func() { _, _ = io.Copy(io.Discard, conn) }()

	waitFor(t, "the silent peer to be redialed", func() bool { return p.count() >= 2 })
	if err := c.Status().LastError; !errors.Is(err, relaylink.ErrHeartbeatTimeout) {
		t.Fatalf("LastError = %v, want a heartbeat timeout", err)
	}

	conn2 := p.accept(t)
	welcome(t, conn2, librelay.ProtocolVersion)
	waitFor(t, "reconnection", func() bool { return c.Status().Connections >= 2 })
}

// TestUnit_MalformedFrameDoesNotDropTheConnection checks a garbled line is
// dropped per-frame without ending the connection.
func TestUnit_MalformedFrameDoesNotDropTheConnection(t *testing.T) {
	t.Parallel()
	p := newRawRelay()
	defer p.closeAll()

	routed := make(chan librelay.Frame, 4)
	cfg := baseConfig()
	cfg.Dial = p.dial
	cfg.Handler = func(_ context.Context, f librelay.Frame) { routed <- f }
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

	// Three kinds the reader must survive: broken JSON, a non-addressable
	// object, and a frame whose session names no instance.
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

// TestUnit_OversizedFrameEndsTheConnection checks an oversized frame ends and
// redials the connection rather than resynchronizing.
func TestUnit_OversizedFrameEndsTheConnection(t *testing.T) {
	t.Parallel()
	p := newRawRelay()
	defer p.closeAll()
	cfg := baseConfig()
	cfg.Dial = p.dial
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

// TestUnit_UnsupportedVersionIsRefusedAndFatal checks the connector refuses
// fatally on a welcome naming an unsupported version.
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
	time.Sleep(50 * time.Millisecond)
	if got := p.count(); got != 1 {
		t.Fatalf("dials = %d after a fatal refusal, want 1", got)
	}
}

// TestUnit_UnknownControlTypeIsAnsweredWithUnsupported checks an unknown
// control request gets exactly one error reply and a notification gets none.
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
	// Notification first, then request: a wrongly-answered notification
	// would arrive before the reply owed to the request.
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

// TestUnit_SendReachesTheRelayAndFailsFastWithout checks Send works when a
// link is held and refuses immediately when it is not.
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

// TestUnit_LifecycleIsCleanAndLeaksNothing checks every goroutine the
// connector started is gone once Close returns, and Close is idempotent.
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

	// Goroutines exit asynchronously; polled until the count settles.
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutines: %d before, %d after five connect/close cycles", before, runtime.NumGoroutine())
}

// TestUnit_CloseWithoutStartIsSafe checks Close is safe on a connector that
// was never started.
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

// TestUnit_NewRequiresAnEndpointAndInstance checks New fails at construction
// when Endpoint or Instance is missing.
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

// TestUnit_RuntimeIsUnaffectedWhenTheRelayIsDown checks that with a relay
// that never answers a dial, calls return promptly and work in flight
// completes on time while the connector keeps retrying in the background.
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

	mission := make(chan int, 1)
	go func() {
		n := 0
		for range 1000 {
			n++
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

	// Gate on Attempts, not dial count: a dial is counted on entry but an
	// attempt only on failure, so gating on dials risks the third still
	// being in flight.
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

// TestUnit_HangingDialDoesNotBlockTheRuntime checks Close returns promptly
// even while a dial is hanging forever.
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

// TestUnit_HangingHandshakeIsBoundedAndRetried checks a peer that never
// answers hello is abandoned and redialed once the handshake deadline
// expires.
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
	conn2 := p.accept(t)
	welcome(t, conn2, librelay.ProtocolVersion)
	waitFor(t, "connection once the relay answers", func() bool { return c.Status().Connections == 1 })
}

// TestUnit_UnauthorizedIsFatal checks an unauthorized refusal stops the
// retry loop instead of hammering the relay.
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

func TestUnit_OnConnectRunsOnEveryConnectionAndCanSendThroughIt(t *testing.T) {
	t.Parallel()
	r := relaytest.New()
	defer r.Close()
	d := &relayDialer{relay: r}

	var conn *relaylink.Connector
	var connects atomic.Int32
	var sendErr atomic.Value
	cfg := baseConfig()
	cfg.Dial = d.dial
	cfg.OnConnect = func(context.Context) {
		n := connects.Add(1)
		f, err := librelay.Frame{Type: librelay.TypeACPMessage, Instance: "inst-a", Session: fmt.Sprintf("reconnect-%d", n)}.
			WithPayload(json.RawMessage(`{}`))
		if err == nil {
			err = conn.Send(f)
		}
		if err != nil {
			sendErr.Store(err)
		}
	}
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	conn = c
	defer c.Close()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, "the first connection's hook", func() bool { return connects.Load() == 1 })
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	if !sawSession(t, ctx, d.link(0), "reconnect-1") {
		t.Fatal("what OnConnect sent on the first connection never reached the relay")
	}

	d.link(0).Drop()
	waitFor(t, "the redial's hook", func() bool { return connects.Load() >= 2 })
	waitFor(t, "a second link", func() bool { return d.link(1) != nil })
	if !sawSession(t, ctx, d.link(1), "reconnect-2") {
		t.Fatal("what OnConnect sent after the redial never reached the relay")
	}
	if err, _ := sendErr.Load().(error); err != nil {
		t.Fatalf("OnConnect could not send on its own connection: %v", err)
	}
}

func sawSession(t *testing.T, ctx context.Context, l *relaytest.Link, session string) bool {
	t.Helper()
	for range 20 {
		f, err := l.Recv(ctx)
		if err != nil {
			return false
		}
		if f.Session == session {
			return true
		}
	}
	return false
}
