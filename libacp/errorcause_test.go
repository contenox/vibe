package libacp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnit_AsErrorPreservesTimeoutClassification(t *testing.T) {
	err := libacp.AsError(context.DeadlineExceeded)
	require.True(t, libacp.IsTimeoutError(err), "timeout must survive AsError")
	require.True(t, libacp.IsRetryableError(err), "timeout must stay retryable")
	require.ErrorIs(t, err, context.DeadlineExceeded, "in-process cause must be recoverable")
}

func TestUnit_AsErrorPreservesWrappedTransportCause(t *testing.T) {
	err := libacp.AsError(fmt.Errorf("write update: %w", io.EOF))
	require.ErrorIs(t, err, io.EOF)
	require.True(t, libacp.IsRetryableError(err), "dropped transport must stay retryable")
	require.Equal(t, libacp.ErrInternalError, err.Code, "only deadlines get a dedicated code")
}

func TestUnit_AsErrorLeavesCancellationNonRetryable(t *testing.T) {
	err := libacp.AsError(context.Canceled)
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, libacp.IsRetryableError(err), "an explicit cancellation is a decision, not a fault")
}

// The wire contract is code/message/data and nothing else: a cause carried for
// local matching must not leak into the serialized form, or a peer would see a
// field it never agreed to.
func TestUnit_ErrorWireFormatUnchangedByCause(t *testing.T) {
	raw, err := json.Marshal(libacp.AsError(context.DeadlineExceeded))
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.ElementsMatch(t, []string{"code", "message"}, keysOf(fields))

	raw, err = json.Marshal(libacp.NewError(libacp.ErrInvalidParams, "bad"))
	require.NoError(t, err)
	require.JSONEq(t, `{"code":-32602,"message":"bad"}`, string(raw))
}

// After a real serialize/parse the Go sentinel is gone for good; only the code
// can still carry the verdict, which is why AsError promotes deadlines.
func TestUnit_TimeoutSurvivesSerializationRoundTrip(t *testing.T) {
	raw, err := json.Marshal(libacp.AsError(context.DeadlineExceeded))
	require.NoError(t, err)

	var remote *libacp.Error
	require.NoError(t, json.Unmarshal(raw, &remote))
	require.Nil(t, errors.Unwrap(remote), "a decoded error cannot have a cause")
	assert.True(t, libacp.IsTimeoutError(remote), "code must carry the verdict across the wire")
	assert.True(t, libacp.IsRetryableError(remote))
}

// timingOutAgent's Prompt fails on its *own* deadline rather than on the
// client's session/cancel — the case a supervisor must be able to retry.
type timingOutAgent struct {
	libacp.UnimplementedAgent
	started chan struct{}
}

func (a *timingOutAgent) Initialize(_ context.Context, _ libacp.InitializeRequest) (libacp.InitializeResponse, error) {
	return libacp.InitializeResponse{ProtocolVersion: libacp.ProtocolVersion}, nil
}

func (a *timingOutAgent) NewSession(_ context.Context, _ libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
	return libacp.NewSessionResponse{SessionID: "sess-1"}, nil
}

func (a *timingOutAgent) Prompt(ctx context.Context, _ libacp.PromptRequest) (libacp.PromptResponse, error) {
	close(a.started)
	own, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	<-own.Done()
	return libacp.PromptResponse{}, own.Err()
}

// Pins the boundary of conn.go's cancellation special case: only a prompt
// cancelled by the *client* resolves as StopReasonCancelled. A prompt that ran
// out of time on the agent's own deadline is a genuine failure and must reach
// the client as a retryable error, not as a silent cancelled turn.
func TestUnit_PromptOwnDeadlineIsRetryableErrorNotCancelledStop(t *testing.T) {
	agent := &timingOutAgent{started: make(chan struct{})}
	h := newCancelHarness(t, agent)

	h.send(libacp.MethodSessionPrompt, 1, promptParams("sess-1"))
	select {
	case <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("prompt never started")
	}

	resp := h.expectResponse(1)
	require.NotNil(t, resp.Error, "an agent-side deadline is a failure, not a cancelled turn")
	require.Nil(t, resp.Result)
	assert.Equal(t, libacp.ErrRequestTimeout, resp.Error.Code)
	assert.True(t, libacp.IsRetryableError(resp.Error), "supervisor must restart, not give up")
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
