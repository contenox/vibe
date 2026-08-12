package agentinstance

import (
	"strings"
	"testing"

	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// TestUnit_DeliverToSession_UnknownSessionNotFound pins that a Manager
// hosting no owning instance reports ErrNotFound, while an empty id is a
// plain argument error.
func TestUnit_DeliverToSession_UnknownSessionNotFound(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	err := mgr.DeliverToSession(ctx, "no-such-session",
		libacp.SessionNotification{Update: libacp.NewAgentMessageChunk("hi")})
	require.ErrorIs(t, err, ErrNotFound, "no live instance owns the session → ErrNotFound (the inbox-fallback signal)")

	err = mgr.DeliverToSession(ctx, "",
		libacp.SessionNotification{Update: libacp.NewAgentMessageChunk("hi")})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotFound, "an empty id is an argument error, not a missing session")
}

// TestManager_DeliverToSession_InjectsIntoSessionStream pins that an
// out-of-band update reaches every attached viewer and is journaled, so a
// later viewer replays it too.
func TestManager_DeliverToSession_InjectsIntoSessionStream(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternal(t, ctx, svc, "ext-agent", stub)

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)
	sid := openSession(t, mgr, id)

	viewer := newMockViewer("supervisor")
	_, err = mgr.Attach(ctx, id, sid, viewer)
	require.NoError(t, err)

	const reportLine = "unit runner reported (result): shipped the board"
	err = mgr.DeliverToSession(ctx, sid,
		libacp.SessionNotification{Update: libacp.NewAgentMessageChunk(reportLine)})
	require.NoError(t, err)

	require.True(t, viewerReported(viewer, reportLine),
		"the attached viewer receives the injected report update")
	requireDeliveredWithSessionID(t, viewer, reportLine, sid)

	late := newMockViewer("late-supervisor")
	_, err = mgr.Attach(ctx, id, sid, late)
	require.NoError(t, err)
	require.True(t, viewerReported(late, reportLine),
		"a later viewer replays the journaled report update")
}

func requireDeliveredWithSessionID(t *testing.T, v *mockViewer, substr string, sid libacp.SessionID) {
	t.Helper()
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, n := range v.updates {
		if n.Update.SessionUpdate != libacp.SessionUpdateAgentMessageChunk {
			continue
		}
		if c := n.Update.Content; c != nil && strings.Contains(c.Text, substr) {
			require.Equal(t, sid, n.SessionID, "injected update carries the owning session id")
			return
		}
	}
	t.Fatalf("no delivered agent_message_chunk contained %q", substr)
}
