package fleetservice

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/approvalflow"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

type fakeHITL struct {
	mu sync.Mutex

	verdict    hitlservice.EvaluationResult
	evalErr    error
	evalCalls  []evalCall
	approved   bool
	requestErr error
	asks       []hitlservice.ApprovalRequest
}

type evalCall struct {
	policyName string
	toolsName  string
	toolName   string
	args       map[string]any
}

func (f *fakeHITL) Evaluate(ctx context.Context, toolsName, toolName string, args map[string]any) (hitlservice.EvaluationResult, error) {
	f.mu.Lock()
	f.evalCalls = append(f.evalCalls, evalCall{
		policyName: hitlservice.PolicyNameFromContext(ctx),
		toolsName:  toolsName,
		toolName:   toolName,
		args:       args,
	})
	f.mu.Unlock()
	if f.evalErr != nil {
		return hitlservice.EvaluationResult{}, f.evalErr
	}
	return f.verdict, nil
}

func (f *fakeHITL) RequestApproval(_ context.Context, req hitlservice.ApprovalRequest, _ taskengine.TaskEventSink) (bool, error) {
	f.mu.Lock()
	f.asks = append(f.asks, req)
	f.mu.Unlock()
	if f.requestErr != nil {
		return false, f.requestErr
	}
	return f.approved, nil
}

func (f *fakeHITL) Respond(context.Context, string, bool) error { return nil }

// The attention-ask half of the Service is inert; this double covers the
// permission path only.
func (f *fakeHITL) RequestAttention(context.Context, hitlservice.AttentionRequest, taskengine.TaskEventSink) (string, error) {
	return "", nil
}
func (f *fakeHITL) Answer(context.Context, string, string) error        { return nil }
func (f *fakeHITL) AnswerAsAgent(context.Context, string, string) error { return nil }
func (f *fakeHITL) AnswerAsAgentNamed(context.Context, string, string, string) error {
	return nil
}
func (f *fakeHITL) AnswerAsAgentBounded(context.Context, string, string, string, int) error {
	return nil
}
func (f *fakeHITL) PendingAttentionAsks(context.Context, string) ([]*runtimetypes.HITLApproval, error) {
	return nil, nil
}
func (f *fakeHITL) AttentionBoundsFor(context.Context, string) (hitlservice.AttentionBounds, error) {
	return hitlservice.AttentionBounds{}, nil
}
func (f *fakeHITL) AgentAnswerCount(context.Context, string) (int, error)   { return 0, nil }
func (f *fakeHITL) AgentApprovalCount(context.Context, string) (int, error) { return 0, nil }
func (f *fakeHITL) RespondAsAgentBounded(context.Context, string, string, bool, string, int) error {
	return nil
}
func (f *fakeHITL) AgentGuidanceFor(context.Context, string) ([]hitlservice.GuidanceNote, error) {
	return nil, nil
}
func (f *fakeHITL) SweepExpired(context.Context) (int, error) { return 0, nil }
func (f *fakeHITL) ListPending(context.Context, int) ([]*runtimetypes.HITLApproval, error) {
	return nil, nil
}
func (f *fakeHITL) ListPendingForSession(context.Context, string, int) ([]*runtimetypes.HITLApproval, error) {
	return nil, nil
}
func (f *fakeHITL) AbandonMissionAsks(context.Context, string) ([]string, error) {
	return nil, nil
}

func (f *fakeHITL) askCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asks)
}

func (f *fakeHITL) lastAsk(t *testing.T) hitlservice.ApprovalRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.asks, "expected a durable ask to have been created")
	return f.asks[len(f.asks)-1]
}

func (f *fakeHITL) lastEval(t *testing.T) evalCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.evalCalls, "expected the envelope to have been evaluated")
	return f.evalCalls[len(f.evalCalls)-1]
}

type fakeMissions struct {
	missionservice.Service
	byInstance map[string]*missionservice.Mission
	err        error
}

func (f *fakeMissions) GetByInstance(_ context.Context, instanceID string) (*missionservice.Mission, error) {
	if f.err != nil {
		return nil, f.err
	}
	m, ok := f.byInstance[instanceID]
	if !ok {
		return nil, libdb.ErrNotFound
	}
	return m, nil
}

func namedRequest(t *testing.T, toolsName, toolName string, args map[string]any) libacp.RequestPermissionRequest {
	t.Helper()
	meta, err := json.Marshal(approvalflow.Meta{ToolsName: toolsName, ToolName: toolName})
	require.NoError(t, err)
	var raw json.RawMessage
	if args != nil {
		raw, err = json.Marshal(args)
		require.NoError(t, err)
	}
	return libacp.RequestPermissionRequest{
		SessionID: "sess-1",
		ToolCall: libacp.PermissionToolCall{
			ToolCallID: "call-1",
			Title:      toolsName + "." + toolName,
			RawInput:   raw,
			Meta:       meta,
		},
		Options: []libacp.PermissionOption{
			{OptionID: "yes", Kind: libacp.PermissionAllowOnce},
			{OptionID: "no", Kind: libacp.PermissionRejectOnce},
		},
		Meta: meta,
	}
}

func unattended(req libacp.RequestPermissionRequest) agentinstance.UnattendedPermission {
	return agentinstance.UnattendedPermission{
		InstanceID: "inst-1",
		AgentID:    "agent-id-1",
		AgentName:  "reviewer",
		SessionID:  req.SessionID,
		Request:    req,
	}
}

func answerer(hitl *fakeHITL, missions missionservice.Service, defaultPolicy string) agentinstance.PermissionFallback {
	return NewUnattendedPermissionAnswerer(UnattendedPermissionDeps{
		HITL:              hitl,
		Missions:          missions,
		Sink:              taskengine.NoopTaskEventSink{},
		DefaultPolicyName: defaultPolicy,
	})
}

func missionsWith(m *missionservice.Mission) *fakeMissions {
	return &fakeMissions{byInstance: map[string]*missionservice.Mission{"inst-1": m}}
}

func TestUnit_Unattended_EvaluatesTheMissionsEnvelope(t *testing.T) {
	hitl := &fakeHITL{verdict: hitlservice.EvaluationResult{Action: hitlservice.ActionAllow}}
	missions := missionsWith(&missionservice.Mission{
		ID: "mission-1", InstanceID: "inst-1", HITLPolicyName: "envelope.json",
	})

	req := namedRequest(t, "local_fs", "read_file", map[string]any{"path": "/x"})
	resp, err := answerer(hitl, missions, "fallback.json")(context.Background(), unattended(req))
	require.NoError(t, err)
	require.Equal(t, "yes", resp.Outcome.OptionID, "an allowed action is answered immediately")
	require.Zero(t, hitl.askCount(), "an action inside the envelope must cost nobody's attention")

	eval := hitl.lastEval(t)
	require.Equal(t, "envelope.json", eval.policyName)
	require.Equal(t, "local_fs", eval.toolsName)
	require.Equal(t, "read_file", eval.toolName)
	require.Equal(t, "/x", eval.args["path"])
}

func TestUnit_Unattended_NoMissionUsesDefaultPolicy(t *testing.T) {
	hitl := &fakeHITL{verdict: hitlservice.EvaluationResult{Action: hitlservice.ActionAllow}}
	missions := &fakeMissions{}

	req := namedRequest(t, "echo", "echo", map[string]any{"text": "hi"})
	_, err := answerer(hitl, missions, "fallback.json")(context.Background(), unattended(req))
	require.NoError(t, err)
	require.Equal(t, "fallback.json", hitl.lastEval(t).policyName)
	require.Zero(t, hitl.askCount())
}

func TestUnit_Unattended_MissionLookupFailureFallsBackSafely(t *testing.T) {
	hitl := &fakeHITL{verdict: hitlservice.EvaluationResult{Action: hitlservice.ActionApprove}}
	missions := &fakeMissions{err: fmt.Errorf("store unavailable")}

	req := namedRequest(t, "local_fs", "write_file", map[string]any{"path": "/x"})
	_, err := answerer(hitl, missions, "fallback.json")(context.Background(), unattended(req))
	require.NoError(t, err)
	require.Equal(t, "fallback.json", hitl.lastEval(t).policyName)
	require.Equal(t, 1, hitl.askCount())
	require.Empty(t, hitl.lastAsk(t).MissionID, "an unresolvable mission is recorded as none, not guessed")
}

func TestUnit_Unattended_DeniedNeedsNoAsk(t *testing.T) {
	hitl := &fakeHITL{verdict: hitlservice.EvaluationResult{Action: hitlservice.ActionDeny}}
	missions := missionsWith(&missionservice.Mission{ID: "mission-1", InstanceID: "inst-1", HITLPolicyName: "envelope.json"})

	req := namedRequest(t, "local_fs", "write_file", map[string]any{"path": "/etc/passwd"})
	resp, err := answerer(hitl, missions, "")(context.Background(), unattended(req))
	require.NoError(t, err)
	require.Equal(t, "no", resp.Outcome.OptionID)
	require.Zero(t, hitl.askCount())
}

func TestUnit_Unattended_UnmappableRequestEscalates(t *testing.T) {
	hitl := &fakeHITL{
		verdict:  hitlservice.EvaluationResult{Action: hitlservice.ActionAllow},
		approved: false,
	}
	missions := missionsWith(&missionservice.Mission{ID: "mission-1", InstanceID: "inst-1", HITLPolicyName: "envelope.json"})

	req := libacp.RequestPermissionRequest{
		SessionID: "sess-1",
		ToolCall: libacp.PermissionToolCall{
			ToolCallID: "call-1",
			Title:      "Edit configuration",
			RawInput:   json.RawMessage(`{"path":"/x"}`),
		},
		Options: []libacp.PermissionOption{
			{OptionID: "yes", Kind: libacp.PermissionAllowOnce},
			{OptionID: "no", Kind: libacp.PermissionRejectOnce},
		},
	}
	resp, err := answerer(hitl, missions, "")(context.Background(), unattended(req))
	require.NoError(t, err)
	require.Equal(t, "no", resp.Outcome.OptionID, "an unanswered escalation refuses")

	require.Empty(t, hitl.evalCalls, "an unmappable request must not be evaluated at all")
	require.Equal(t, 1, hitl.askCount(), "it must reach a human instead")

	ask := hitl.lastAsk(t)
	require.Equal(t, "envelope.json", ask.PolicyName, "the ask still names the envelope in force")
	require.Equal(t, "Edit configuration", ask.ToolName, "the row describes what was asked for")
	require.Equal(t, "mission-1", ask.MissionID)
}

func TestUnit_Unattended_AllowWithoutArgsEscalates(t *testing.T) {
	hitl := &fakeHITL{verdict: hitlservice.EvaluationResult{Action: hitlservice.ActionAllow}, approved: true}
	missions := missionsWith(&missionservice.Mission{ID: "mission-1", InstanceID: "inst-1", HITLPolicyName: "envelope.json"})

	req := namedRequest(t, "local_fs", "read_file", nil)
	resp, err := answerer(hitl, missions, "")(context.Background(), unattended(req))
	require.NoError(t, err)
	require.Equal(t, 1, hitl.askCount(), "an allow that could not see the arguments must still ask")
	require.Equal(t, "yes", resp.Outcome.OptionID, "and the human's approval is honored")
}

func TestUnit_Unattended_DenyWithoutArgsStandsWithoutAsking(t *testing.T) {
	hitl := &fakeHITL{verdict: hitlservice.EvaluationResult{Action: hitlservice.ActionDeny}}
	missions := missionsWith(&missionservice.Mission{ID: "mission-1", InstanceID: "inst-1", HITLPolicyName: "envelope.json"})

	req := namedRequest(t, "local_fs", "write_file", nil)
	resp, err := answerer(hitl, missions, "")(context.Background(), unattended(req))
	require.NoError(t, err)
	require.Equal(t, "no", resp.Outcome.OptionID)
	require.Zero(t, hitl.askCount())
}

func TestUnit_Unattended_PolicyErrorEscalates(t *testing.T) {
	hitl := &fakeHITL{evalErr: fmt.Errorf("policy source unavailable"), approved: false}
	missions := missionsWith(&missionservice.Mission{ID: "mission-1", InstanceID: "inst-1", HITLPolicyName: "envelope.json"})

	req := namedRequest(t, "local_fs", "write_file", map[string]any{"path": "/x"})
	resp, err := answerer(hitl, missions, "")(context.Background(), unattended(req))
	require.NoError(t, err)
	require.Equal(t, 1, hitl.askCount())
	require.Equal(t, "no", resp.Outcome.OptionID)
}

func TestUnit_Unattended_AskCarriesAttribution(t *testing.T) {
	hitl := &fakeHITL{verdict: hitlservice.EvaluationResult{Action: hitlservice.ActionApprove}, approved: true}
	missions := missionsWith(&missionservice.Mission{ID: "mission-1", InstanceID: "inst-1", HITLPolicyName: "envelope.json"})

	req := namedRequest(t, "local_fs", "write_file", map[string]any{"path": "/workspace/x"})
	_, err := answerer(hitl, missions, "")(context.Background(), unattended(req))
	require.NoError(t, err)

	ask := hitl.lastAsk(t)
	require.Equal(t, "inst-1", ask.InstanceID)
	require.Equal(t, "sess-1", ask.SessionID)
	require.Equal(t, "reviewer", ask.AgentName)
	require.Equal(t, "mission-1", ask.MissionID)
	require.Equal(t, "local_fs", ask.ToolsName)
	require.Equal(t, "write_file", ask.ToolName)
	require.Equal(t, "call-1", ask.ToolCallID)
	require.Equal(t, "/workspace/x", ask.Args["path"])
}

func TestUnit_Unattended_UnwiredDepsRefuse(t *testing.T) {
	fallback := NewUnattendedPermissionAnswerer(UnattendedPermissionDeps{})
	req := namedRequest(t, "local_fs", "write_file", map[string]any{"path": "/x"})
	resp, err := fallback(context.Background(), unattended(req))
	require.NoError(t, err)
	require.Equal(t, "no", resp.Outcome.OptionID)
}

func TestUnit_Unattended_RequestApprovalFailureRefuses(t *testing.T) {
	hitl := &fakeHITL{
		verdict:    hitlservice.EvaluationResult{Action: hitlservice.ActionApprove},
		requestErr: context.Canceled,
	}
	missions := missionsWith(&missionservice.Mission{ID: "mission-1", InstanceID: "inst-1", HITLPolicyName: "envelope.json"})

	req := namedRequest(t, "local_fs", "write_file", map[string]any{"path": "/x"})
	resp, err := answerer(hitl, missions, "")(context.Background(), unattended(req))
	require.NoError(t, err)
	require.Equal(t, "no", resp.Outcome.OptionID)
}
