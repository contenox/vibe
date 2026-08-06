package acpsvc

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// This file exercises adopt: a session/new carrying the contenox.adopt `_meta`
// key binds the new upstream session to an already-running instance+session
// instead of spawning anything. The keystone is
// TestLoopback_Adopt_DispatchedPermissionReachesAdoptingViewer, which
// reproduces the fleet-dispatch black hole (a permission request auto-denied
// because nobody is watching) and closes it by adopting.

// dispatchLike drives the kernel the way fleetservice.Dispatch does (Start,
// then OpenSession) without attaching a viewer — the condition adopt exists
// to repair. Calls the Manager directly rather than importing fleetservice.
func dispatchLike(t *testing.T, mgr agentinstance.Manager, agentName, cwd string) (string, libacp.SessionID) {
	t.Helper()
	ctx := context.Background()
	instanceID, err := mgr.Start(ctx, agentName, t.TempDir())
	require.NoError(t, err)
	sessionID, err := mgr.OpenSession(ctx, instanceID, agentinstance.SessionSpec{Cwd: cwd})
	require.NoError(t, err)
	return instanceID, sessionID
}

// denyRecorder collects the kernel's EventUnsupervisedDeny events, so a test
// can assert whether a permission request was auto-denied for lack of a
// controller.
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

// cancelPermission is the permission answer the adopting client gives in
// these tests: the stub agent's callbacks scenario ends the turn as a
// cancelled refusal without an fs/* round trip. What matters is that the
// request reached the adopter at all.
var cancelPermission = libacp.RequestPermissionResponse{
	Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeCancelled},
}

// parseAdoptMeta — the defensive `_meta` decode.

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

// TestAdopt_MetaRoundTrips pins the request wire shape: `{"contenox.adopt":{"instanceId":...,"sessionId":...}}`.
func TestAdopt_MetaRoundTrips(t *testing.T) {
	raw := adoptMetaJSON("inst-7", libacp.SessionID("sess-9"))
	require.JSONEq(t, `{"contenox.adopt":{"instanceId":"inst-7","sessionId":"sess-9"}}`, string(raw))
	ref, ok := parseAdoptMeta(raw)
	require.True(t, ok)
	require.Equal(t, adoptRef{InstanceID: "inst-7", SessionID: "sess-9"}, ref)
}

// TestUnit_AdoptResultMeta_RoundTrips pins the response wire shape: contenox.adopt echoes the outcome beside the unchanged contenox.agent attribution.
func TestUnit_AdoptResultMeta_RoundTrips(t *testing.T) {
	raw := adoptedSessionMetaJSON("reporter", "inst-7", libacp.SessionID("sess-9"), true)
	require.JSONEq(t,
		`{"contenox.agent":"reporter","contenox.adopt":{"instanceId":"inst-7","sessionId":"sess-9","controller":true}}`,
		string(raw))

	res, ok := parseAdoptResultMeta(raw)
	require.True(t, ok)
	require.Equal(t, adoptResult{InstanceID: "inst-7", SessionID: "sess-9", Controller: true}, res)

	require.Equal(t, "reporter", parseAgentMeta(raw),
		"contenox.agent stays readable beside the adopt outcome")

	observer := adoptedSessionMetaJSON("reporter", "inst-7", libacp.SessionID("sess-9"), false)
	res, ok = parseAdoptResultMeta(observer)
	require.True(t, ok)
	require.False(t, res.Controller)

	_, ok = parseAdoptResultMeta(agentMetaJSON("reporter"))
	require.False(t, ok, "a non-adopted session's _meta carries no adopt outcome")
}

// TestLoopback_Adopt_DispatchedPermissionReachesAdoptingViewer is the keystone: dispatch, then adopt, then a downstream permission request reaches the adopting viewer instead of being auto-denied.
func TestLoopback_Adopt_DispatchedPermissionReachesAdoptingViewer(t *testing.T) {
	rec := &denyRecorder{}
	f := newInstancesFixtureWith(t, func(db libdb.DBManager) agentinstance.Manager {
		return agentinstance.New(agentregistryservice.New(db), agentinstance.WithEventSink(rec.sink))
	})
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-adopt-perm", nil)
	ctx := context.Background()
	cwd := t.TempDir()

	// Dispatch: an instance + session with no viewer attached.
	instanceID, downstreamID := dispatchLike(t, f.mgr, agentName, cwd)

	// The black hole: a permission-gated turn on an unwatched session is
	// auto-denied by the kernel, and the downstream gives up.
	stop, err := f.mgr.Prompt(ctx, instanceID, downstreamID,
		[]libacp.ContentBlock{libacp.NewTextContent("callbacks")})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonRefusal, stop,
		"an unwatched dispatched session's permission request is auto-denied and the turn refuses")
	require.Equal(t, 1, rec.count(), "the kernel recorded exactly one unsupervised deny")

	// Adopt the running instance+session onto a fresh upstream ACP session.
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

	require.Equal(t, 1, liveInstances(t, f.mgr), "adopt must NOT bring up a second instance")
	ed := c.externalDriver(newResp.SessionID)
	require.Equal(t, instanceID, extInstanceID(ed))
	require.Nil(t, extHandle(ed), "an adopted session's driver owns no process")

	// The payoff: the same permission-gated turn now reaches a human surface.
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

// TestLoopback_Adopt_FollowUpPromptStreamsBackThroughAdoptedSession pins: a follow-up prompt on an adopted session routes to the still-running unit and its reply streams back.
func TestLoopback_Adopt_FollowUpPromptStreamsBackThroughAdoptedSession(t *testing.T) {
	f := newInstancesFixture(t)
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-adopt-followup", nil)
	ctx := context.Background()
	cwd := t.TempDir()

	// Adopt immediately, before any prompt, so the follow-up's stream is the
	// only thing on the wire.
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

	res, ok := parseAdoptResultMeta(newResp.Meta)
	require.True(t, ok, "an adopted session's response _meta carries the contenox.adopt outcome")
	require.True(t, res.Controller, "adopting an unattended dispatched session takes control")
	require.Equal(t, instanceID, res.InstanceID)
	require.Equal(t, string(downstreamID), res.SessionID)

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

// TestLoopback_Adopt_DetachReinstatesUnsupervisedFallback pins both directions: while adopted, permission asks reach the client; after the connection drops, the same gated turn falls back to the kernel's unattended deny.
func TestLoopback_Adopt_DetachReinstatesUnsupervisedFallback(t *testing.T) {
	rec := &denyRecorder{}
	f := newInstancesFixtureWith(t, func(db libdb.DBManager) agentinstance.Manager {
		return agentinstance.New(agentregistryservice.New(db), agentinstance.WithEventSink(rec.sink))
	})
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-adopt-detach", nil)
	ctx := context.Background()
	cwd := t.TempDir()

	instanceID, downstreamID := dispatchLike(t, f.mgr, agentName, cwd)

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

	stop, err := f.mgr.Prompt(ctx, instanceID, downstreamID,
		[]libacp.ContentBlock{libacp.NewTextContent("callbacks")})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonRefusal, stop,
		"an unwatched session's permission request is auto-denied again after detach")
	require.Equal(t, 1, rec.count(),
		"after detach the unsupervised fallback answers again — exactly one new deny")
}

// TestLoopback_Adopt_ReplaysJournalToAdopter pins: updates a dispatched session emitted unwatched are replayed to the adopting viewer from the kernel's in-memory journal.
func TestLoopback_Adopt_ReplaysJournalToAdopter(t *testing.T) {
	f := newInstancesFixture(t)
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-adopt-replay", nil)
	ctx := context.Background()
	cwd := t.TempDir()

	instanceID, downstreamID := dispatchLike(t, f.mgr, agentName, cwd)

	// Emits four updates (chunk, tool_call, tool_call_update, chunk) into the journal.
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

// TestLoopback_Adopt_ReconnectUsesOrdinaryReattachPath pins: adoption is one-time, not a mode — a later session/load re-attaches through the ordinary path, no second instance.
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
	// Re-attach is lazy: the first prompt after a load drives it.
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

// TestLoopback_Adopt_SecondAdopterObservesWithoutControl pins the kernel's N-viewers/one-controller rule: a second adopter observes the stream but permission requests still go to the first.
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

	// Attach-ordered, not request-ordered: the controller is the first adopter.
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

// Rejections below: every one is a clean session/new failure with no session
// created and nothing stopped — the instance belongs to whoever dispatched it.

func TestLoopback_Adopt_NilInstancesRefused(t *testing.T) {
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

	_, err = c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
		Meta:       adoptMetaJSON(instanceID, "attacker-supplied-session"),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not live on instance")

	// The real session still adopts cleanly afterwards.
	_, err = c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
		Meta:       adoptMetaJSON(instanceID, downstreamID),
	})
	require.NoError(t, err)
}

// TestLoopback_Adopt_NotRunningInstanceRefused pins: a non-running instance (StateError — Stopped is removed from the registry outright) refuses adopt.
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

// TestLoopback_Adopt_SessionOpenedButSilentIsAdoptable pins: a dispatched session that has emitted nothing yet is still open, listed, and adoptable — the case adopt exists for.
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

	_, err = f.mgr.Prompt(ctx, instanceID, downstreamID,
		[]libacp.ContentBlock{libacp.NewTextContent("say something")})
	require.NoError(t, err)
}

// Fall-through below: a session/new without the adopt key behaves exactly as before.

// TestLoopback_Adopt_AbsentMetaLeavesBothExistingPathsUnchanged pins: adopt is purely additive — the native and contenox.agent paths are unchanged.
func TestLoopback_Adopt_AbsentMetaLeavesBothExistingPathsUnchanged(t *testing.T) {
	f := newInstancesFixture(t)
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-adopt-fallthrough", nil)
	ctx := context.Background()

	c := f.connect()
	_, err := c.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	nativeResp, err := c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)
	require.Empty(t, parseAgentMeta(nativeResp.Meta), "a native session carries no agent attribution")
	require.Equal(t, 0, liveInstances(t, f.mgr), "a native session brings up no instance")

	extResp, err := c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)
	require.Equal(t, agentName, metaAgent(t, extResp.Meta))
	require.Equal(t, 1, liveInstances(t, f.mgr), "the agent path still spawns its own instance")
}

// TestLoopback_Adopt_MalformedAdoptMetaFallsThrough pins: a malformed contenox.adopt value never fails session/new, it reads as "no adopt".
func TestLoopback_Adopt_MalformedAdoptMetaFallsThrough(t *testing.T) {
	f := newInstancesFixture(t)
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-adopt-malformed", nil)
	ctx := context.Background()

	c := f.connect()
	_, err := c.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	nativeResp, err := c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       json.RawMessage(`{"contenox.adopt":"not-an-object"}`),
	})
	require.NoError(t, err)
	require.Empty(t, parseAgentMeta(nativeResp.Meta))

	extResp, err := c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta: json.RawMessage(
			`{"contenox.agent":"` + agentName + `","contenox.adopt":{"instanceId":"only-half"}}`),
	})
	require.NoError(t, err)
	require.Equal(t, agentName, metaAgent(t, extResp.Meta))
}

// TestAdopt_HoldRelayQueuesThenFlushesInOrder pins: while held, relays queue (not drop); releaseRelay flushes them in arrival order before live relay resumes.
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

// fakeAdoptManager is an agentinstance.Manager whose Get answer a test
// dictates, for the one state a real kernel won't hand back on demand
// (registered but not Running). Counts the two calls a refused adopt must
// never make.
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
