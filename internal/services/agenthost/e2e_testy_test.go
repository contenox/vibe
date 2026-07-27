package agenthost_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/contenox/beam/internal/services/agenthost"
	"github.com/contenox/beam/libacp"
	"github.com/contenox/beam/libacp/acpexec"
	"github.com/stretchr/testify/require"
)

// This file is the second leg of the ACP client-host e2e: it drives
// testy — the Rust reference SDK's deterministic conformance agent — through
// the composed registry → DriveTurn path, pinning that contenox's client-host
// role is spec-correct against an agent implementation contenox does not own.
// The tests are opt-in: testy is not vendored, so they skip unless
// ACP_TESTY_BIN points at a local build (see `task acp-client-e2e`).
//
// testy interop notes this file depends on (established in
// libacp/acpexec/e2e_testy_test.go, which drives testy at the lower libacp
// layer):
//   - testy's prompt text IS a JSON command ({"command":"echo",...},
//     {"command":"greet"}, ...) per testy.rs's TestyCommand; a plain-text
//     prompt is not part of its deterministic repertoire.
//   - testy echoes back whatever protocolVersion it is sent instead of
//     negotiating. DriveTurn always sends libacp.ProtocolVersion (1) and
//     checks equality, which works; version negotiation is deliberately not
//     exercised here.
//   - testy never exits on stdin-close, so every teardown takes the kill
//     path — a short KillGrace keeps that from adding acpexec's default 5s
//     grace to every test.
//   - testy's initialize response carries no agentInfo (optional per spec),
//     so nothing here asserts on it.
const hostTestyBinEnv = "ACP_TESTY_BIN"

// testyBinFromEnv gates a test on ACP_TESTY_BIN: skip (with pointer to the
// make target) when unset, fail hard when set but not accessible — a broken
// path must not masquerade as an environment without testy.
func testyBinFromEnv(t *testing.T) string {
	t.Helper()
	bin := os.Getenv(hostTestyBinEnv)
	if bin == "" {
		t.Skipf("skipping: set %s to a built testy binary to run (see `make acp-client-e2e`)", hostTestyBinEnv)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("%s=%q is not accessible: %v", hostTestyBinEnv, bin, err)
	}
	return bin
}

// testyCommandPrompt JSON-serializes v into the single text content block
// testy expects as its prompt.
func testyCommandPrompt(t *testing.T, v any) []libacp.ContentBlock {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return []libacp.ContentBlock{libacp.NewTextContent(string(raw))}
}

// TestHostE2E_Testy_EchoRoundTrip is the composed conformance round trip: a
// testy agents row created and resolved through the real registry service,
// handed to DriveTurn, spawns the reference agent and drives a full
// initialize → session/new → prompt turn. testy's echo command answers with
// exactly the message it was sent as a single agent_message_chunk, so the
// reply on the harness is byte-for-byte deterministic.
func TestHostE2E_Testy_EchoRoundTrip(t *testing.T) {
	requireSandboxable(t)
	testyBin := testyBinFromEnv(t)
	ctx, agent := registerAgent(t, "testy-echo", testyBin)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	const message = "the quick brown fox jumps over the lazy dog"
	var stderr acpexec.LockedBuffer
	harness := &agenthost.RecordingHarness{}
	res, err := agenthost.DriveTurn(ctx, agent, harness, agenthost.TurnRequest{
		Cwd:    t.TempDir(),
		Prompt: testyCommandPrompt(t, map[string]any{"command": "echo", "message": message}),
		Stderr: &stderr,
		// testy never exits on stdin-close; without a short grace every
		// teardown waits out acpexec's default 5s before killing it.
		KillGrace: 500 * time.Millisecond,
	})
	require.NoError(t, err, "testy stderr:\n%s", stderr.String())

	require.Equal(t, libacp.ProtocolVersion, res.Initialize.ProtocolVersion, "testy stderr:\n%s", stderr.String())
	require.NotEmpty(t, res.SessionID, "testy stderr:\n%s", stderr.String())
	require.Equal(t, libacp.StopReasonEndTurn, res.StopReason, "testy stderr:\n%s", stderr.String())

	// echo's whole reply is one agent_message_chunk carrying exactly the
	// message, on the driven session.
	require.Equal(t, message, harness.MessageText(), "testy stderr:\n%s", stderr.String())
	updates := harness.Updates()
	require.Len(t, updates, 1, "testy stderr:\n%s", stderr.String())
	require.Equal(t, res.SessionID, updates[0].SessionID)
	require.Equal(t, libacp.SessionUpdateAgentMessageChunk, updates[0].Update.SessionUpdate)

	tracker := &libacp.TurnTracker{}
	for _, n := range updates {
		tracker.Observe(n)
	}
	require.NoError(t, tracker.Err(res.StopReason), "testy stderr:\n%s", stderr.String())
}

// TestHostE2E_Testy_Greet drives testy's zero-argument greet command through
// the same composed path: its deterministic reply is the single message chunk
// "Hello, world!".
func TestHostE2E_Testy_Greet(t *testing.T) {
	requireSandboxable(t)
	testyBin := testyBinFromEnv(t)
	ctx, agent := registerAgent(t, "testy-greet", testyBin)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var stderr acpexec.LockedBuffer
	harness := &agenthost.RecordingHarness{}
	res, err := agenthost.DriveTurn(ctx, agent, harness, agenthost.TurnRequest{
		Cwd:       t.TempDir(),
		Prompt:    testyCommandPrompt(t, map[string]any{"command": "greet"}),
		Stderr:    &stderr,
		KillGrace: 500 * time.Millisecond,
	})
	require.NoError(t, err, "testy stderr:\n%s", stderr.String())

	require.Equal(t, libacp.StopReasonEndTurn, res.StopReason, "testy stderr:\n%s", stderr.String())
	require.Equal(t, "Hello, world!", harness.MessageText(), "testy stderr:\n%s", stderr.String())
}
