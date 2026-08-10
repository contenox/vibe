package relayacp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/relayacp"
	"github.com/contenox/contenox/internal/relaylink"
	"github.com/contenox/contenox/internal/relaytest"
	"github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/librelay"
)

// testTimeout keeps a failing assertion a failure rather than a hung `go test`.
const testTimeout = 10 * time.Second

// testInstance is the runtime identity every harness pairs under.
const testInstance = "inst-a"

// echoMethod is the extension method the test agent answers. An extension
// method rather than a real ACP call: it exercises libacp's whole read,
// dispatch and write path while letting a test state a request and a response
// in one line each.
const echoMethod = "_relayacp/whoami"

// A tunnel's Handle method value IS a connector handler. Asserting it here puts
// the seam in front of the compiler rather than leaving it to the wiring, which
// is the one place a signature drift would not be caught until runtime.
var _ relaylink.Handler = (*relayacp.Tunnel)(nil).Handle

// identityFactory builds an agent that answers [echoMethod] with the identity
// of the connection serving it, alongside the request's own parameters. Two
// attachments that shared a connection would answer with one identity, which is
// what makes crossed streams visible rather than merely unlikely.
func identityFactory(counter *atomic.Int64) libacp.AgentFactory {
	return func(c *libacp.AgentSideConnection) libacp.Agent {
		id := fmt.Sprintf("agent-%d", counter.Add(1))
		c.SetExtRequestHandler(func(_ context.Context, _ string, params json.RawMessage) (json.RawMessage, *libacp.Error) {
			if len(params) == 0 {
				params = json.RawMessage(`null`)
			}
			return json.RawMessage(fmt.Sprintf(`{"agent":%q,"params":%s}`, id, params)), nil
		})
		return libacp.UnimplementedAgent{}
	}
}

// harness wires a tunnel to a relaytest double through a real connector, so
// every assertion below is made against frames that actually crossed a
// connection rather than against a hand-rolled stand-in for one.
type harness struct {
	t      *testing.T
	relay  *relaytest.Relay
	link   *relaytest.Link
	conn   *relaylink.Connector
	tunnel *relayacp.Tunnel

	closeOnce sync.Once
	stopPump  context.CancelFunc

	mu   sync.Mutex
	seen []librelay.Frame
	byID map[string]chan librelay.Frame
}

// close releases everything the harness owns and waits for it. It is
// idempotent, so a test that tears down early and the registered cleanup do not
// have to coordinate.
func (h *harness) close() {
	h.closeOnce.Do(func() {
		h.stopPump()
		h.tunnel.Close()
		_ = h.conn.Close()
		h.relay.Close()
	})
}

// newHarness returns a connected harness. tune adjusts the tunnel's bounds
// before construction; it may be nil.
//
// The send closure late-binds the connector, which is the same ordering the CLI
// wiring relies on: the connector needs the tunnel's handler to be built and
// the tunnel needs somewhere to send, and Handle only ever runs on a read loop
// that does not exist until Start.
func newHarness(t *testing.T, factory libacp.AgentFactory, tune func(*relayacp.Config)) *harness {
	t.Helper()
	h := &harness{t: t, relay: relaytest.New(), byID: map[string]chan librelay.Frame{}}
	h.link = h.relay.Dial()

	cfg := relayacp.Config{Instance: testInstance, Factory: factory}
	cfg.Send = func(f librelay.Frame) error { return h.conn.Send(f) }
	if tune != nil {
		tune(&cfg)
	}
	tunnel, err := relayacp.New(cfg)
	if err != nil {
		t.Fatalf("relayacp.New: %v", err)
	}
	h.tunnel = tunnel

	conn, err := relaylink.New(relaylink.Config{
		Endpoint: "relay.invalid:443",
		Instance: testInstance,
		Agent:    "contenox/test",
		Handler:  tunnel.Handle,
		Dial: func(context.Context, string, relaylink.Credentials) (net.Conn, error) {
			return h.link.Conn(), nil
		},
	})
	if err != nil {
		t.Fatalf("relaylink.New: %v", err)
	}
	h.conn = conn
	if err := conn.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	pumpCtx, stopPump := context.WithCancel(context.Background())
	h.stopPump = stopPump
	go h.pump(pumpCtx)
	t.Cleanup(h.close)

	waitFor(t, "connected", func() bool { return conn.Status().State == relaylink.StateConnected })
	return h
}

// pump drains what the connector sent, keeping cargo and discarding control
// traffic: heartbeats are the connection's business and not the tunnel's.
func (h *harness) pump(ctx context.Context) {
	for {
		f, err := h.link.Recv(ctx)
		if err != nil {
			return
		}
		if librelay.IsControl(f.Type) {
			continue
		}
		h.mu.Lock()
		h.seen = append(h.seen, f)
		ch := h.channelLocked(f.Session)
		h.mu.Unlock()
		select {
		case ch <- f:
		default:
		}
	}
}

func (h *harness) channelLocked(session string) chan librelay.Frame {
	ch := h.byID[session]
	if ch == nil {
		ch = make(chan librelay.Frame, 64)
		h.byID[session] = ch
	}
	return ch
}

// send delivers one ACP message to the tunnel as the relay would, stamped with
// the attachment identifier the relay assigned.
func (h *harness) send(session, message string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.t.Context(), testTimeout)
	defer cancel()
	f := librelay.Frame{
		Type:     librelay.TypeACPMessage,
		Instance: testInstance,
		Session:  session,
		Payload:  json.RawMessage(message),
	}
	if err := h.link.Send(ctx, f); err != nil {
		h.t.Fatalf("route to %s: %v", session, err)
	}
}

// ask sends a request and returns the response frame for the same attachment.
func (h *harness) ask(session string, id int, from string) librelay.Frame {
	h.t.Helper()
	h.send(session, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{"from":%q}}`, id, echoMethod, from))
	return h.recv(session)
}

// recv returns the next frame the tunnel emitted for session.
func (h *harness) recv(session string) librelay.Frame {
	h.t.Helper()
	h.mu.Lock()
	ch := h.channelLocked(session)
	h.mu.Unlock()
	select {
	case f := <-ch:
		return f
	case <-time.After(testTimeout):
		h.t.Fatalf("timed out waiting for a frame on attachment %s", session)
		return librelay.Frame{}
	}
}

// quiet reports whether the tunnel emitted nothing for session within d.
func (h *harness) quiet(session string, d time.Duration) bool {
	h.mu.Lock()
	ch := h.channelLocked(session)
	h.mu.Unlock()
	select {
	case f := <-ch:
		h.t.Errorf("attachment %s emitted %s", session, f.Payload)
		return false
	case <-time.After(d):
		return true
	}
}

// emitted returns every cargo frame the connector sent so far.
func (h *harness) emitted() []librelay.Frame {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]librelay.Frame(nil), h.seen...)
}

// waitFor polls cond until it holds or the deadline passes. Polling rather than
// signalling: the tunnel exposes no "wait until attached" call, because nothing
// in the runtime is allowed to sequence on a relay.
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

// decodeEcho pulls the agent identity and the echoed parameters out of one
// response payload.
func decodeEcho(t *testing.T, payload json.RawMessage) (agent, from string) {
	t.Helper()
	var msg struct {
		Result struct {
			Agent  string `json:"agent"`
			Params struct {
				From string `json:"from"`
			} `json:"params"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatalf("decode %s: %v", payload, err)
	}
	if len(msg.Error) > 0 {
		t.Fatalf("agent answered with an error: %s", payload)
	}
	return msg.Result.Agent, msg.Result.Params.From
}

// TestUnit_TwoAttachmentsAreTwoIndependentACPSessions is the property the whole
// package exists for: two clients attached to one instance are served by two
// connections and their traffic never crosses.
//
// The requests are interleaved rather than run in sequence, so a tunnel that
// served both attachments from one connection would have to get lucky to pass.
func TestUnit_TwoAttachmentsAreTwoIndependentACPSessions(t *testing.T) {
	t.Parallel()
	var counter atomic.Int64
	h := newHarness(t, identityFactory(&counter), nil)

	first := h.ask("att-a", 1, "a")
	second := h.ask("att-b", 1, "b")
	third := h.ask("att-a", 2, "a-again")

	agentA, fromA := decodeEcho(t, first.Payload)
	agentB, fromB := decodeEcho(t, second.Payload)
	agentA2, fromA2 := decodeEcho(t, third.Payload)

	if fromA != "a" || fromB != "b" || fromA2 != "a-again" {
		t.Fatalf("parameters crossed: %q, %q, %q", fromA, fromB, fromA2)
	}
	if agentA == agentB {
		t.Fatalf("both attachments were served by %s; they must be separate connections", agentA)
	}
	if agentA != agentA2 {
		t.Fatalf("attachment att-a was served by %s and then by %s; an attachment is one connection", agentA, agentA2)
	}
	if got := h.tunnel.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
}

// TestUnit_EveryOutboundFrameEchoesItsSessionAndCarriesNothingElse pins the
// two-end contract. A frame returning with an empty or altered Session names no
// attachment at the relay and is dropped there, so the client would wait
// forever on an answer that was produced and discarded.
//
// It pins the rest of the envelope too, and each field for its own reason.
// Frame-level correlation ids are absent because requests and responses are
// correlated inside the JSON-RPC message, and a frame-level id would oblige an
// answer this tunnel never produces. The cursor is absent because a relay built
// against a librelay that predates it destroys the field in transit, so the
// tunnel must neither set it nor need it. A payload spanning lines is refused
// because it would be two messages a client's reader cannot split back apart.
func TestUnit_EveryOutboundFrameEchoesItsSessionAndCarriesNothingElse(t *testing.T) {
	t.Parallel()
	var counter atomic.Int64
	h := newHarness(t, identityFactory(&counter), nil)

	want := map[string]int{"att-a": 0, "att-b": 0, "att-c": 0}
	for session := range want {
		for i := range 3 {
			h.ask(session, i+1, session)
			want[session]++
		}
	}

	got := map[string]int{}
	for _, f := range h.emitted() {
		if f.Type != librelay.TypeACPMessage {
			t.Fatalf("tunnel emitted a %q frame; it carries ACP and nothing else", f.Type)
		}
		if _, ok := want[f.Session]; !ok {
			t.Fatalf("frame carried session %q, which no attachment holds", f.Session)
		}
		if f.Instance != testInstance {
			t.Fatalf("frame carried instance %q, want %q", f.Instance, testInstance)
		}
		if f.ID != "" || f.ReplyTo != "" {
			t.Fatalf("frame carried envelope correlation id=%q re=%q", f.ID, f.ReplyTo)
		}
		if f.Seq != 0 {
			t.Fatalf("frame carried cursor %d; resumption must not be a prerequisite", f.Seq)
		}
		if strings.Contains(string(f.Payload), "\n") {
			t.Fatalf("frame payload spans lines: %s", f.Payload)
		}
		if !json.Valid(f.Payload) {
			t.Fatalf("frame payload is not one JSON value: %s", f.Payload)
		}
		got[f.Session]++
	}
	for session, n := range want {
		if got[session] != n {
			t.Fatalf("attachment %s received %d frames, want %d", session, got[session], n)
		}
	}
}

// TestUnit_SlowAttachmentIsDroppedAndDoesNotBlockOthers covers the backpressure
// policy. Handle runs on the connector's read loop, so a wedged attachment must
// cost itself and nothing else: blocking there would stop the connector
// answering heartbeats, and the relay would take the whole instance offline
// because one client stopped reading.
//
// The wedge is reproduced exactly rather than simulated: the first attachment's
// agent never finishes being built, so its connection never starts reading and
// its queue is the only thing holding what arrives. Past the bound the
// attachment is dropped, the healthy one answers as if nothing had happened —
// which is the whole assertion, since it proves the read loop never blocked —
// and the dropped one unwinds on its own once it is unwedged.
func TestUnit_SlowAttachmentIsDroppedAndDoesNotBlockOthers(t *testing.T) {
	t.Parallel()
	var counter atomic.Int64
	inner := identityFactory(&counter)

	var once sync.Once
	wedged := make(chan struct{})
	release := make(chan struct{})
	factory := func(c *libacp.AgentSideConnection) libacp.Agent {
		mine := false
		once.Do(func() { mine = true })
		if mine {
			close(wedged)
			<-release
		}
		return inner(c)
	}

	h := newHarness(t, factory, func(cfg *relayacp.Config) { cfg.Queue = 1 })

	h.send("att-slow", fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":{}}`, echoMethod))
	<-wedged

	for i := range 10 {
		h.send("att-slow", fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{}}`, i+2, echoMethod))
	}

	if _, from := decodeEcho(t, h.ask("att-live", 1, "live").Payload); from != "live" {
		t.Fatalf("healthy attachment answered %q", from)
	}
	if !h.quiet("att-slow", 100*time.Millisecond) {
		t.Fatal("the dropped attachment produced traffic")
	}

	close(release)
	waitFor(t, "the dropped attachment to be reclaimed", func() bool { return h.tunnel.Len() == 1 })
}

// TestUnit_CloseJoinsEveryAttachmentAndLeaksNothing is the teardown
// requirement: everything a tunnel started is gone once Close returns, and
// Close is idempotent. A remote client drops constantly — a phone does it every
// time it changes network — so an attachment that leaked a goroutine would be a
// leak measured per disconnection.
//
// It also covers the frame that arrives after Close: a closed tunnel attaches
// nothing rather than attaching something nobody will ever tear down. The
// goroutine count is asserted to settle rather than to be instantaneous,
// because goroutines exit asynchronously.
func TestUnit_CloseJoinsEveryAttachmentAndLeaksNothing(t *testing.T) {
	var counter atomic.Int64

	before := runtime.NumGoroutine()
	for range 5 {
		func() {
			h := newHarness(t, identityFactory(&counter), nil)
			for _, session := range []string{"att-a", "att-b", "att-c"} {
				h.ask(session, 1, session)
			}
			if got := h.tunnel.Len(); got != 3 {
				t.Fatalf("Len = %d, want 3", got)
			}
			h.tunnel.Close()
			h.tunnel.Close()
			if got := h.tunnel.Len(); got != 0 {
				t.Fatalf("Len after Close = %d, want 0", got)
			}
			h.tunnel.Handle(context.Background(), librelay.Frame{
				Type:     librelay.TypeACPMessage,
				Instance: testInstance,
				Session:  "att-late",
				Payload:  json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"_x/y"}`),
			})
			if got := h.tunnel.Len(); got != 0 {
				t.Fatalf("Len after Handle on a closed tunnel = %d, want 0", got)
			}
			h.close()
		}()
	}

	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutines: %d before, %d after five tunnels served fifteen attachments",
		before, runtime.NumGoroutine())
}

// TestUnit_OnlyACPCargoForThisInstanceEverAttaches keeps the tunnel from
// treating anything else on the connection as a message. Resumption types are
// the case that matters: they must pass through without attaching anything, so
// a relay that speaks them changes nothing here and a relay that does not is
// equally well served.
//
// A frame carrying no instance at all is the one case that IS accepted: the
// routing key is already implied by the connection it arrived on.
func TestUnit_OnlyACPCargoForThisInstanceEverAttaches(t *testing.T) {
	t.Parallel()
	var counter atomic.Int64
	h := newHarness(t, identityFactory(&counter), nil)

	body := json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":{}}`, echoMethod))
	for _, f := range []librelay.Frame{
		{Type: librelay.TypeResume, Instance: testInstance, Session: "att-x", Payload: json.RawMessage(`{"after_seq":3}`)},
		{Type: librelay.TypeResumed, Instance: testInstance, Session: "att-x", Payload: json.RawMessage(`{"from_seq":4}`)},
		{Type: "some.future.cargo", Instance: testInstance, Session: "att-x", Payload: body},
		{Type: librelay.TypeACPMessage, Instance: testInstance, Session: "att-x"},
		{Type: librelay.TypeACPMessage, Instance: "inst-other", Session: "att-y", Payload: body},
	} {
		h.tunnel.Handle(context.Background(), f)
	}
	if got := h.tunnel.Len(); got != 0 {
		t.Fatalf("Len = %d after cargo the tunnel does not carry, want 0", got)
	}

	h.tunnel.Handle(context.Background(), librelay.Frame{Type: librelay.TypeACPMessage, Session: "att-x", Payload: body})
	if _, from := decodeEcho(t, h.recv("att-x").Payload); from != "" {
		t.Fatalf("unexpected echo %q", from)
	}
}

// TestUnit_LeastRecentlyAddressedAttachmentIsEvictedAtTheCap covers the
// backstop. [librelay.TypeACPDetach] is the ordinary ending, but a client that
// lost its network sends none and a relay pinned to an older librelay emits
// none at all, so a client that closed its socket can still be invisible from
// here; refusing at the cap instead of evicting would let those abandoned
// attachments accumulate until no live client could attach.
//
// att-a is addressed again before the cap is reached, which makes att-b what
// the cap gives up. Both survivors are then driven once more, so the test
// asserts they are whole connections rather than carcasses still in the map.
func TestUnit_LeastRecentlyAddressedAttachmentIsEvictedAtTheCap(t *testing.T) {
	t.Parallel()
	var counter atomic.Int64
	h := newHarness(t, identityFactory(&counter), func(cfg *relayacp.Config) { cfg.MaxAttachments = 2 })

	h.ask("att-a", 1, "a")
	h.ask("att-b", 1, "b")
	h.ask("att-a", 2, "a")

	h.ask("att-c", 1, "c")
	waitFor(t, "the evicted attachment to be reclaimed", func() bool { return h.tunnel.Len() == 2 })
	if !h.quiet("att-b", 100*time.Millisecond) {
		t.Fatal("the evicted attachment produced traffic")
	}

	agentA, _ := decodeEcho(t, h.ask("att-a", 3, "a").Payload)
	agentC, _ := decodeEcho(t, h.ask("att-c", 2, "c").Payload)
	if agentA == agentC {
		t.Fatalf("both survivors were served by %s", agentA)
	}
}

// TestUnit_NewRejectsAnIncompleteConfig keeps configuration failures at
// construction, where a caller can see them, rather than in a goroutine that
// silently serves nobody.
func TestUnit_NewRejectsAnIncompleteConfig(t *testing.T) {
	t.Parallel()
	factory := identityFactory(new(atomic.Int64))
	send := func(librelay.Frame) error { return nil }
	for name, cfg := range map[string]relayacp.Config{
		"no instance":   {Send: send, Factory: factory},
		"no send":       {Instance: testInstance, Factory: factory},
		"no factory":    {Instance: testInstance, Send: send},
		"unroutable id": {Instance: "inst\x00a", Send: send, Factory: factory},
		"oversized id":  {Instance: strings.Repeat("x", librelay.MaxIDBytes+1), Send: send, Factory: factory},
	} {
		if _, err := relayacp.New(cfg); err == nil {
			t.Fatalf("New accepted a config with %s", name)
		}
	}
}

// detach sends the explicit end-of-attachment frame for session, exactly as a
// relay whose librelay carries the constant would emit it.
func (h *harness) detach(session string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.t.Context(), testTimeout)
	defer cancel()
	f := librelay.Frame{Type: librelay.TypeACPDetach, Instance: testInstance, Session: session}
	if err := h.link.Send(ctx, f); err != nil {
		h.t.Fatalf("detach %s: %v", session, err)
	}
}

// TestUnit_DetachTearsDownOneAttachmentDeterministically covers the ordinary
// ending. Eviction bounds the map but reclaims on arrival pressure alone, so a
// live-but-idle attachment is what a burst of new clients gives up; a detach
// names its own attachment and ends that one, at the moment its client went
// away and regardless of what else is attached.
//
// The survivor is driven again afterwards, so the assertion is that it is a
// whole connection and not merely still a key in a map.
func TestUnit_DetachTearsDownOneAttachmentDeterministically(t *testing.T) {
	t.Parallel()
	var counter atomic.Int64
	h := newHarness(t, identityFactory(&counter), nil)

	h.ask("att-a", 1, "a")
	h.ask("att-b", 1, "b")
	if got := h.tunnel.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}

	h.detach("att-a")
	waitFor(t, "the detached attachment to be reclaimed", func() bool { return h.tunnel.Len() == 1 })
	if !h.quiet("att-a", 100*time.Millisecond) {
		t.Fatal("the detached attachment produced traffic")
	}
	if _, from := decodeEcho(t, h.ask("att-b", 2, "b-again").Payload); from != "b-again" {
		t.Fatal("the surviving attachment stopped answering")
	}
}

// TestUnit_DetachNeverAttachesAnything is the bound a detach must not become. A
// peer holding stale identifiers could otherwise mint one attachment per detach
// it sends, which is exactly the appetite the cap exists to limit — so a detach
// naming nothing live is a no-op, and so is one arriving for another instance,
// for a session already ended, or after the tunnel closed.
func TestUnit_DetachNeverAttachesAnything(t *testing.T) {
	t.Parallel()
	var counter atomic.Int64
	h := newHarness(t, identityFactory(&counter), nil)

	h.ask("att-a", 1, "a")
	for _, f := range []librelay.Frame{
		{Type: librelay.TypeACPDetach, Instance: testInstance, Session: "att-never-seen"},
		{Type: librelay.TypeACPDetach, Instance: "inst-other", Session: "att-a"},
		{Type: librelay.TypeACPDetach, Instance: testInstance},
	} {
		h.tunnel.Handle(context.Background(), f)
	}
	if got := h.tunnel.Len(); got != 1 {
		t.Fatalf("Len = %d after detaches that name nothing this tunnel holds, want 1", got)
	}

	h.tunnel.Handle(context.Background(), librelay.Frame{Type: librelay.TypeACPDetach, Instance: testInstance, Session: "att-a"})
	waitFor(t, "the detached attachment to be reclaimed", func() bool { return h.tunnel.Len() == 0 })
	h.tunnel.Handle(context.Background(), librelay.Frame{Type: librelay.TypeACPDetach, Instance: testInstance, Session: "att-a"})
	if got := h.tunnel.Len(); got != 0 {
		t.Fatalf("Len = %d after a repeated detach, want 0", got)
	}

	h.tunnel.Close()
	h.tunnel.Handle(context.Background(), librelay.Frame{Type: librelay.TypeACPDetach, Instance: testInstance, Session: "att-a"})
	if got := h.tunnel.Len(); got != 0 {
		t.Fatalf("Len = %d after a detach on a closed tunnel, want 0", got)
	}
}

// TestUnit_AReattachedSessionIsAFreshConnection pins what a detach costs: the
// agent behind it, not the identifier. A relay reuses an attachment identifier
// only by minting a new one, but a client that detached and returned under the
// same name must not be handed the carcass of the connection it left — its ACP
// handshake starts over, so anything else would resume a JSON-RPC stream whose
// peer no longer exists.
func TestUnit_AReattachedSessionIsAFreshConnection(t *testing.T) {
	t.Parallel()
	var counter atomic.Int64
	h := newHarness(t, identityFactory(&counter), nil)

	before, _ := decodeEcho(t, h.ask("att-a", 1, "a").Payload)
	h.detach("att-a")
	waitFor(t, "the detached attachment to be reclaimed", func() bool { return h.tunnel.Len() == 0 })

	after, _ := decodeEcho(t, h.ask("att-a", 1, "a").Payload)
	if before == after {
		t.Fatalf("re-attaching served the same connection %s; a detach ends it", before)
	}
	if got := h.tunnel.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
}
