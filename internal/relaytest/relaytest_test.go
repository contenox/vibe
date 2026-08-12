package relaytest_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/relaytest"
	"github.com/contenox/contenox/librelay"
)

const testTimeout = 10 * time.Second

// TestUnit_RelayDoubleRoundTripsBothDirections checks frames cross the
// double in both directions with the envelope intact.
func TestUnit_RelayDoubleRoundTripsBothDirections(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := relaytest.New()
	defer r.Close()
	link := r.Dial()
	w, rd := librelay.NewWriter(link.Conn()), librelay.NewReader(link.Conn())

	hello, err := librelay.Frame{Type: librelay.TypeHello, Instance: "inst-a", ID: "1"}.
		WithPayload(librelay.Hello{ProtocolVersion: librelay.ProtocolVersion, Instance: "inst-a", Agent: "contenox/test"})
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	if err := w.WriteFrame(hello); err != nil {
		t.Fatalf("WriteFrame hello: %v", err)
	}
	got := mustRead(t, rd)
	if got.Type != librelay.TypeWelcome || got.ReplyTo != "1" {
		t.Fatalf("welcome = %+v", got)
	}
	var welcome librelay.Welcome
	if err := got.DecodePayload(&welcome); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if welcome.ProtocolVersion != librelay.ProtocolVersion {
		t.Fatalf("negotiated version = %d, want %d", welcome.ProtocolVersion, librelay.ProtocolVersion)
	}
	if link.Instance() != "inst-a" {
		t.Fatalf("link bound instance %q, want inst-a", link.Instance())
	}

	cargo, err := librelay.Frame{Type: librelay.TypeACPMessage, Instance: "inst-a", Session: "sess-1", ID: "9"}.
		WithPayload(json.RawMessage(`{"jsonrpc":"2.0","method":"session/request_permission"}`))
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	if err := r.Route(ctx, cargo); err != nil {
		t.Fatalf("Route: %v", err)
	}
	got = mustRead(t, rd)
	if got.Type != librelay.TypeACPMessage || got.Session != "sess-1" || got.ID != "9" {
		t.Fatalf("routed frame = %+v", got)
	}
	if string(got.Payload) != `{"jsonrpc":"2.0","method":"session/request_permission"}` {
		t.Fatalf("payload = %s", got.Payload)
	}

	answer := librelay.Frame{Type: librelay.TypeACPMessage, Instance: "inst-a", Session: "sess-1", ReplyTo: "9"}
	answer, _ = answer.WithPayload(json.RawMessage(`{"jsonrpc":"2.0","result":{"outcome":"selected"}}`))
	if err := w.WriteFrame(answer); err != nil {
		t.Fatalf("WriteFrame answer: %v", err)
	}
	for {
		f, err := link.Recv(ctx)
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if f.Type == librelay.TypeHello {
			continue
		}
		if f.ReplyTo != "9" || f.Session != "sess-1" {
			t.Fatalf("received frame = %+v", f)
		}
		break
	}
}

// TestUnit_RelayDoubleRoutesByInstance checks two instances on one relay
// never see each other's traffic, routed by envelope alone.
func TestUnit_RelayDoubleRoutesByInstance(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := relaytest.New()
	defer r.Close()
	a, b := r.Dial(), r.Dial()
	sayHello(t, a, "inst-a")
	sayHello(t, b, "inst-b")

	f := librelay.Frame{Type: librelay.TypeACPMessage, Instance: "inst-b", Session: "s1"}
	if err := r.Route(ctx, f); err != nil {
		t.Fatalf("Route: %v", err)
	}
	got := mustRead(t, librelay.NewReader(b.Conn()))
	if got.Instance != "inst-b" {
		t.Fatalf("frame landed on %q", got.Instance)
	}

	if err := r.Route(ctx, librelay.Frame{Type: librelay.TypeACPMessage, Instance: "inst-c"}); err == nil {
		t.Fatal("Route to an unbound instance succeeded")
	}

	_ = a.Conn().SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := librelay.NewReader(a.Conn()).ReadFrame(); err == nil {
		t.Fatal("a received a frame addressed to b")
	}
}

// TestUnit_RelayDoubleHeartbeatCorrelates checks heartbeat probes are
// answered with correlated acks.
func TestUnit_RelayDoubleHeartbeatCorrelates(t *testing.T) {
	t.Parallel()
	r := relaytest.New()
	defer r.Close()
	link := r.Dial()
	rd := sayHello(t, link, "inst-a")

	w := librelay.NewWriter(link.Conn())
	for _, id := range []string{"hb-1", "hb-2", "hb-3"} {
		if err := w.WriteFrame(librelay.Frame{Type: librelay.TypeHeartbeat, Instance: "inst-a", ID: id}); err != nil {
			t.Fatalf("WriteFrame heartbeat %s: %v", id, err)
		}
		got := mustRead(t, rd)
		if got.Type != librelay.TypeAck || got.ReplyTo != id {
			t.Fatalf("ack for %s = %+v", id, got)
		}
	}
}

// TestUnit_RelayDoubleAnswersUnknownTypes checks an unknown control request
// gets an error reply, a notification gets none, and unknown cargo is
// routed untouched.
func TestUnit_RelayDoubleAnswersUnknownTypes(t *testing.T) {
	t.Parallel()
	r := relaytest.New()
	defer r.Close()
	link := r.Dial()
	rd := sayHello(t, link, "inst-a")
	w := librelay.NewWriter(link.Conn())

	if err := w.WriteFrame(librelay.Frame{Type: "relay.invented.in.2027", Instance: "inst-a", ID: "z1"}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got := mustRead(t, rd)
	if got.Type != librelay.TypeError || got.ReplyTo != "z1" {
		t.Fatalf("reply = %+v", got)
	}
	var e librelay.Error
	if err := got.DecodePayload(&e); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if e.Code != librelay.CodeUnsupportedType {
		t.Fatalf("code = %q, want %q", e.Code, librelay.CodeUnsupportedType)
	}

	if err := w.WriteFrame(librelay.Frame{Type: "relay.invented.in.2027", Instance: "inst-a"}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if err := w.WriteFrame(librelay.Frame{Type: librelay.TypeHeartbeat, Instance: "inst-a", ID: "hb"}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got = mustRead(t, rd)
	if got.Type != librelay.TypeAck || got.ReplyTo != "hb" {
		t.Fatalf("expected only an ack, got %+v", got)
	}

	if err := w.WriteFrame(librelay.Frame{Type: "acp.invented.in.2027", Instance: "inst-a", Session: "s1", ID: "z2"}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	queued, err := link.Recv(ctx)
	for err == nil && queued.Type != "acp.invented.in.2027" {
		queued, err = link.Recv(ctx)
	}
	if err != nil {
		t.Fatalf("unknown cargo was not queued for routing: %v", err)
	}
	_ = link.Conn().SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if f, err := rd.ReadFrame(); err == nil {
		t.Fatalf("relay answered unknown cargo with %+v", f)
	}
}

// TestUnit_RelayDoubleDropIsVisible checks Drop ends reads immediately, is
// idempotent, and leaves the double usable for assertions afterward.
func TestUnit_RelayDoubleDropIsVisible(t *testing.T) {
	t.Parallel()
	r := relaytest.New()
	defer r.Close()
	link := r.Dial()
	rd := sayHello(t, link, "inst-a")

	link.Drop()
	if _, err := rd.ReadFrame(); err == nil {
		t.Fatal("reads succeeded after Drop")
	}
	link.Drop()

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	for {
		f, err := link.Recv(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv after Drop: %v", err)
		}
		if f.Type != librelay.TypeHello {
			t.Fatalf("unexpected buffered frame %+v", f)
		}
	}
}

// TestUnit_RelayDoubleNoAutoControl checks NoAutoControl leaves hello and
// heartbeat unanswered.
func TestUnit_RelayDoubleNoAutoControl(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := relaytest.New(relaytest.NoAutoControl())
	defer r.Close()
	link := r.Dial()
	w := librelay.NewWriter(link.Conn())
	if err := w.WriteFrame(librelay.Frame{Type: librelay.TypeHeartbeat, Instance: "inst-a", ID: "hb"}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := link.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if got.ID != "hb" {
		t.Fatalf("received %+v", got)
	}
	_ = link.Conn().SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := librelay.NewReader(link.Conn()).ReadFrame(); err == nil {
		t.Fatal("relay answered with auto-control disabled")
	}
}

// TestUnit_RelayDoubleRecordsMalformedFrames checks a malformed frame is
// recorded in Errors without ending the link.
func TestUnit_RelayDoubleRecordsMalformedFrames(t *testing.T) {
	t.Parallel()
	r := relaytest.New()
	defer r.Close()
	link := r.Dial()

	if _, err := link.Conn().Write([]byte("{\"type\":\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sayHello(t, link, "inst-a")
	if len(link.Errors()) == 0 {
		t.Fatal("malformed frame was not recorded")
	}
}

func sayHello(t *testing.T, link *relaytest.Link, instance string) *librelay.Reader {
	t.Helper()
	w, rd := librelay.NewWriter(link.Conn()), librelay.NewReader(link.Conn())
	f, err := librelay.Frame{Type: librelay.TypeHello, Instance: instance, ID: "hello-" + instance}.
		WithPayload(librelay.Hello{ProtocolVersion: librelay.ProtocolVersion, Instance: instance})
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	if err := w.WriteFrame(f); err != nil {
		t.Fatalf("WriteFrame hello: %v", err)
	}
	got := mustRead(t, rd)
	if got.Type != librelay.TypeWelcome {
		t.Fatalf("hello answered with %+v", got)
	}
	return rd
}

func mustRead(t *testing.T, rd *librelay.Reader) librelay.Frame {
	t.Helper()
	f, err := rd.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	return f
}

// TestUnit_RelayDoubleSignsTheWelcome checks the double signs a welcome
// that verifies against its public key.
func TestUnit_RelayDoubleSignsTheWelcome(t *testing.T) {
	t.Parallel()
	r := relaytest.New()
	defer r.Close()
	link := r.Dial()
	w, rd := librelay.NewWriter(link.Conn()), librelay.NewReader(link.Conn())

	nonce, err := librelay.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	hello, err := librelay.Frame{Type: librelay.TypeHello, Instance: "inst-a", ID: "1"}.
		WithPayload(librelay.Hello{ProtocolVersion: librelay.ProtocolVersion, Instance: "inst-a", Nonce: nonce})
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	if err := w.WriteFrame(hello); err != nil {
		t.Fatalf("WriteFrame hello: %v", err)
	}
	var wel librelay.Welcome
	if err := mustRead(t, rd).DecodePayload(&wel); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	pub, err := librelay.ParsePublicKey(r.PublicKey())
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	if err := librelay.VerifyWelcome(pub, nonce, wel.ProtocolVersion, "inst-a", wel.Signature); err != nil {
		t.Fatalf("VerifyWelcome: %v", err)
	}
}

// TestUnit_RelayDoubleCanRefuseToSign checks a NoSignature relay answers
// welcome unsigned.
func TestUnit_RelayDoubleCanRefuseToSign(t *testing.T) {
	t.Parallel()
	r := relaytest.New(relaytest.NoSignature())
	defer r.Close()
	link := r.Dial()
	w, rd := librelay.NewWriter(link.Conn()), librelay.NewReader(link.Conn())

	nonce, _ := librelay.NewNonce()
	hello, err := librelay.Frame{Type: librelay.TypeHello, Instance: "inst-a", ID: "1"}.
		WithPayload(librelay.Hello{ProtocolVersion: librelay.ProtocolVersion, Instance: "inst-a", Nonce: nonce})
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	if err := w.WriteFrame(hello); err != nil {
		t.Fatalf("WriteFrame hello: %v", err)
	}
	var wel librelay.Welcome
	if err := mustRead(t, rd).DecodePayload(&wel); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if len(wel.Signature) != 0 {
		t.Fatal("NoSignature relay signed anyway")
	}
}

// TestUnit_RelayDoubleSignsOnlyWhenChallenged checks a hello with no nonce
// gets an unsigned welcome.
func TestUnit_RelayDoubleSignsOnlyWhenChallenged(t *testing.T) {
	t.Parallel()
	r := relaytest.New()
	defer r.Close()
	link := r.Dial()
	w, rd := librelay.NewWriter(link.Conn()), librelay.NewReader(link.Conn())

	hello, err := librelay.Frame{Type: librelay.TypeHello, Instance: "inst-a", ID: "1"}.
		WithPayload(librelay.Hello{ProtocolVersion: librelay.ProtocolVersion, Instance: "inst-a"})
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	if err := w.WriteFrame(hello); err != nil {
		t.Fatalf("WriteFrame hello: %v", err)
	}
	var wel librelay.Welcome
	if err := mustRead(t, rd).DecodePayload(&wel); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if len(wel.Signature) != 0 {
		t.Fatal("relay signed an unchallenged hello")
	}
}
