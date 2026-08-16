package contenoxcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/eventtrigger"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/librelay"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

type resultRecorder struct {
	frames chan librelay.Frame
}

func newResultRecorder() *resultRecorder {
	return &resultRecorder{frames: make(chan librelay.Frame, 8)}
}

func (r *resultRecorder) send(f librelay.Frame) error {
	r.frames <- f
	return nil
}

func (r *resultRecorder) next(t *testing.T) librelay.Frame {
	t.Helper()
	select {
	case f := <-r.frames:
		return f
	case <-time.After(10 * time.Second):
		t.Fatal("no frame was sent")
		return librelay.Frame{}
	}
}

func (r *resultRecorder) noneYet(t *testing.T) {
	t.Helper()
	select {
	case f := <-r.frames:
		t.Fatalf("unexpected frame sent: %+v", f)
	default:
	}
}

type stubChainRunner struct {
	mu    sync.Mutex
	err   error
	gate  chan struct{}
	calls int
	last  relayChainRequest
}

func (s *stubChainRunner) RunChain(_ context.Context, req relayChainRequest) error {
	s.mu.Lock()
	s.calls++
	s.last = req
	gate, err := s.gate, s.err
	s.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return err
}

func (s *stubChainRunner) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func testTriggerHandler(triggers relayChainTriggers) (*relayTriggerHandler, *resultRecorder) {
	rec := newResultRecorder()
	h := newRelayTriggerHandler(triggers, libtracker.NoopTracker{})
	h.instance = "inst-test"
	h.send = rec.send
	return h, rec
}

func chainTriggerFrame(t *testing.T, payload librelay.ChainTrigger) librelay.Frame {
	t.Helper()
	f, err := librelay.Frame{Type: librelay.TypeChainTrigger, Instance: "inst-test"}.WithPayload(payload)
	require.NoError(t, err)
	return f
}

func requireResult(t *testing.T, f librelay.Frame, requestID, status string) librelay.ChainTriggerResult {
	t.Helper()
	require.Equal(t, librelay.TypeChainTriggerResult, f.Type)
	require.Equal(t, "inst-test", f.Instance)
	require.Empty(t, f.Session)
	var res librelay.ChainTriggerResult
	require.NoError(t, f.DecodePayload(&res))
	require.Equal(t, requestID, res.RequestID)
	require.Equal(t, status, res.Status)
	return res
}

// TestUnit_RelayChainTrigger_RefusesWhatCannotStart asserts each refusal path answers with exactly one refused result carrying the reason, and no chain ever runs.
func TestUnit_RelayChainTrigger_RefusesWhatCannotStart(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		triggers    relayChainTriggers
		payload     librelay.ChainTrigger
		wantMention string
	}{
		"no runner refuses with the default reason": {
			triggers:    relayChainTriggers{},
			payload:     librelay.ChainTrigger{RequestID: "req-1", Chain: "chain-x.json", SessionMode: "new", Input: json.RawMessage(`{}`)},
			wantMention: "not available",
		},
		"no runner refuses with the surface's reason": {
			triggers:    relayChainTriggers{unavailable: "event triggers are beta-gated off on this machine"},
			payload:     librelay.ChainTrigger{RequestID: "req-2", Chain: "chain-x.json", SessionMode: "new", Input: json.RawMessage(`{}`)},
			wantMention: "beta-gated",
		},
		"a trigger naming no chain": {
			triggers:    relayChainTriggers{runner: &stubChainRunner{}},
			payload:     librelay.ChainTrigger{RequestID: "req-3", SessionMode: "new", Input: json.RawMessage(`{}`)},
			wantMention: "chain is required",
		},
		"an unknown session mode": {
			triggers:    relayChainTriggers{runner: &stubChainRunner{}},
			payload:     librelay.ChainTrigger{RequestID: "req-5", Chain: "chain-x.json", SessionMode: "clone", Input: json.RawMessage(`{}`)},
			wantMention: `"clone"`,
		},
		"a session name carrying a control character": {
			triggers: relayChainTriggers{runner: &stubChainRunner{}},
			payload: librelay.ChainTrigger{RequestID: "req-6", Chain: "chain-x.json", SessionMode: librelay.ChainSessionReused,
				SessionName: "refund\ndesk", Input: json.RawMessage(`{}`)},
			wantMention: "malformed session_name",
		},
		"a session name that is only whitespace": {
			triggers: relayChainTriggers{runner: &stubChainRunner{}},
			payload: librelay.ChainTrigger{RequestID: "req-7", Chain: "chain-x.json", SessionMode: librelay.ChainSessionReused,
				SessionName: "   ", Input: json.RawMessage(`{}`)},
			wantMention: "malformed session_name",
		},
		"a session name past the column width": {
			triggers: relayChainTriggers{runner: &stubChainRunner{}},
			payload: librelay.ChainTrigger{RequestID: "req-8", Chain: "chain-x.json", SessionMode: librelay.ChainSessionReused,
				SessionName: strings.Repeat("n", maxChainSessionNameBytes+1), Input: json.RawMessage(`{}`)},
			wantMention: "malformed session_name",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, rec := testTriggerHandler(tc.triggers)
			h.handle(t.Context(), chainTriggerFrame(t, tc.payload))
			res := requireResult(t, rec.next(t), tc.payload.RequestID, librelay.ChainTriggerStatusRefused)
			require.Contains(t, res.Error, tc.wantMention)
			rec.noneYet(t)
			if s, ok := tc.triggers.runner.(*stubChainRunner); ok {
				require.Zero(t, s.callCount(), "a refused trigger must never reach the runner")
			}
		})
	}
}

func TestUnit_RelayChainTrigger_CarriesTheSessionToTheRunner(t *testing.T) {
	t.Parallel()
	runner := &stubChainRunner{}
	h, rec := testTriggerHandler(relayChainTriggers{runner: runner})
	h.handle(t.Context(), chainTriggerFrame(t, librelay.ChainTrigger{
		RequestID:   "req-1",
		Chain:       "chain-x.json",
		SessionMode: librelay.ChainSessionReused,
		SessionName: "  refund-desk  ",
		Input:       json.RawMessage(`{}`),
	}))
	requireResult(t, rec.next(t), "req-1", librelay.ChainTriggerStatusOK)

	runner.mu.Lock()
	defer runner.mu.Unlock()
	require.Equal(t, librelay.ChainSessionReused, runner.last.SessionMode)
	require.Equal(t, "refund-desk", runner.last.SessionName, "the wire name is trimmed before it becomes a row value")
}

// TestUnit_RelayChainTrigger_UnanswerableFramesSendNoResult asserts a frame with no well-formed request_id gets no result, except a request-shaped frame still owes its one librelay error reply.
func TestUnit_RelayChainTrigger_UnanswerableFramesSendNoResult(t *testing.T) {
	t.Parallel()
	runner := &stubChainRunner{}
	h, rec := testTriggerHandler(relayChainTriggers{runner: runner})

	foreign := chainTriggerFrame(t, librelay.ChainTrigger{
		RequestID: "req-1", Chain: "chain-x.json", SessionMode: "new", Input: json.RawMessage(`{}`),
	})
	foreign.Instance = "inst-other"

	for name, f := range map[string]librelay.Frame{
		"another instance's trigger": foreign,
		"an undecodable payload":     {Type: librelay.TypeChainTrigger, Instance: "inst-test", Payload: json.RawMessage(`{"request_id":5}`)},
		"a missing request_id":       chainTriggerFrame(t, librelay.ChainTrigger{Chain: "chain-x.json", SessionMode: "new", Input: json.RawMessage(`{}`)}),
		"a control-character request_id": chainTriggerFrame(t, librelay.ChainTrigger{
			RequestID: "req\n1", Chain: "chain-x.json", SessionMode: "new", Input: json.RawMessage(`{}`),
		}),
	} {
		t.Run(name, func(t *testing.T) {
			h.handle(t.Context(), f)
			rec.noneYet(t)
			require.Zero(t, runner.callCount())
		})
	}

	// A malformed trigger that is a request still gets exactly one reply: the codec-level error, never a result naming a request_id nobody sent.
	bad := librelay.Frame{Type: librelay.TypeChainTrigger, Instance: "inst-test", ID: "42", Payload: json.RawMessage(`{"request_id":5}`)}
	h.handle(t.Context(), bad)
	reply := rec.next(t)
	require.Equal(t, librelay.TypeError, reply.Type)
	require.Equal(t, "42", reply.ReplyTo)
	var e librelay.Error
	require.NoError(t, reply.DecodePayload(&e))
	require.Equal(t, librelay.CodeMalformedFrame, e.Code)
	rec.noneYet(t)
}

// TestUnit_RelayChainTrigger_RunsOffTheReadLoopExactlyOnce asserts handle returns while the chain still runs, a re-delivered request_id does not start a second run while the first owes its result, and completion frees the id again.
func TestUnit_RelayChainTrigger_RunsOffTheReadLoopExactlyOnce(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	runner := &stubChainRunner{gate: gate}
	h, rec := testTriggerHandler(relayChainTriggers{runner: runner})
	f := chainTriggerFrame(t, librelay.ChainTrigger{
		RequestID: "req-1", Chain: "chain-x.json", SessionMode: "new", Input: json.RawMessage(`{}`),
	})

	// handle returning while the runner is parked on the gate IS the non-blocking property.
	h.handle(t.Context(), f)
	rec.noneYet(t)

	h.handle(t.Context(), f)
	rec.noneYet(t)

	close(gate)
	requireResult(t, rec.next(t), "req-1", librelay.ChainTriggerStatusOK)
	rec.noneYet(t)
	require.Equal(t, 1, runner.callCount(), "a re-delivered request_id must not run twice")

	// The in-flight claim ends with the run: joining the goroutine makes the release observable.
	h.wait()
	h.handle(t.Context(), f)
	requireResult(t, rec.next(t), "req-1", librelay.ChainTriggerStatusOK)
	require.Equal(t, 2, runner.callCount())
}

// TestUnit_RelayChainTrigger_ClassifiesRunOutcomes asserts a runner refusal answers refused and a started-then-failed run answers error, with the reason on the wire either way.
func TestUnit_RelayChainTrigger_ClassifiesRunOutcomes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		runErr     error
		wantStatus string
	}{
		"a refusal from inside the runner": {
			runErr:     refuseChainTrigger(errors.New("no such chain")),
			wantStatus: librelay.ChainTriggerStatusRefused,
		},
		"a chain that started and failed": {
			runErr:     errors.New("provider returned 500"),
			wantStatus: librelay.ChainTriggerStatusError,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, rec := testTriggerHandler(relayChainTriggers{runner: &stubChainRunner{err: tc.runErr}})
			h.handle(t.Context(), chainTriggerFrame(t, librelay.ChainTrigger{
				RequestID: "req-1", Chain: "chain-x.json", SessionMode: "new", Input: json.RawMessage(`{}`),
			}))
			res := requireResult(t, rec.next(t), "req-1", tc.wantStatus)
			require.Contains(t, res.Error, tc.runErr.Error())
			rec.noneYet(t)
		})
	}
}

type fakeChainAgent struct {
	mu      sync.Mutex
	err     error
	newErr  error
	calls   int
	lastCtx context.Context
	lastReq agentservice.PromptRequest

	listDelay   time.Duration
	sessions    []*agentservice.SessionInfo
	created     []string
	resumed     []string
	lists       int
	promptSeen  []string
	promptNames []string
}

func (f *fakeChainAgent) Prompt(ctx context.Context, req agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastCtx = ctx
	f.lastReq = req
	f.promptSeen = append(f.promptSeen, req.SessionID)
	for _, s := range f.sessions {
		if s.ID == req.SessionID {
			f.promptNames = append(f.promptNames, s.Name)
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
}

func (f *fakeChainAgent) last() (context.Context, agentservice.PromptRequest, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastCtx, f.lastReq, f.calls
}

func (f *fakeChainAgent) sessionOps() (created, resumed, promptSeen, promptNames []string, lists int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.created...), append([]string(nil), f.resumed...),
		append([]string(nil), f.promptSeen...), append([]string(nil), f.promptNames...), f.lists
}

func (f *fakeChainAgent) Capabilities(context.Context) (*agentservice.AgentCapabilities, error) {
	return &agentservice.AgentCapabilities{}, nil
}

func (f *fakeChainAgent) SessionNew(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newErr != nil {
		return "", f.newErr
	}
	f.created = append(f.created, name)
	id := fmt.Sprintf("sid-%d", len(f.sessions)+1)
	f.sessions = append(f.sessions, &agentservice.SessionInfo{ID: id, Name: name})
	return id, nil
}

func (f *fakeChainAgent) SessionList(context.Context) ([]*agentservice.SessionInfo, error) {
	f.mu.Lock()
	f.lists++
	out := append([]*agentservice.SessionInfo(nil), f.sessions...)
	delay := f.listDelay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	return out, nil
}

func (f *fakeChainAgent) SessionLoad(context.Context, string) (string, []taskengine.Message, error) {
	return "", nil, nil
}

func (f *fakeChainAgent) SessionResume(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed = append(f.resumed, name)
	for _, s := range f.sessions {
		if s.Name == name {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("session %q not found", name)
}
func (f *fakeChainAgent) SessionDelete(context.Context, string) error          { return nil }
func (f *fakeChainAgent) SessionEnsureDefault(context.Context) (string, error) { return "", nil }

var _ agentservice.Agent = (*fakeChainAgent)(nil)

func triggerTestWorkspace(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return t.TempDir()
}

// TestUnit_RelayTriggerRunner_HappyPathThroughTheHandler drives a trigger frame end to end: the chain resolves on the machine's path, runs at hop+1 under the payload's policy and request_id, and the ok result answers the frame.
func TestUnit_RelayTriggerRunner_HappyPathThroughTheHandler(t *testing.T) {
	dir := triggerTestWorkspace(t)
	chainPath := filepath.Join(dir, "chain-on-event.json")
	require.NoError(t, os.WriteFile(chainPath, []byte(`{"id":"on-event","tasks":[]}`), 0o600))

	agent := &fakeChainAgent{}
	h, rec := testTriggerHandler(relayChainTriggers{runner: &relayTriggerRunner{
		agent:       agent,
		opts:        chatOpts{EffectiveDefaultModel: "test-model", EffectiveContext: 4096},
		contenoxDir: dir,
	}})

	input := `{"nid":7,"workspace_id":"ws","type":"missionservice.events.report_added","hop":2,"data":{"missionId":"m-1"}}`
	f := chainTriggerFrame(t, librelay.ChainTrigger{
		RequestID:   "req-run-1",
		Chain:       "chain-on-event.json",
		SessionMode: librelay.ChainSessionNew,
		Input:       json.RawMessage(input),
		Policy:      "hitl-policy-strict.json",
	})
	f.ID = "42"
	h.handle(t.Context(), f)

	result := rec.next(t)
	requireResult(t, result, "req-run-1", librelay.ChainTriggerStatusOK)
	require.Equal(t, "42", result.ReplyTo, "a trigger sent as a request is answered by its result")
	rec.noneYet(t)

	ctx, req, calls := agent.last()
	require.Equal(t, 1, calls)
	require.Equal(t, input, req.Input, "the chain receives the event JSON verbatim")
	require.Equal(t, taskengine.DataTypeJSON, req.InputType)
	require.Equal(t, chainPath, req.ChainRef, "the chain resolved on this machine's path, never from the wire")
	require.Equal(t, "on-event", req.Chain.ID)
	inputValue, ok := req.InputValue.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "missionservice.events.report_added", inputValue["type"])
	require.Equal(t, "test-model", req.TemplateVars["model"])
	require.Equal(t, 4096, req.ContextLength)
	require.NotEmpty(t, req.SessionID, "the event lands in a session, not beside one")

	require.Equal(t, 3, runtimetypes.EventHopFromContext(ctx), "the run executes at the event's hop+1")
	require.Equal(t, "hitl-policy-strict.json", hitlservice.PolicyNameFromContext(ctx))
	require.Equal(t, "req-run-1", ctx.Value(libtracker.ContextKeyRequestID), "the frame's request_id is the run's correlation key")
}

// TestUnit_RelayTriggerRunner_DefaultPolicyWhenPayloadNamesNone asserts no policy in the payload means no context override, hitlservice's standard resolution.
func TestUnit_RelayTriggerRunner_DefaultPolicyWhenPayloadNamesNone(t *testing.T) {
	dir := triggerTestWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chain-on-event.json"), []byte(`{"id":"c","tasks":[]}`), 0o600))

	agent := &fakeChainAgent{}
	runner := &relayTriggerRunner{agent: agent, contenoxDir: dir}
	require.NoError(t, runner.RunChain(t.Context(), relayChainRequest{
		Chain: "chain-on-event.json",
		Input: json.RawMessage(`{"hop":0}`),
	}))
	ctx, _, _ := agent.last()
	require.Empty(t, hitlservice.PolicyNameFromContext(ctx))
	require.Equal(t, 1, runtimetypes.EventHopFromContext(ctx), "a hopless envelope still runs at hop 1")
}

// TestUnit_RelayTriggerRunner_RefusesBeforeTheChainStarts asserts an unresolvable/unreadable chain, a malformed envelope, and the hop ceiling all refuse without the agent ever being asked.
func TestUnit_RelayTriggerRunner_RefusesBeforeTheChainStarts(t *testing.T) {
	dir := triggerTestWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chain-good.json"), []byte(`{"id":"c","tasks":[]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chain-broken.json"), []byte(`{not json`), 0o600))

	for name, req := range map[string]relayChainRequest{
		"an unknown chain":       {Chain: "chain-nowhere.json", Input: json.RawMessage(`{}`)},
		"an unreadable chain":    {Chain: "chain-broken.json", Input: json.RawMessage(`{}`)},
		"input that is no event": {Chain: "chain-good.json", Input: json.RawMessage(`[1,2]`)},
		"input that is absent":   {Chain: "chain-good.json"},
		"a hop that is not an integer": {Chain: "chain-good.json",
			Input: json.RawMessage(`{"hop":"deep"}`)},
		"an envelope past the hop budget": {Chain: "chain-good.json",
			Input: json.RawMessage(`{"hop":` + jsonInt(eventtrigger.DefaultMaxHop+1) + `}`)},
	} {
		t.Run(name, func(t *testing.T) {
			agent := &fakeChainAgent{}
			runner := &relayTriggerRunner{agent: agent, contenoxDir: dir}
			err := runner.RunChain(t.Context(), req)
			require.ErrorIs(t, err, errChainTriggerRefused)
			_, _, calls := agent.last()
			require.Zero(t, calls, "a refused trigger must never start its chain")
		})
	}

	// The ceiling itself still fires: the guard refuses past the budget, not at it.
	agent := &fakeChainAgent{}
	runner := &relayTriggerRunner{agent: agent, contenoxDir: dir}
	require.NoError(t, runner.RunChain(t.Context(), relayChainRequest{
		Chain: "chain-good.json",
		Input: json.RawMessage(`{"hop":` + jsonInt(eventtrigger.DefaultMaxHop) + `}`),
	}))
	ctx, _, calls := agent.last()
	require.Equal(t, 1, calls)
	require.Equal(t, eventtrigger.DefaultMaxHop+1, runtimetypes.EventHopFromContext(ctx))
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestUnit_BuildRelayChainTriggers(t *testing.T) {
	t.Parallel()
	noEngine := buildRelayChainTriggers(nil, t.TempDir(), DefaultWorkspaceID, nil, chatOpts{EffectiveOptInBeta: true})
	require.Nil(t, noEngine.runner)
	require.Contains(t, noEngine.unavailable, "no engine")

	betaOff := buildRelayChainTriggers(nil, t.TempDir(), DefaultWorkspaceID, &Engine{}, chatOpts{})
	require.NotNil(t, betaOff.runner, "the beta gate no longer withholds the runner")
	require.Empty(t, betaOff.unavailable)

	on := buildRelayChainTriggers(nil, t.TempDir(), DefaultWorkspaceID, &Engine{}, chatOpts{EffectiveOptInBeta: true})
	require.NotNil(t, on.runner)
	require.Empty(t, on.unavailable)
}

func TestUnit_RelayTriggerRunner_SessionModes(t *testing.T) {
	reused := func(name string) relayChainRequest {
		return relayChainRequest{
			Chain: "chain-on-event.json", SessionMode: librelay.ChainSessionReused,
			SessionName: name, Input: json.RawMessage(`{"hop":0}`),
		}
	}
	fresh := func(mode string) relayChainRequest {
		return relayChainRequest{Chain: "chain-on-event.json", SessionMode: mode, Input: json.RawMessage(`{"hop":0}`)}
	}

	for name, tc := range map[string]struct {
		firings      []relayChainRequest
		wantCreated  int
		wantSessions int
		wantLists    int
		wantNames    []string
	}{
		"new mode gives every firing its own session": {
			firings:      []relayChainRequest{fresh(librelay.ChainSessionNew), fresh(librelay.ChainSessionNew)},
			wantCreated:  2,
			wantSessions: 2,
			wantLists:    0,
		},
		"an absent mode is new": {
			firings:      []relayChainRequest{fresh(""), fresh("")},
			wantCreated:  2,
			wantSessions: 2,
			wantLists:    0,
		},
		"reused mode creates on the first firing and reuses on the second": {
			firings:      []relayChainRequest{reused("refund-desk"), reused("refund-desk")},
			wantCreated:  1,
			wantSessions: 1,
			wantLists:    2,
			wantNames:    []string{triggerSessionPrefix + "refund-desk"},
		},
		"reused mode with no wire name derives it from the chain file": {
			firings:      []relayChainRequest{reused(""), reused("")},
			wantCreated:  1,
			wantSessions: 1,
			wantLists:    2,
			wantNames:    []string{triggerSessionPrefix + "chain-on-event"},
		},
		"reused sessions with different names never merge": {
			firings:      []relayChainRequest{reused("refund-desk"), reused("ops-desk")},
			wantCreated:  2,
			wantSessions: 2,
			wantLists:    2,
			wantNames:    []string{triggerSessionPrefix + "refund-desk", triggerSessionPrefix + "ops-desk"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := triggerTestWorkspace(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "chain-on-event.json"), []byte(`{"id":"c","tasks":[]}`), 0o600))

			agent := &fakeChainAgent{}
			runner := &relayTriggerRunner{agent: agent, contenoxDir: dir}
			for _, f := range tc.firings {
				require.NoError(t, runner.RunChain(t.Context(), f))
			}

			created, resumed, promptSeen, promptNames, lists := agent.sessionOps()
			require.Len(t, created, tc.wantCreated)
			require.Equal(t, tc.wantLists, lists, `only "reused" reads the session list`)
			require.Empty(t, resumed, "reuse resolves by listing, never by switching the workspace's active session")
			if tc.wantNames != nil {
				require.Equal(t, tc.wantNames, created)
			}

			require.Len(t, promptSeen, len(tc.firings), "every firing reaches the chain with a session")
			require.Len(t, promptNames, len(tc.firings), "every session Prompt received was one the runner created")
			distinct := map[string]struct{}{}
			for _, id := range promptSeen {
				require.NotEmpty(t, id, "PromptRequest.SessionID carries the session the event lands in")
				distinct[id] = struct{}{}
			}
			require.Len(t, distinct, tc.wantSessions)
			for _, n := range created {
				require.True(t, strings.HasPrefix(n, triggerSessionPrefix),
					"a trigger session name is namespaced away from the identity-blind unique index")
				require.True(t, validChainSessionName(n))
			}
		})
	}
}

func TestUnit_RelayTriggerRunner_RefusesWhenTheSessionCannotBeCreated(t *testing.T) {
	dir := triggerTestWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chain-on-event.json"), []byte(`{"id":"c","tasks":[]}`), 0o600))

	agent := &fakeChainAgent{newErr: errors.New("session name already exists")}
	runner := &relayTriggerRunner{agent: agent, contenoxDir: dir}
	err := runner.RunChain(t.Context(), relayChainRequest{
		Chain: "chain-on-event.json", SessionMode: librelay.ChainSessionNew, Input: json.RawMessage(`{"hop":0}`),
	})
	require.ErrorIs(t, err, errChainTriggerRefused)
	require.Contains(t, err.Error(), "session name already exists")
	_, _, calls := agent.last()
	require.Zero(t, calls, "no session means no run")
}

func TestUnit_RelayTriggerRunner_ConcurrentFiringsShareOneReusedSession(t *testing.T) {
	dir := triggerTestWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chain-on-event.json"), []byte(`{"id":"c","tasks":[]}`), 0o600))

	agent := &fakeChainAgent{listDelay: 5 * time.Millisecond}
	runner := &relayTriggerRunner{agent: agent, contenoxDir: dir}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, runner.RunChain(t.Context(), relayChainRequest{
				Chain: "chain-on-event.json", SessionMode: librelay.ChainSessionReused,
				SessionName: "refund-desk", Input: json.RawMessage(`{"hop":0}`),
			}))
		}()
	}
	wg.Wait()

	created, _, promptSeen, _, _ := agent.sessionOps()
	require.Equal(t, []string{triggerSessionPrefix + "refund-desk"}, created)
	require.Len(t, promptSeen, 8)
	for _, id := range promptSeen {
		require.Equal(t, promptSeen[0], id, "every concurrent firing lands in the one reused session")
	}
	runner.mu.Lock()
	require.Empty(t, runner.inSession, "the per-name gate is released once no firing holds it")
	runner.mu.Unlock()
}

func TestUnit_RelayTriggerRunner_WritesSessionsAcpsvcCanList(t *testing.T) {
	ctx := context.Background()
	dir := triggerTestWorkspace(t)
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "trigger.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), DefaultWorkspaceID)
	require.NoError(t, store.CreateNamedMessageIndex(ctx, "idx-local", "local-user", "default"))

	triggers := buildRelayChainTriggers(db, dir, DefaultWorkspaceID, &Engine{}, chatOpts{})
	runner, ok := triggers.runner.(*relayTriggerRunner)
	require.True(t, ok)

	name := triggerSessionPrefix + "default"
	first, err := runner.ensureSession(ctx, librelay.ChainSessionReused, name)
	require.NoError(t, err)
	require.NotEmpty(t, first)

	second, err := runner.ensureSession(ctx, librelay.ChainSessionReused, name)
	require.NoError(t, err)
	require.Equal(t, first, second, "the second firing resolves the same row instead of creating another")

	listed, err := store.ListMessageSessions(ctx, acpsvc.ClientIdentity)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, first, listed[0].ID)
	require.Equal(t, name, listed[0].Name, "an unnamed row can never be attached, so the name is the contract")

	local, err := store.GetMessageSessionByName(ctx, "local-user", "default")
	require.NoError(t, err)
	require.Equal(t, "idx-local", local.ID, "the prefix keeps a wire name clear of another identity's session")
}
