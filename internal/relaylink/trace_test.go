package relaylink_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/relaylink"
	"github.com/contenox/contenox/librelay"
	"github.com/contenox/contenox/libtracker"
)

// routedTrace is what the handler observed about one frame: the correlation key
// the connector put in its context, and whether the key was there at all. The
// two are recorded separately because "absent" and "empty string" are different
// answers — a context carrying "" would make every untraced record claim a
// trace field it cannot join on.
type routedTrace struct {
	frame   librelay.Frame
	traceID string
	present bool
}

// TestUnit_FrameTraceReachesTheHandlerContext is the correlation contract seen
// from the runtime end, and it covers the three states a frame can be in.
//
// A traced frame puts its key in the context the handler is called with, which
// is what makes every tracker record opened below it report trace_id without a
// single Start call site knowing a relay was involved.
//
// An untraced frame leaves the context alone. That is the ordinary state of
// machine-initiated traffic and of anything from a relay built before the
// field existed, so it must not be papered over with a key minted here — one
// invented at this layer would name an action this process never saw.
//
// A trace that is over-long or outside the alphabet never reaches the handler
// at all. It is refused by the codec as an unaddressable frame, counted, and
// the link carries on: the value is peer-supplied text bound for a log field,
// so the bound has to hold before it is anywhere it could be read.
func TestUnit_FrameTraceReachesTheHandlerContext(t *testing.T) {
	t.Parallel()
	p := newRawRelay()
	defer p.closeAll()

	routed := make(chan routedTrace, 8)
	cfg := baseConfig()
	cfg.Dial = p.dial
	cfg.Handler = func(ctx context.Context, f librelay.Frame) {
		id, ok := ctx.Value(libtracker.ContextKeyTraceID).(string)
		routed <- routedTrace{frame: f, traceID: id, present: ok}
	}
	// A heartbeat arriving mid-assertion would be a second reason for the
	// connection to end; liveness is not what is under test.
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

	trace := librelay.NewTraceID()
	if err := w.WriteFrame(librelay.Frame{
		Type: librelay.TypeACPMessage, Instance: "inst-a", Session: "s-traced", Trace: trace,
	}); err != nil {
		t.Fatalf("write traced frame: %v", err)
	}
	got := recvRouted(t, routed)
	if !got.present || got.traceID != trace {
		t.Fatalf("handler context trace = %q (present %v), want %q", got.traceID, got.present, trace)
	}
	if got.frame.Trace != trace {
		t.Fatalf("routed frame trace = %q, want %q", got.frame.Trace, trace)
	}

	if err := w.WriteFrame(librelay.Frame{
		Type: librelay.TypeACPMessage, Instance: "inst-a", Session: "s-plain",
	}); err != nil {
		t.Fatalf("write untraced frame: %v", err)
	}
	got = recvRouted(t, routed)
	if got.present {
		t.Fatalf("an untraced frame put %q in the handler's context; absent must stay absent", got.traceID)
	}

	// Written raw: WriteFrame refuses both of these, which is the point —
	// only a peer that built the value some other way can send one.
	before := c.Status().BadFrames
	for _, bad := range []string{
		`{"type":"acp.message","instance":"inst-a","session":"s-bad","trace":"tr- not an id"}`,
		`{"type":"acp.message","instance":"inst-a","session":"s-bad","trace":"` +
			longTrace(librelay.MaxTraceBytes+1) + `"}`,
	} {
		if _, err := conn.Write([]byte(bad + "\n")); err != nil {
			t.Fatalf("write %q: %v", bad, err)
		}
	}
	if err := w.WriteFrame(librelay.Frame{
		Type: librelay.TypeACPMessage, Instance: "inst-a", Session: "s-after",
	}); err != nil {
		t.Fatalf("write sentinel frame: %v", err)
	}
	got = recvRouted(t, routed)
	if got.frame.Session != "s-after" {
		t.Fatalf("a frame with an unloggable trace was routed: %+v", got.frame)
	}
	if n := c.Status().BadFrames - before; n != 2 {
		t.Fatalf("BadFrames rose by %d, want 2", n)
	}
	if n := c.Status().Connections; n != 1 {
		t.Fatalf("Connections = %d: a bad trace must not end the connection", n)
	}
}

// recvRouted takes the next observation or fails the test, so a missing frame
// is a named failure rather than a hung run.
func recvRouted(t *testing.T, ch <-chan routedTrace) routedTrace {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(testTimeout):
		t.Fatal("no frame reached the handler")
		return routedTrace{}
	}
}

// longTrace returns n acceptable trace bytes, which is a value the alphabet
// admits and the ceiling does not.
func longTrace(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
