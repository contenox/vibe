package relaylink_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/relaylink"
	"github.com/contenox/contenox/librelay"
	"github.com/contenox/contenox/libtracker"
)

type routedTrace struct {
	frame   librelay.Frame
	traceID string
	present bool
}

// TestUnit_FrameTraceReachesTheHandlerContext checks a frame's trace key
// reaches the handler's context when present, stays absent when not, and a
// malformed trace is dropped as a bad frame without ending the connection.
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

	// Written raw: WriteFrame refuses both, so only a peer bypassing it
	// could send one.
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

func longTrace(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
