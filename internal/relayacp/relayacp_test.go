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

const testTimeout = 10 * time.Second

const testInstance = "inst-a"

const echoMethod = "_relayacp/whoami"

// Asserts Handle satisfies relaylink.Handler at compile time.
var _ relaylink.Handler = (*relayacp.Tunnel)(nil).Handle

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

func (h *harness) close() {
	h.closeOnce.Do(func() {
		h.stopPump()
		h.tunnel.Close()
		_ = h.conn.Close()
		h.relay.Close()
	})
}

func newHarness(t *testing.T, factory libacp.AgentFactory, tune func(*relayacp.Config)) *harness {
	t.Helper()
	h := &harness{t: t, relay: relaytest.New(), byID: map[string]chan librelay.Frame{}}
	h.link = h.relay.Dial()

	cfg := relayacp.Config{Instance: testInstance, Factory: factory}
	// Late-bound: the connector needs the tunnel's handler before it exists,
	// and the tunnel needs somewhere to send.
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

func (h *harness) ask(session string, id int, from string) librelay.Frame {
	h.t.Helper()
	h.send(session, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{"from":%q}}`, id, echoMethod, from))
	return h.recv(session)
}

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

func (h *harness) emitted() []librelay.Frame {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]librelay.Frame(nil), h.seen...)
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

// TestUnit_TwoAttachmentsAreTwoIndependentACPSessions checks two clients
// attached to one instance are served by independent connections whose
// interleaved traffic never crosses.
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

// TestUnit_EveryOutboundFrameEchoesItsSessionAndCarriesNothingElse checks
// every outbound frame echoes its Session and carries no other envelope
// field.
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

// TestUnit_SlowAttachmentIsDroppedAndDoesNotBlockOthers checks a wedged
// attachment is dropped at its queue bound without blocking the read loop or
// other attachments.
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

// TestUnit_CloseJoinsEveryAttachmentAndLeaksNothing checks every goroutine a
// tunnel started is gone once Close returns, Close is idempotent, and a frame
// arriving after Close attaches nothing.
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

// TestUnit_OnlyACPCargoForThisInstanceEverAttaches checks only an ACP
// message frame for this instance (or with no instance at all) attaches
// anything; resumption and unknown cargo types never do.
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

// TestUnit_LeastRecentlyAddressedAttachmentIsEvictedAtTheCap checks the
// least recently addressed attachment, not the newest, is evicted at the cap.
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

// TestUnit_NewRejectsAnIncompleteConfig checks New fails at construction on
// a missing or invalid field.
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

func (h *harness) detach(session string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.t.Context(), testTimeout)
	defer cancel()
	f := librelay.Frame{Type: librelay.TypeACPDetach, Instance: testInstance, Session: session}
	if err := h.link.Send(ctx, f); err != nil {
		h.t.Fatalf("detach %s: %v", session, err)
	}
}

// TestUnit_DetachTearsDownOneAttachmentDeterministically checks a detach
// ends its own named attachment immediately, leaving others live.
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

// TestUnit_DetachNeverAttachesAnything checks a detach naming nothing live,
// another instance, an already-ended session, or arriving after Close is a
// no-op.
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

// TestUnit_AReattachedSessionIsAFreshConnection checks a session reused
// after a detach gets a fresh connection, not the old one.
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
