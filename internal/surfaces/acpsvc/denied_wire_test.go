package acpsvc

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/localtools"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// TestLoopback_DeniedToolCall_WireShowsFailedExactlyOnce pins the forensic
// regression (beam-fe7fa151): a HITL-denied call closes with exactly one
// terminal update, status failed, carrying the denial reason.
func TestLoopback_DeniedToolCall_WireShowsFailedExactlyOnce(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1) // deferred available_commands_update

	fake := &loopbackAgent{}
	fake.promptFunc = func(ctx context.Context, req agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
		reqID, _ := ctx.Value(libtracker.ContextKeyRequestID).(string)
		require.NotEmpty(t, reqID)
		subject := taskengine.TaskEventRequestSubject(reqID)
		publish := func(ev taskengine.TaskEvent) {
			raw, mErr := json.Marshal(ev)
			require.NoError(t, mErr)
			require.NoError(t, h.bus.Publish(ctx, subject, raw))
		}
		publish(taskengine.TaskEvent{
			Kind:         taskengine.TaskEventToolCallPending,
			ToolName:     "git.git_restore",
			ApprovalID:   "call-denied-1",
			ApprovalArgs: map[string]any{"paths": []any{"."}},
		})
		// The HITL wrapper's denial: sentinel result, no error, no execution.
		publish(taskengine.TaskEvent{
			Kind:         taskengine.TaskEventToolCall,
			ToolName:     "git.git_restore",
			ApprovalID:   "call-denied-1",
			ApprovalArgs: map[string]any{"paths": []any{"."}},
			Content:      localtools.DenyMessage,
		})
		return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
	}
	h.swapAgent(newResp.SessionID, fake)

	_, err = h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("restore it")},
	})
	require.NoError(t, err)

	// session_info + pending card + terminal update, nothing else.
	updates := h.lc.drain(t, 3)

	var terminal []libacp.SessionUpdate
	for _, u := range updates {
		if u.Update.ToolCallID != "call-denied-1" {
			continue
		}
		switch u.Update.Status {
		case libacp.ToolCallStatusCompleted, libacp.ToolCallStatusFailed:
			terminal = append(terminal, u.Update)
		}
	}
	require.Len(t, terminal, 1, "exactly one terminal update per denied call on one connection")
	require.Equal(t, libacp.ToolCallStatusFailed, terminal[0].Status,
		"a denied call must never reach the client as completed")
	require.JSONEq(t, `{"error":`+jsonString(localtools.DenyMessage)+`}`, string(terminal[0].Meta))
	require.JSONEq(t, jsonString(localtools.DenyMessage), string(terminal[0].RawOutput))
}

// inertRWC backs an AgentSideConnection that is never Run: writes are
// discarded, reads never happen.
type inertRWC struct{}

func (inertRWC) Read([]byte) (int, error)    { <-make(chan struct{}); return 0, io.EOF }
func (inertRWC) Write(p []byte) (int, error) { return len(p), nil }
func (inertRWC) Close() error                { return nil }

func newMirrorProbeTransport(contenoxID string, sid libacp.SessionID) *Transport {
	b := &Transport{
		connectionID:    "probe-" + string(sid),
		sessions:        make(map[libacp.SessionID]*sessionEntry),
		contenoxToACPID: map[string]libacp.SessionID{contenoxID: sid},
	}
	// Disarm the pump so queued mirror items stay observable.
	b.mirrorOnce.Do(func() {})
	b.mirrorCh = make(chan mirrorItem, 8)
	return b
}

// TestUnit_Mirror_ExactlyOncePerConnection pins one delivery per connection:
// replay is never mirrored; a viewer-attached holder is not mirrored to.
func TestUnit_Mirror_ExactlyOncePerConnection(t *testing.T) {
	router := NewSessionRouter()

	origin := &Transport{
		connectionID:    "origin",
		sessions:        map[libacp.SessionID]*sessionEntry{"sid-a": {InternalSessionID: "cx-1"}},
		contenoxToACPID: map[string]libacp.SessionID{"cx-1": "sid-a"},
		deps:            Deps{SessionRouter: router},
	}
	origin.conn = libacp.NewAgentSideConnection(inertRWC{}, func(*libacp.AgentSideConnection) libacp.Agent {
		return libacp.UnimplementedAgent{}
	})
	other := newMirrorProbeTransport("cx-1", "sid-b")

	router.bind("cx-1", origin)
	router.bind("cx-1", other)

	ctx := context.Background()
	live := libacp.SessionNotification{
		SessionID: "sid-a",
		Update:    libacp.NewAgentMessageChunk("live chunk"),
	}

	// Replay traffic is addressed to the loading connection alone.
	origin.replayMessages(ctx, "sid-a", []taskengine.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}, false)
	require.Empty(t, other.mirrorCh, "a replay mirrored to an established holder re-delivers history")

	// A live update still reaches the second holder exactly once.
	origin.sendUpdate(ctx, live)
	require.Len(t, other.mirrorCh, 1)
	<-other.mirrorCh

	// A holder watching the session's native turn through a viewer gets the
	// stream from the viewer fan-out; the mirror must not double it.
	other.markNativeViewing("sid-b")
	origin.sendUpdate(ctx, live)
	require.Empty(t, other.mirrorCh)

	other.unmarkNativeViewing("sid-b")
	origin.sendUpdate(ctx, live)
	require.Len(t, other.mirrorCh, 1)
}
