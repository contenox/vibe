package acpsvc

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	libacp "github.com/contenox/beam/libacp"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// This file exercises ADOPT: a session/new carrying the contenox.adopt `_meta` key binds
// the new upstream session to an ALREADY-RUNNING instance+session instead of spawning
// anything. The keystone is
// TestLoopback_Adopt_DispatchedPermissionReachesAdoptingViewer, which reproduces the
// fleet-dispatch black hole (a permission request auto-denied because nobody is watching)
// and then closes it by adopting. The downstream is the hermetic in-repo acp-stub-agent;
// there is no LLM backend and no mocked kernel on the keystone path.

// dispatchLike drives the kernel the way fleetservice.Dispatch does — Start, then
// OpenSession — and returns the ids a dispatch would hand back. It attaches NO viewer,
// which is exactly the condition adopt exists to repair. It deliberately calls the
// Manager rather than importing fleetservice: the hole is in the kernel-facing shape of
// a dispatch, not in that package's policy checks.
func dispatchLike(t *testing.T, mgr agentinstance.Manager, agentName, cwd string) (string, libacp.SessionID) {
	t.Helper()
	ctx := context.Background()
	instanceID, err := mgr.Start(ctx, agentName, t.TempDir())
	require.NoError(t, err)
	sessionID, err := mgr.OpenSession(ctx, instanceID, agentinstance.SessionSpec{Cwd: cwd})
	require.NoError(t, err)
	return instanceID, sessionID
}

// denyRecorder collects the kernel's EventUnsupervisedDeny events, so a test can assert
// that a permission request WAS auto-denied for lack of a controller (before adoption)
// and was NOT after one attached.
type denyRecorder struct {
	mu   sync.Mutex
	dens []libacp.SessionID
}

func (d *denyRecorder) sink(ev agentinstance.Event) {
	if ev.Kind != agentinstance.EventUnsupervisedDeny {
		return
	}
	d.mu.Lock()
	d.dens = append(d.dens, ev.SessionID)
	d.mu.Unlock()
}

func (d *denyRecorder) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.dens)
}

// cancelPermission is the permission answer the adopting client gives in these tests. The
// stub agent's callbacks scenario ends the turn as a refusal on a cancelled outcome
// WITHOUT going on to its fs/* round trip — which the Instances path deliberately does not
// serve (the kernel's harness answers fs/* with MethodNotFound). What the test asserts is
// that the request REACHED the adopter at all, which the unattached case can never do.
var cancelPermission = libacp.RequestPermissionResponse{
	Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeCancelled},
}

// -----------------------------------------------------------------------------
// parseAdoptMeta — the defensive `_meta` decode.
// -----------------------------------------------------------------------------

func TestAdopt_ParseAdoptMeta(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta string
		want adoptRef
		ok   bool
	}{
		{name: "nil meta", meta: ""},
		{name: "empty object", meta: `{}`},
		{name: "unrelated keys only", meta: `{"contenox.agent":"claude","other":1}`},
		{name: "malformed json", meta: `{"contenox.adopt":`},
		{name: "wrong-shaped value (string)", meta: `{"contenox.adopt":"inst-1"}`},
		{name: "wrong-shaped value (array)", meta: `{"contenox.adopt":["inst-1","sess-1"]}`},
		{name: "wrong-shaped field types", meta: `{"contenox.adopt":{"instanceId":7,"sessionId":true}}`},
		{name: "instanceId only", meta: `{"contenox.adopt":{"instanceId":"inst-1"}}`},
		{name: "sessionId only", meta: `{"contenox.adopt":{"sessionId":"sess-1"}}`},
		{name: "blank ids", meta: `{"contenox.adopt":{"instanceId":"  ","sessionId":""}}`},
		{
			name: "both ids",
			meta: `{"contenox.adopt":{"instanceId":"inst-1","sessionId":"sess-1"}}`,
			want: adoptRef{InstanceID: "inst-1", SessionID: "sess-1"},
			ok:   true,
		},
		{
			name: "ids are trimmed",
			meta: `{"contenox.adopt":{"instanceId":" inst-1 ","sessionId":"\tsess-1\n"}}`,
			want: adoptRef{InstanceID: "inst-1", SessionID: "sess-1"},
			ok:   true,
		},
		{
			name: "coexists with contenox.agent",
			meta: `{"contenox.agent":"claude","contenox.adopt":{"instanceId":"inst-1","sessionId":"sess-1"}}`,
			want: adoptRef{InstanceID: "inst-1", SessionID: "sess-1"},
			ok:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.meta != "" {
				raw = json.RawMessage(tc.meta)
			}
			got, ok := parseAdoptMeta(raw)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestAdopt_MetaRoundTrips pins the exact wire shape a client (beam, a future CLI) must
// send: `{"contenox.adopt":{"instanceId":...,"sessionId":...}}`.
func TestAdopt_MetaRoundTrips(t *testing.T) {
	raw := adoptMetaJSON("inst-7", libacp.SessionID("sess-9"))
	require.JSONEq(t, `{"contenox.adopt":{"instanceId":"inst-7","sessionId":"sess-9"}}`, string(raw))
	ref, ok := parseAdoptMeta(raw)
	require.True(t, ok)
	require.Equal(t, adoptRef{InstanceID: "inst-7", SessionID: "sess-9"}, ref)
}

// TestUnit_AdoptResultMeta_RoundTrips pins the RESPONSE wire shape the Beam half consumes:
// alongside the unchanged contenox.agent attribution, the session/new response `_meta`
// echoes contenox.adopt with the adopt outcome — instanceId, sessionId, and the controller
// flag the UI labels "übernommen" vs "beobachten" from. It also guards the one thing that
// could silently break by adding the key: parseAgentMeta must still read the agent name out
// of the combined blob, so every existing attribution reader is unaffected.
func TestUnit_AdoptResultMeta_RoundTrips(t *testing.T) {
	raw := adoptedSessionMetaJSON("reporter", "inst-7", libacp.SessionID("sess-9"), true)
	require.JSONEq(t,
		`{"contenox.agent":"reporter","contenox.adopt":{"instanceId":"inst-7","sessionId":"sess-9","controller":true}}`,
		string(raw))

	res, ok := parseAdoptResultMeta(raw)
	require.True(t, ok)
	require.Equal(t, adoptResult{InstanceID: "inst-7", SessionID: "sess-9", Controller: true}, res)

	// The added key must not shadow attribution: existing readers still find the agent.
	require.Equal(t, "reporter", parseAgentMeta(raw),
		"contenox.agent stays readable beside the adopt outcome")

	// An observer adopt reports controller=false — the "beobachten" case.
	observer := adoptedSessionMetaJSON("reporter", "inst-7", libacp.SessionID("sess-9"), false)
	res, ok = parseAdoptResultMeta(observer)
	require.True(t, ok)
	require.False(t, res.Controller)

	// Defensive decode: a response with no adopt key (an ordinary external session) reads
	// as "no adopt outcome" rather than erroring.
	_, ok = parseAdoptResultMeta(agentMetaJSON("reporter"))
	require.False(t, ok, "a non-adopted session's _meta carries no adopt outcome")
}

// -----------------------------------------------------------------------------
// The keystone: dispatch → adopt → a downstream permission request reaches the
// adopting viewer instead of being auto-denied as unsupervised.
// -----------------------------------------------------------------------------

func TestLoopback_Adopt_DispatchedPermissionReachesAdoptingViewer(t *testing.T) {
	rec := &denyRecorder{}
	f := newInstancesFixtureWith(t, func(db libdb.DBManager) agentinstance.Manager {
		return agentinstance.New(agentregistryservice.New(db), agentinstance.WithEventSink(rec.sink))
	})
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-adopt-perm", nil)
	ctx := context.Background()
	cwd := t.TempDir()

	// --- 1. DISPATCH: an instance + session with NO viewer attached. ---
	instanceID, downstreamID := dispatchLike(t, f.mgr, agentName, cwd)

	// --- 2. The black hole, reproduced. A permission-gated turn on an unwatched
	// session is auto-denied by the kernel (no controller viewer), the downstream sees a
	// "cancelled" outcome and gives up: nobody was asked, and nobody could be. ---
	stop, err := f.mgr.Prompt(ctx, instanceID, downstreamID,
		[]libacp.ContentBlock{libacp.NewTextContent("callbacks")})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonRefusal, stop,
		"an unwatched dispatched session's permission request is auto-denied and the turn refuses")
	require.Equal(t, 1, rec.count(), "the kernel recorded exactly one unsupervised deny")

	// --- 3. ADOPT the running instance+session onto a fresh upstream ACP session. ---
	c := f.connect()
	_, err = c.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	c.lc.setPermissionResponse(cancelPermission)

	newResp, err := c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
		Meta:       adoptMetaJSON(instanceID, downstreamID),
	})
	require.NoError(t, err)
	require.NotEmpty(t, newResp.SessionID)
	require.Equal(t, agentName, metaAgent(t, newResp.Meta),
		"attribution comes from the INSTANCE, not the client")

	// Adoption binds, it does not spawn: still exactly one instance, and the session's
	// driver points at the adopted one.
	require.Equal(t, 1, liveInstances(t, f.mgr), "adopt must NOT bring up a second instance")
	ed := c.externalDriver(newResp.SessionID)
	require.Equal(t, instanceID, extInstanceID(ed))
	require.Nil(t, extHandle(ed), "an adopted session's driver owns no process")

	// --- 4. The payoff: the SAME permission-gated turn now reaches a human surface. ---
	promptResp, err := c.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("callbacks")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonRefusal, promptResp.StopReason,
		"the adopter answered 'cancelled', so the downstream still refuses — but it was ASKED")

	permReq, ok := c.lc.lastPermissionRequest()
	require.True(t, ok, "the downstream session/request_permission must reach the adopting viewer")
	require.Equal(t, newResp.SessionID, permReq.SessionID,
		"the request is remapped onto the UPSTREAM session id the client knows")
	require.Equal(t, "write scratch file", permReq.ToolCall.Title,
		"it is the downstream agent's real request, not a synthesized one")
	require.Len(t, permReq.Options, 2, "the downstream's own permission options are forwarded intact")
	require.Equal(t, 1, rec.count(),
		"no further unsupervised deny: the adopter is the session's controller now")
}

// TestLoopback_Adopt_FollowUpPromptStreamsBackThroughAdoptedSession is the "talk to it"
// half of the flagship loop: after adopting a dispatched unit's session, a follow-up prompt
// typed into the ADOPTED upstream session routes through the kernel to the still-running
// unit (Manager.Prompt on the instances path — the driver holds no connection of its own)
// and the unit's reply STREAMS BACK to this client, remapped onto the upstream session id it
// knows. This exercises the real acpsvc client connection end to end, not the kernel API
// directly, so it proves the transport verb — not just the kernel primitive underneath it.
func TestLoopback_Adopt_FollowUpPromptStreamsBackThroughAdoptedSession(t *testing.T) {
	f := newInstancesFixture(t)
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-adopt-followup", nil)
	ctx := context.Background()
	cwd := t.TempDir()

	// Dispatch: a running instance + session with NO viewer (the fleet condition adopt
	// repairs). Adopt IMMEDIATELY, before any prompt, so the session is silent and carries
	// no journal — the follow-up prompt's stream is then the only thing on the wire.
	instanceID, downstreamID := dispatchLike(t, f.mgr, agentName, cwd)

	c := f.connect()
	_, err := c.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
		Meta:       adoptMetaJSON(instanceID, downstreamID),
	})
	require.NoError(t, err)

	// The response `_meta` reports the OUTCOME: this connection took CONTROL of the
	// unattended dispatched session — the "übernommen" fact beam labels the tab from, echoed
	// back beside the exact binding the client asked for.
	res, ok := parseAdoptResultMeta(newResp.Meta)
	require.True(t, ok, "an adopted session's response _meta carries the contenox.adopt outcome")
	require.True(t, res.Controller, "adopting an unattended dispatched session takes control")
	require.Equal(t, instanceID, res.InstanceID)
	require.Equal(t, string(downstreamID), res.SessionID)

	// The payoff: a follow-up prompt on the adopted session reaches the unit and its reply
	// streams back. The stub's plain-prompt path acks with one agent_message_chunk (relayed
	// live during the turn) and the driver pushes a post-turn session_info_update after the
	// response — two updates in all.
	promptResp, err := c.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("hello from the adopter")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, promptResp.StopReason,
		"the follow-up prompt round-tripped to the unit and completed")

	notes := c.lc.drain(t, 2)
	var chunk *libacp.SessionNotification
	for i := range notes {
		require.Equal(t, newResp.SessionID, notes[i].SessionID,
			"every relayed update is remapped onto the UPSTREAM session id, not the downstream one")
		if notes[i].Update.SessionUpdate == libacp.SessionUpdateAgentMessageChunk {
			chunk = &notes[i]
		}
	}
	require.NotNil(t, chunk, "the unit's reply chunk reached the adopting client")
	require.Equal(t, "ack", chunk.Update.Content.Text,
		"the stub's reply text streamed back through the adopted session")
}

// TestLoopback_Adopt_DetachReinstatesUnsupervisedFallback is the honest other side of
// re-humanization: adoption hands the human the unit's permission asks, and DETACH hands
// them back. It proves both directions on one session — while adopted, a gated tool call's
// ask reaches the CLIENT (not the kernel's headless deny); after the connection drops (the
// WS-drop teardown path: connCtx fires and the bridge self-detaches from the kernel's
// fan-out), the SAME gated turn falls to the kernel's unattended fallback again. The
// fixture wires only an event sink, so the fallback here is the kernel's built-in headless
// deny (a wired WithPermissionFallback — serve's mission HITL envelope — would answer in its
// place); either way, detach reinstates it the instant the last viewer leaves.
func TestLoopback_Adopt_DetachReinstatesUnsupervisedFallback(t *testing.T) {
	rec := &denyRecorder{}
	f := newInstancesFixtureWith(t, func(db libdb.DBManager) agentinstance.Manager {
		return agentinstance.New(agentregistryservice.New(db), agentinstance.WithEventSink(rec.sink))
	})
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-adopt-detach", nil)
	ctx := context.Background()
	cwd := t.TempDir()

	instanceID, downstreamID := dispatchLike(t, f.mgr, agentName, cwd)

	// Adopt and take control, then drive a gated turn: the stub's callbacks scenario asks a
	// permission, which now reaches the human surface instead of being auto-denied.
	c := f.connect()
	_, err := c.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	c.lc.setPermissionResponse(cancelPermission)
	newResp, err := c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
		Meta:       adoptMetaJSON(instanceID, downstreamID),
	})
	require.NoError(t, err)

	_, err = c.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("callbacks")},
	})
	require.NoError(t, err)
	_, gotWhileAdopted := c.lc.lastPermissionRequest()
	require.True(t, gotWhileAdopted, "while adopted, the gated tool call's permission ask reaches the client")
	require.Zero(t, rec.count(), "and NOT the kernel's unsupervised deny — a human was asked")

	// Detach: drop the upstream connection. The bridge's connCtx watcher removes it from the
	// kernel's fan-out; the session loses its controller and returns to unattended.
	c.drop()
	require.Eventually(t, func() bool {
		st, gerr := f.mgr.Get(instanceID)
		return gerr == nil && st.Viewers == 0
	}, 2*time.Second, 10*time.Millisecond, "the dropped connection's viewer detaches from the kernel")

	// The SAME gated turn on the now-unwatched session is auto-denied again by the built-in
	// headless fallback — exactly one NEW deny, proving the fallback resumes at detach.
	stop, err := f.mgr.Prompt(ctx, instanceID, downstreamID,
		[]libacp.ContentBlock{libacp.NewTextContent("callbacks")})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonRefusal, stop,
		"an unwatched session's permission request is auto-denied again after detach")
	require.Equal(t, 1, rec.count(),
		"after detach the unsupervised fallback answers again — exactly one new deny")
}

// TestLoopback_Adopt_ReplaysJournalToAdopter is the "I can see what it did before I got
// here" property: the updates a dispatched session emitted while nobody watched are
// replayed to the adopting viewer from the kernel's in-memory journal — the ONLY record
// of them, since dispatch writes no durable transcript.
func TestLoopback_Adopt_ReplaysJournalToAdopter(t *testing.T) {
	f := newInstancesFixture(t)
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-adopt-replay", nil)
	ctx := context.Background()
	cwd := t.TempDir()

	instanceID, downstreamID := dispatchLike(t, f.mgr, agentName, cwd)

	// A full unwatched turn: the stub's session_updates scenario emits four updates
	// (chunk, tool_call, tool_call_update, chunk) that go straight into the journal.
	stop, err := f.mgr.Prompt(ctx, instanceID, downstreamID,
		[]libacp.ContentBlock{libacp.NewTextContent("session_updates")})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, stop)

	c := f.connect()
	_, err = c.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
		Meta:       adoptMetaJSON(instanceID, downstreamID),
	})
	require.NoError(t, err)

	notes := c.lc.drain(t, 4)
	kinds := make([]libacp.SessionUpdateKind, 0, len(notes))
	for _, n := range notes {
		require.Equal(t, newResp.SessionID, n.SessionID,
			"replayed updates are remapped onto the upstream session id")
		kinds = append(kinds, n.Update.SessionUpdate)
	}
	require.Equal(t, []libacp.SessionUpdateKind{
		libacp.SessionUpdateAgentMessageChunk,
		libacp.SessionUpdateToolCall,
		libacp.SessionUpdateToolCallUpdate,
		libacp.SessionUpdateAgentMessageChunk,
	}, kinds, "the pre-adoption turn is replayed in arrival order")
	require.Equal(t, "running scenario...", notes[0].Update.Content.Text)
	require.Equal(t, "done", notes[3].Update.Content.Text)
}

// TestLoopback_Adopt_ReconnectUsesOrdinaryReattachPath proves adoption is a ONE-TIME
// binding, not a mode: because it persists the instance + downstream ids exactly as the
// bring-up path does, a later session/load re-attaches through the ordinary
// externalDriver.ensureAttached with no adopt-specific logic and no second instance.
func TestLoopback_Adopt_ReconnectUsesOrdinaryReattachPath(t *testing.T) {
	f := newInstancesFixture(t)
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-adopt-reconnect", nil)
	ctx := context.Background()
	cwd := t.TempDir()

	instanceID, downstreamID := dispatchLike(t, f.mgr, agentName, cwd)
	_, err := f.mgr.Prompt(ctx, instanceID, downstreamID,
		[]libacp.ContentBlock{libacp.NewTextContent("warm the journal")})
	require.NoError(t, err)

	c1 := f.connect()
	_, err = c1.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := c1.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd: cwd, McpServers: []libacp.McpServer{}, Meta: adoptMetaJSON(instanceID, downstreamID),
	})
	require.NoError(t, err)
	c1.drop()

	c2 := f.connect()
	_, err = c2.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	_, err = c2.client.LoadSession(ctx, libacp.LoadSessionRequest{SessionID: newResp.SessionID, Cwd: cwd})
	require.NoError(t, err)
	// The re-attach is lazy: the first prompt after a load drives it.
	promptResp, err := c2.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("still there?")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, promptResp.StopReason)

	ed := c2.externalDriver(newResp.SessionID)
	require.Equal(t, instanceID, extInstanceID(ed),
		"the reloaded adopted session re-attaches to the SAME dispatched instance")
	require.Nil(t, extHandle(ed))
	require.Equal(t, 1, liveInstances(t, f.mgr), "reconnect must NOT spawn a second instance")
}

// TestLoopback_Adopt_SecondAdopterObservesWithoutControl pins the kernel's N-viewers /
// one-controller rule as adopt inherits it: a second connection adopting the same session
// is admitted as an OBSERVER (it still sees the stream), while permission requests keep
// going to the first adopter. Adopt adds no controller logic of its own.
func TestLoopback_Adopt_SecondAdopterObservesWithoutControl(t *testing.T) {
	f := newInstancesFixture(t)
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-adopt-observer", nil)
	ctx := context.Background()
	cwd := t.TempDir()

	instanceID, downstreamID := dispatchLike(t, f.mgr, agentName, cwd)
	_, err := f.mgr.Prompt(ctx, instanceID, downstreamID,
		[]libacp.ContentBlock{libacp.NewTextContent("warm the journal")})
	require.NoError(t, err)

	first := f.connect()
	_, err = first.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	first.lc.setPermissionResponse(cancelPermission)
	firstResp, err := first.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd: cwd, McpServers: []libacp.McpServer{}, Meta: adoptMetaJSON(instanceID, downstreamID),
	})
	require.NoError(t, err)

	second := f.connect()
	_, err = second.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	second.lc.setPermissionResponse(cancelPermission)
	secondResp, err := second.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd: cwd, McpServers: []libacp.McpServer{}, Meta: adoptMetaJSON(instanceID, downstreamID),
	})
	require.NoError(t, err)
	require.NotEqual(t, firstResp.SessionID, secondResp.SessionID,
		"each adopter gets its own upstream session over the same downstream one")

	st, err := f.mgr.Get(instanceID)
	require.NoError(t, err)
	require.Equal(t, 2, st.Viewers, "both adopters are viewers of the one downstream session")

	// A permission-gated turn driven by the SECOND adopter is still answered by the
	// FIRST — the controller is attach-ordered, not request-ordered.
	_, err = second.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: secondResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("callbacks")},
	})
	require.NoError(t, err)
	_, gotFirst := first.lc.lastPermissionRequest()
	require.True(t, gotFirst, "the controller (first adopter) answers the permission")
	_, gotSecond := second.lc.lastPermissionRequest()
	require.False(t, gotSecond, "the observer (second adopter) is never asked")
}

// -----------------------------------------------------------------------------
// Rejections. Every one is a clean session/new failure with NO session created and
// NOTHING stopped — the instance belongs to whoever dispatched it.
// -----------------------------------------------------------------------------

func TestLoopback_Adopt_NilInstancesRefused(t *testing.T) {
	// The stdio/connCtx harness wires no Manager: there is no instance to adopt, and
	// falling through to a fresh bring-up would silently spawn a second agent.
	h := newLoopbackHarness(t)
	require.Nil(t, h.tr.deps.Instances)
	ctx := context.Background()
	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	_, err = h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       adoptMetaJSON("inst-1", "sess-1"),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "agent-instance manager")
}

func TestLoopback_Adopt_UnknownInstanceRefused(t *testing.T) {
	f := newInstancesFixture(t)
	ctx := context.Background()
	c := f.connect()
	_, err := c.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	_, err = c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       adoptMetaJSON(uuid.NewString(), "sess-1"),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown instance")
}

func TestLoopback_Adopt_SessionNotOnInstanceRefused(t *testing.T) {
	f := newInstancesFixture(t)
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-adopt-wrongsess", nil)
	ctx := context.Background()
	cwd := t.TempDir()

	instanceID, downstreamID := dispatchLike(t, f.mgr, agentName, cwd)
	_, err := f.mgr.Prompt(ctx, instanceID, downstreamID,
		[]libacp.ContentBlock{libacp.NewTextContent("warm the journal")})
	require.NoError(t, err)

	c := f.connect()
	_, err = c.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	// The instance is real and running; the session id is not one of ITS sessions.
	// Without this check the client would become controller of a session it invented.
	_, err = c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
		Meta:       adoptMetaJSON(instanceID, "attacker-supplied-session"),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not live on instance")

	// The real session is untouched: it still adopts cleanly afterwards.
	_, err = c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
		Meta:       adoptMetaJSON(instanceID, downstreamID),
	})
	require.NoError(t, err)
}

// TestLoopback_Adopt_StoppedInstanceRefused covers both non-running rejections through
// the surface a client actually hits: a Manager double reporting a StateError instance
// (the kernel removes a Stopped instance from its registry outright, so StateError is the
// only non-running state a real Get can return).
func TestLoopback_Adopt_NotRunningInstanceRefused(t *testing.T) {
	fake := &fakeAdoptManager{
		status: agentinstance.InstanceStatus{
			ID:         "inst-dead",
			AgentName:  "runner",
			State:      agentinstance.StateError,
			SessionIDs: []string{"sess-1"},
		},
	}
	f := newInstancesFixtureWith(t, func(libdb.DBManager) agentinstance.Manager { return fake })
	ctx := context.Background()
	c := f.connect()
	_, err := c.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	_, err = c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       adoptMetaJSON("inst-dead", "sess-1"),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not running")
	require.Zero(t, fake.attaches(), "a refused adopt never attaches a viewer")
	require.Zero(t, fake.stops(), "a refused adopt never stops the instance it declined")
}

// TestLoopback_Adopt_SessionOpenedButSilentIsAdoptable pins the case adopt exists FOR: a
// dispatched session that has emitted nothing at all. The kernel's InstanceStatus.SessionIDs
// is sourced from its session driver (seeded at OpenSession), not from its viewer hub
// (which materializes a session only on its first delivered update or first attach), so a
// silent session is open, listed, and adoptable the instant dispatch returns.
//
// This is not a corner case: on local inference the silent window IS the cold model load
// and the long first reasoning pass — the stretch where an operator most wants to take
// control, and the stretch during which an earlier hub-derived SessionIDs made adoption
// impossible. This test previously asserted that refusal; it now asserts the fix.
func TestLoopback_Adopt_SessionOpenedButSilentIsAdoptable(t *testing.T) {
	f := newInstancesFixture(t)
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-adopt-silent", nil)
	ctx := context.Background()
	cwd := t.TempDir()

	instanceID, downstreamID := dispatchLike(t, f.mgr, agentName, cwd)
	st, err := f.mgr.Get(instanceID)
	require.NoError(t, err)
	require.Equal(t, []string{string(downstreamID)}, st.SessionIDs,
		"an opened-but-silent session is live on the instance from the moment it is opened")
	require.Zero(t, st.Viewers, "and nobody is watching it — the condition adopt repairs")

	c := f.connect()
	_, err = c.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	// No prompt has run: the session has produced no update and has no journal.
	// Adoption must still succeed and must still hand this connection the controller
	// role, since the dispatched session has no controller.
	resp, err := c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
		Meta:       adoptMetaJSON(instanceID, downstreamID),
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.SessionID)
	require.Equal(t, agentName, parseAgentMeta(resp.Meta),
		"an adopted session is attributed to the kernel's agent, not the client's claim")

	st, err = f.mgr.Get(instanceID)
	require.NoError(t, err)
	require.Equal(t, 1, st.Viewers, "the adopter is attached as a viewer of the silent session")

	// The adopted session is a working one: the downstream still drives a turn on it.
	_, err = f.mgr.Prompt(ctx, instanceID, downstreamID,
		[]libacp.ContentBlock{libacp.NewTextContent("say something")})
	require.NoError(t, err)
}

// -----------------------------------------------------------------------------
// Fall-through: a session/new WITHOUT the adopt key behaves exactly as before.
// -----------------------------------------------------------------------------

// TestLoopback_Adopt_AbsentMetaLeavesBothExistingPathsUnchanged proves adopt is purely
// additive: no `_meta` still lands on the native chain engine, and a contenox.agent
// `_meta` still BRINGS UP a fresh Manager-owned instance.
func TestLoopback_Adopt_AbsentMetaLeavesBothExistingPathsUnchanged(t *testing.T) {
	f := newInstancesFixture(t)
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-adopt-fallthrough", nil)
	ctx := context.Background()

	c := f.connect()
	_, err := c.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	// No `_meta` at all: the native path, which advertises no external agent.
	nativeResp, err := c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)
	require.Empty(t, parseAgentMeta(nativeResp.Meta), "a native session carries no agent attribution")
	require.Equal(t, 0, liveInstances(t, f.mgr), "a native session brings up no instance")

	// contenox.agent only: the historical external bring-up, one fresh instance.
	extResp, err := c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)
	require.Equal(t, agentName, metaAgent(t, extResp.Meta))
	require.Equal(t, 1, liveInstances(t, f.mgr), "the agent path still spawns its own instance")
}

// TestLoopback_Adopt_MalformedAdoptMetaFallsThrough pins the defensive decode end to end:
// an unparseable / wrong-shaped contenox.adopt value must NOT fail session/new — it reads
// as "no adopt" and the request lands on the path it would have without the key.
func TestLoopback_Adopt_MalformedAdoptMetaFallsThrough(t *testing.T) {
	f := newInstancesFixture(t)
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-adopt-malformed", nil)
	ctx := context.Background()

	c := f.connect()
	_, err := c.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	// Wrong-shaped adopt value, no agent key: native path, no error.
	nativeResp, err := c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       json.RawMessage(`{"contenox.adopt":"not-an-object"}`),
	})
	require.NoError(t, err)
	require.Empty(t, parseAgentMeta(nativeResp.Meta))

	// Incomplete adopt value alongside a valid agent key: the agent path still runs.
	extResp, err := c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta: json.RawMessage(
			`{"contenox.agent":"` + agentName + `","contenox.adopt":{"instanceId":"only-half"}}`),
	})
	require.NoError(t, err)
	require.Equal(t, agentName, metaAgent(t, extResp.Meta))
}

// -----------------------------------------------------------------------------
// The relay hold — adopt's replay must survive the pre-response window.
// -----------------------------------------------------------------------------

// TestAdopt_HoldRelayQueuesThenFlushesInOrder pins the mechanism the journal replay rides
// on: while held, relays are QUEUED (not dropped, unlike suppressReplay), and releaseRelay
// emits them in arrival order before live relay resumes.
func TestAdopt_HoldRelayQueuesThenFlushesInOrder(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	b := newExternalBridge(h.tr, "upstream-hold", true)

	b.holdRelay()
	for _, text := range []string{"one", "two", "three"} {
		b.relayUpstream(ctx, libacp.NewAgentMessageChunk(text))
	}
	select {
	case n := <-h.lc.updates:
		t.Fatalf("a held relay must not reach the client: %+v", n)
	case <-time.After(100 * time.Millisecond):
	}

	b.releaseRelay(ctx)
	b.relayUpstream(ctx, libacp.NewAgentMessageChunk("live"))

	got := h.lc.drain(t, 4)
	texts := make([]string, 0, len(got))
	for _, n := range got {
		require.Equal(t, libacp.SessionID("upstream-hold"), n.SessionID)
		texts = append(texts, n.Update.Content.Text)
	}
	require.Equal(t, []string{"one", "two", "three", "live"}, texts)
}

// -----------------------------------------------------------------------------
// A Manager double for the states a real kernel will not hand back on demand.
// -----------------------------------------------------------------------------

// fakeAdoptManager is an agentinstance.Manager whose Get answer a test dictates. It
// exists for the ONE case the real kernel cannot be driven into on request (an instance
// that is registered but not Running — Stop removes it outright), and it counts the two
// calls a refused adopt must never make.
type fakeAdoptManager struct {
	status agentinstance.InstanceStatus

	mu          sync.Mutex
	attachCount int
	stopCount   int
}

func (m *fakeAdoptManager) attaches() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.attachCount
}

func (m *fakeAdoptManager) stops() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopCount
}

func (m *fakeAdoptManager) Start(context.Context, string, string) (string, error) { return "", nil }

func (m *fakeAdoptManager) StartResolved(context.Context, *runtimetypes.Agent, string) (string, error) {
	return "", nil
}

func (m *fakeAdoptManager) Attach(context.Context, string, libacp.SessionID, agentinstance.Viewer) (bool, error) {
	m.mu.Lock()
	m.attachCount++
	m.mu.Unlock()
	return true, nil
}

func (m *fakeAdoptManager) Detach(string, libacp.SessionID, string) error { return nil }

func (m *fakeAdoptManager) List(context.Context) ([]agentinstance.FleetEntry, error) {
	return nil, nil
}

func (m *fakeAdoptManager) Get(instanceID string) (agentinstance.InstanceStatus, error) {
	if instanceID != m.status.ID {
		return agentinstance.InstanceStatus{}, agentinstance.ErrNotFound
	}
	return m.status, nil
}

func (m *fakeAdoptManager) OpenSession(context.Context, string, agentinstance.SessionSpec) (libacp.SessionID, error) {
	return "", nil
}

func (m *fakeAdoptManager) Prompt(context.Context, string, libacp.SessionID, []libacp.ContentBlock) (libacp.StopReason, error) {
	return libacp.StopReasonEndTurn, nil
}

func (m *fakeAdoptManager) DeliverToSession(context.Context, libacp.SessionID, libacp.SessionNotification) error {
	return nil
}

func (m *fakeAdoptManager) Cancel(string, libacp.SessionID) error { return nil }

func (m *fakeAdoptManager) CloseSession(string, libacp.SessionID) error { return nil }

func (m *fakeAdoptManager) SetConfigOption(context.Context, string, libacp.SessionID, string, libacp.SessionConfigOptionValue) error {
	return nil
}

func (m *fakeAdoptManager) SessionConfigOptions(string, libacp.SessionID) ([]libacp.SessionConfigOption, error) {
	return nil, nil
}

func (m *fakeAdoptManager) AvailableCommands(string, libacp.SessionID) ([]libacp.AvailableCommand, error) {
	return nil, nil
}

func (m *fakeAdoptManager) Stop(string) error {
	m.mu.Lock()
	m.stopCount++
	m.mu.Unlock()
	return nil
}

func (m *fakeAdoptManager) Close() error { return nil }

var _ agentinstance.Manager = (*fakeAdoptManager)(nil)
