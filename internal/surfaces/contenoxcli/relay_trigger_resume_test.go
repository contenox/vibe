package contenoxcli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/librelay"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

type fakeCheckpointStore struct {
	rows map[string]*runtimetypes.ChainCheckpoint
}

func (f fakeCheckpointStore) GetChainCheckpoint(_ context.Context, id string) (*runtimetypes.ChainCheckpoint, error) {
	row, ok := f.rows[id]
	if !ok {
		return nil, libdb.ErrNotFound
	}
	return row, nil
}

func checkpointOwing(requestID, sessionID string) fakeCheckpointStore {
	payload := []byte(`{"checkpoint":{"approvalId":"ask-1"}}`)
	if requestID != "" {
		payload = []byte(`{"trigger_request_id":"` + requestID + `","checkpoint":{"approvalId":"ask-1"}}`)
	}
	return fakeCheckpointStore{rows: map[string]*runtimetypes.ChainCheckpoint{
		"ask-1": {ID: "ask-1", SessionID: sessionID, Payload: payload},
	}}
}

func testResumeBridge(t *testing.T) (*relayResumeBridge, *resultRecorder) {
	t.Helper()
	rec := newResultRecorder()
	b := newRelayResumeBridge(libtracker.NoopTracker{})
	b.attach("inst-test", rec.send)
	return b, rec
}

func TestUnit_RelayResumeBridge_ReportsWhatTheResumedRunDidUnderTheOriginalRequestID(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		resp        *agentservice.PromptResponse
		resumeErr   error
		wantStatus  string
		wantMention string
	}{
		"a run that finished": {
			resp:       &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn},
			wantStatus: librelay.ChainTriggerStatusOK,
		},
		"a run that parked on a second human": {
			resp:        &agentservice.PromptResponse{StopReason: agentservice.StopSuspended, SuspendedApprovalID: "ask-2"},
			wantStatus:  librelay.ChainTriggerStatusAwaitingHuman,
			wantMention: "ask-2",
		},
		"a run that failed after resuming": {
			resp:        &agentservice.PromptResponse{StopReason: agentservice.StopFailed},
			resumeErr:   errors.New("provider returned 500"),
			wantStatus:  librelay.ChainTriggerStatusError,
			wantMention: "provider returned 500",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, rec := testResumeBridge(t)
			hook := b.hookWith(checkpointOwing("dispatch/9", "sid-1"),
				func(context.Context, string) (*agentservice.PromptResponse, error) {
					return tc.resp, tc.resumeErr
				})

			err := hook(t.Context(), "ask-1")
			if tc.resumeErr != nil {
				require.ErrorIs(t, err, tc.resumeErr)
			} else {
				require.NoError(t, err)
			}

			res := requireResult(t, rec.next(t), "dispatch/9", tc.wantStatus)
			if tc.wantMention != "" {
				require.Contains(t, res.Error, tc.wantMention)
			}
			rec.noneYet(t)
			require.True(t, b.runs.idle(), "the session gate is released with the resume")
		})
	}
}

func TestUnit_RelayResumeBridge_SendsNothingWhenNobodyIsOwedAnOutcome(t *testing.T) {
	t.Parallel()
	done := func(context.Context, string) (*agentservice.PromptResponse, error) {
		return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
	}

	for name, tc := range map[string]struct {
		store   checkpointRowReader
		resume  resumeRun
		wantErr error
	}{
		"a run no trigger started": {
			store:  checkpointOwing("", "sid-1"),
			resume: done,
		},
		"an approval with no checkpoint at all": {
			store:  fakeCheckpointStore{},
			resume: done,
		},
		"a checkpoint another resumer holds": {
			store: checkpointOwing("dispatch/9", "sid-1"),
			resume: func(context.Context, string) (*agentservice.PromptResponse, error) {
				return nil, agentservice.ErrNoCheckpoint
			},
			wantErr: hitlservice.ErrNoCheckpoint,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, rec := testResumeBridge(t)
			err := b.hookWith(tc.store, tc.resume)(t.Context(), "ask-1")
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			rec.noneYet(t)
		})
	}
}

func TestUnit_RelayResumeBridge_ADetachedLinkLosesNothingElse(t *testing.T) {
	t.Parallel()
	b, rec := testResumeBridge(t)
	b.detach()
	err := b.hookWith(checkpointOwing("dispatch/9", "sid-1"),
		func(context.Context, string) (*agentservice.PromptResponse, error) {
			return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
		})(t.Context(), "ask-1")
	require.NoError(t, err, "an undeliverable outcome must not fail the verdict that produced it")
	rec.noneYet(t)
}

func TestUnit_RelayResumeBridge_AResumedRunAndANewFiringNeverRunAtOnceInOneSession(t *testing.T) {
	dir := triggerTestWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chain-on-event.json"), []byte(`{"id":"c","tasks":[]}`), 0o600))

	b, rec := testResumeBridge(t)
	agent := &fakeChainAgent{}
	runner := &relayTriggerRunner{agent: agent, contenoxDir: dir, resumes: b}

	resuming := make(chan struct{})
	proceed := make(chan struct{})
	hook := b.hookWith(checkpointOwing("dispatch/9", "sid-1"),
		func(context.Context, string) (*agentservice.PromptResponse, error) {
			close(resuming)
			<-proceed
			return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
		})

	resumed := make(chan error, 1)
	go func() { resumed <- hook(context.Background(), "ask-1") }()
	<-resuming

	fired := make(chan error, 1)
	go func() {
		_, err := runner.RunChain(context.Background(), relayChainRequest{
			RequestID: "dispatch/10", Chain: "chain-on-event.json",
			SessionMode: librelay.ChainSessionReused, SessionName: "refund-desk",
			Input: json.RawMessage(`{"hop":0}`),
		})
		fired <- err
	}()

	require.Eventually(t, func() bool {
		created, _, _, _, _ := agent.sessionOps()
		return len(created) == 1
	}, 5*time.Second, 5*time.Millisecond, "the firing resolves its session before it waits for the session's turn")
	_, _, calls := agent.last()
	require.Zero(t, calls, "a firing must not run in a session whose parked run is resuming")

	close(proceed)
	require.NoError(t, <-resumed)
	require.NoError(t, <-fired)
	_, _, calls = agent.last()
	require.Equal(t, 1, calls)
	require.True(t, b.runs.idle())

	requireResult(t, rec.next(t), "dispatch/9", librelay.ChainTriggerStatusOK)
}

func TestUnit_RelayTriggerRunner_CarriesTheTriggerRequestIDIntoTheRun(t *testing.T) {
	dir := triggerTestWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chain-on-event.json"), []byte(`{"id":"c","tasks":[]}`), 0o600))

	agent := &fakeChainAgent{}
	runner := &relayTriggerRunner{agent: agent, contenoxDir: dir}
	_, err := runner.RunChain(t.Context(), relayChainRequest{
		RequestID: "dispatch/11", Chain: "chain-on-event.json", Input: json.RawMessage(`{"hop":0}`),
	})
	require.NoError(t, err)
	ctx, _, _ := agent.last()
	require.Equal(t, "dispatch/11", agentservice.TriggerRequestIDFromContext(ctx),
		"the run carries who is owed its outcome, so its checkpoint does too")

	h, rec := testTriggerHandler(relayChainTriggers{runner: runner})
	h.handle(t.Context(), chainTriggerFrame(t, librelay.ChainTrigger{
		RequestID: "dispatch/12", Chain: "chain-on-event.json", SessionMode: librelay.ChainSessionNew,
		Input: json.RawMessage(`{"hop":0}`),
	}))
	requireResult(t, rec.next(t), "dispatch/12", librelay.ChainTriggerStatusOK)
	ctx, _, _ = agent.last()
	require.Equal(t, "dispatch/12", agentservice.TriggerRequestIDFromContext(ctx),
		"the frame's request_id is what the handler hands the runner")
}
