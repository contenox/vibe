package acpsvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/chatservice"
	"github.com/contenox/beam/internal/services/missionservice"
	libacp "github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// reportNotification is the shape reportrouter builds for a delivered mission
// report: an agent_message_chunk addressed by the firing session's CONTENOX id,
// carrying the contenox.missionReport attribution. Built by hand here so this
// package's tests do not depend on reportrouter (which would be an import
// cycle waiting to happen) while still asserting against the real wire shape.
func reportNotification(contenoxSessionID, text string) libacp.SessionNotification {
	update := libacp.NewAgentMessageChunk(text)
	update.Meta = json.RawMessage(`{"contenox.missionReport":{"missionId":"m-1","reportId":"r-1","kind":"result"}}`)
	return libacp.SessionNotification{SessionID: libacp.SessionID(contenoxSessionID), Update: update}
}

// TestLoopback_MissionReport_RoutedThroughSessionRouterReachesTheFiringSession
// is the regression for the bug that made every beam-fired mission report land
// in the operator inbox: serve's report router was handed only the agent-instance
// kernel, which knows unit sessions and never a chat session, so delivery to the
// FIRING session always missed.
//
// It drives the real serve wiring: a session created over the real client, and
// the shared SessionRouter — the object serve now gives the report router —
// asked to deliver by the session's contenox id, exactly as the router does.
func TestLoopback_MissionReport_RoutedThroughSessionRouterReachesTheFiringSession(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)
	h.lc.drain(t, 1) // the session's available_commands_update

	contenoxID := h.contenoxSessionID(newResp.SessionID)
	require.NotEmpty(t, contenoxID, "the session must carry a contenox id — it is what a mission names as its parent")

	err = h.router.DeliverToContenoxSession(ctx, contenoxID, reportNotification(contenoxID, "unit chain-acp reported (result): done"))
	require.NoError(t, err, "the router must resolve the firing session to its live connection")

	got := h.lc.drain(t, 1)[0]
	require.Equal(t, newResp.SessionID, got.SessionID,
		"the report is re-addressed to the ACP session id the client knows, not the contenox id the router used")
	require.Contains(t, got.Update.Content.Text, "unit chain-acp reported")
	require.Contains(t, string(got.Update.Meta), "contenox.missionReport",
		"the attribution must survive so the client renders a report, not chat text")
}

// TestLoopback_MissionReport_UnknownSessionIsNotLive pins the other half of the
// contract the report router branches on: a contenox id no live connection owns
// is ErrSessionNotLive, which is what routes the report to the operator inbox
// instead — a miss, never a fault.
func TestLoopback_MissionReport_UnknownSessionIsNotLive(t *testing.T) {
	h := newLoopbackHarness(t)

	err := h.router.DeliverToContenoxSession(context.Background(), "cnx-nobody-owns-this",
		reportNotification("cnx-nobody-owns-this", "orphan report"))
	require.ErrorIs(t, err, ErrSessionNotLive)
}

// TestLoopback_MissionReport_IsPersistedIntoTheTranscript is the regression for
// the half of the bug that survived live delivery: a pushed update lives on the
// wire only, so an operator who reloaded after the report arrived saw a session
// with no trace of it. The report must be in the durable transcript too — which
// is also what puts it in the next turn's history, the whole point of delivering
// it to a session rather than to a screen.
func TestLoopback_MissionReport_IsPersistedIntoTheTranscript(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	contenoxID := h.contenoxSessionID(newResp.SessionID)
	require.NoError(t, h.router.DeliverToContenoxSession(ctx, contenoxID,
		reportNotification(contenoxID, "unit chain-acp reported (result): the README now names the runtime")))
	h.lc.drain(t, 1)

	history := h.history(t, contenoxID)
	require.Len(t, history, 1, "the delivered report is one assistant message in the transcript")
	require.Equal(t, "assistant", history[0].Role)
	require.Contains(t, history[0].Content, "the README now names the runtime")
}

// TestLoopback_SlashCommand_TurnIsPersisted is the regression for the vanishing
// session: a slash command used to be a wire event only, so a session whose
// FIRST message was `/mission …` came back from a reload with no messages at
// all — no title, no last-activity timestamp (so beam's freshest-first roster
// sorted it under every real chat), and an empty transcript. It looked deleted.
//
// /help stands in for the family here: it needs no fleet wiring, and the code
// path it takes through dispatchCommand is the one /mission takes.
func TestLoopback_SlashCommand_TurnIsPersisted(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	resp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("/help")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, resp.StopReason)
	h.lc.drain(t, 1)

	history := h.history(t, h.contenoxSessionID(newResp.SessionID))
	require.Len(t, history, 2, "a command turn is the line typed plus the answer given")
	require.Equal(t, "user", history[0].Role)
	require.Equal(t, "/help", history[0].Content, "the transcript shows what the operator actually typed")
	require.Equal(t, "assistant", history[1].Role)
	require.Contains(t, history[1].Content, "Available commands")
}

// TestLoopback_SlashCommand_HistoryRewritingCommandsAreNotPersisted guards the
// exclusion: /clear owns the transcript it just emptied, so appending the
// command that emptied it would leave behind the very line it promised to
// remove.
func TestLoopback_SlashCommand_HistoryRewritingCommandsAreNotPersisted(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	_, err = h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("/clear")},
	})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	require.Empty(t, h.history(t, h.contenoxSessionID(newResp.SessionID)),
		"/clear leaves an empty transcript, including of itself")
}

// TestLoopback_SessionList_CarriesMissionAttribution pins what tells a mission
// UNIT's session apart from the operator's own chats. A dispatched unit's
// session is created in the same workspace under the same identity, so without
// this attribution beam's sidebar showed it as an anonymous chat — and, having
// real messages while the session that fired it had none, listed it higher.
func TestLoopback_SessionList_CarriesMissionAttribution(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	// A dispatched unit's session/new — the kernel hands the unit its mission id
	// as opaque _meta (fleetservice.Dispatch), which is what binds the session to
	// the mission.
	unit, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       missionservice.MarshalMissionMeta("m-42"),
	})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	// An ordinary chat session alongside it.
	chat, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	list, err := h.client.ListSessions(ctx, libacp.ListSessionsRequest{})
	require.NoError(t, err)

	byID := make(map[libacp.SessionID]libacp.SessionInfo, len(list.Sessions))
	for _, s := range list.Sessions {
		byID[s.SessionID] = s
	}
	unitInfo, ok := byID[unit.SessionID]
	require.True(t, ok, "the unit session must be listed")
	missionID, found := missionservice.ParseMissionMeta(unitInfo.Meta)
	require.True(t, found, "a unit session must be identifiable as one from session/list alone")
	require.Equal(t, "m-42", missionID)

	chatInfo, ok := byID[chat.SessionID]
	require.True(t, ok)
	_, found = missionservice.ParseMissionMeta(chatInfo.Meta)
	require.False(t, found, "an ordinary chat carries no mission attribution")
}

// contenoxSessionID reads the internal contenox id bound to an ACP session — the
// id a mission names as its ParentSessionID, and the key both the SessionRouter
// and the transcript are addressed by.
func (h *loopbackHarness) contenoxSessionID(sid libacp.SessionID) string {
	h.tr.sessionMu.Lock()
	defer h.tr.sessionMu.Unlock()
	entry, ok := h.tr.sessions[sid]
	if !ok {
		return ""
	}
	return entry.InternalSessionID
}

// history reads a session's durable transcript, the same store session/load
// replays from.
func (h *loopbackHarness) history(t *testing.T, contenoxSessionID string) []taskengine.Message {
	t.Helper()
	mgr := chatservice.NewManager("loopback-ws")
	msgs, err := mgr.ListMessages(context.Background(), h.tr.deps.DB.WithoutTransaction(), contenoxSessionID)
	require.NoError(t, err)
	return msgs
}

// TestLoopback_MissionTurnAndReport_SurviveAReload is the whole reported
// symptom, end to end, at the seam it actually broke: fire a command as a
// session's FIRST act, take a report on it, then DROP the connection and come
// back on a fresh one — the browser reload.
//
// Before the fix this session came back empty: the command was a wire event and
// the report was a wire event, so nothing had ever been written, and the reload
// replayed nothing. Both must now be in the replay, in order.
func TestLoopback_MissionTurnAndReport_SurviveAReload(t *testing.T) {
	f := newInstancesFixture(t)
	ctx := context.Background()
	cwd := t.TempDir()

	c1 := f.connect()
	_, err := c1.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := c1.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: cwd, McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	c1.lc.drain(t, 1)

	// The operator's first act in the session is a slash command.
	_, err = c1.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("/help")},
	})
	require.NoError(t, err)
	c1.lc.drain(t, 1)

	// The unit reports home while they are still attached.
	contenoxID := contenoxSessionIDOf(t, c1.tr, newResp.SessionID)
	require.NoError(t, c1.tr.DeliverToContenoxSession(ctx, contenoxID,
		reportNotification(contenoxID, "unit chain-acp reported (result): mission complete")))
	c1.lc.drain(t, 1)

	// F5.
	c1.drop()
	c2 := f.connect()
	_, err = c2.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	_, err = c2.client.LoadSession(ctx, libacp.LoadSessionRequest{SessionID: newResp.SessionID, Cwd: cwd})
	require.NoError(t, err)

	var replayed []string
	for _, note := range c2.lc.drain(t, 3) {
		if note.Update.Content != nil {
			replayed = append(replayed, note.Update.Content.Text)
		}
	}
	joined := strings.Join(replayed, "\n")
	require.Contains(t, joined, "/help", "the reloaded session shows the command the operator typed")
	require.Contains(t, joined, "Available commands", "…and the answer it gave")
	require.Contains(t, joined, "mission complete", "…and the report that landed while they were attached")
}

// contenoxSessionIDOf reads the internal contenox id bound to sid on tr (white
// box, like externalDriverOf beside it) — the id a mission names as its parent.
func contenoxSessionIDOf(t *testing.T, tr *Transport, sid libacp.SessionID) string {
	t.Helper()
	tr.sessionMu.Lock()
	defer tr.sessionMu.Unlock()
	entry := tr.sessions[sid]
	require.NotNil(t, entry, "session %q not open on this connection", sid)
	return entry.InternalSessionID
}
