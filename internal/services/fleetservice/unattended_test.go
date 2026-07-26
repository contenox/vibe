package fleetservice

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/approvalflow"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// These cover the answerer's DECISIONS with the HITL service and mission store
// faked out, so each branch is provable in isolation: which policy governs a
// request, when an evaluation is trusted, and — the one that matters most —
// that every way of failing to understand a request costs a human's attention
// instead of granting one. The wire behavior these decisions produce is covered
// end to end in e2e_unattended_permission_test.go.

// ─── fakes ────────────────────────────────────────────────────────────────

// fakeHITL records what it was asked to evaluate and what ask it was handed,
// and answers from preset values. It implements only what the answerer calls;
// the rest satisfies the interface.
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

// The attention-ask half of the Service: this double covers the PERMISSION path
// only, so both are inert here.
func (f *fakeHITL) RequestAttention(context.Context, hitlservice.AttentionRequest, taskengine.TaskEventSink) (string, error) {
	return "", nil
}
func (f *fakeHITL) Answer(context.Context, string, string) error        { return nil }
func (f *fakeHITL) AnswerAsAgent(context.Context, string, string) error { return nil }
func (f *fakeHITL) PendingAttentionAsks(context.Context, string) ([]*runtimetypes.HITLApproval, error) {
	return nil, nil
}
func (f *fakeHITL) AttentionBoundsFor(context.Context, string) (hitlservice.AttentionBounds, error) {
	return hitlservice.AttentionBounds{}, nil
}
func (f *fakeHITL) AgentAnswerCount(context.Context, string) (int, error) { return 0, nil }
func (f *fakeHITL) SweepExpired(context.Context) (int, error)             { return 0, nil }
func (f *fakeHITL) ListPending(context.Context, int) ([]*runtimetypes.HITLApproval, error) {
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

// fakeMissions answers GetByInstance from a map; everything else is unused here.
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

// ─── helpers ──────────────────────────────────────────────────────────────

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

// ─── tests ────────────────────────────────────────────────────────────────

// The mission's envelope is what governs the unit — not the process-global
// active policy, and not a rule set invented for the headless case.
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

// A unit with no mission behind it is still unattended, and is governed by the
// configured default rather than by a second rule set.
func TestUnit_Unattended_NoMissionUsesDefaultPolicy(t *testing.T) {
	hitl := &fakeHITL{verdict: hitlservice.EvaluationResult{Action: hitlservice.ActionAllow}}
	missions := &fakeMissions{}

	req := namedRequest(t, "echo", "echo", map[string]any{"text": "hi"})
	_, err := answerer(hitl, missions, "fallback.json")(context.Background(), unattended(req))
	require.NoError(t, err)
	require.Equal(t, "fallback.json", hitl.lastEval(t).policyName)
	require.Zero(t, hitl.askCount())
}

// A mission lookup that FAILS is not a reason to fail open: the default policy
// applies, and the request is still judged.
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

// A denied action is refused without creating an ask: there is nothing for a
// human to decide about an action the envelope forbids outright.
func TestUnit_Unattended_DeniedNeedsNoAsk(t *testing.T) {
	hitl := &fakeHITL{verdict: hitlservice.EvaluationResult{Action: hitlservice.ActionDeny}}
	missions := missionsWith(&missionservice.Mission{ID: "mission-1", InstanceID: "inst-1", HITLPolicyName: "envelope.json"})

	req := namedRequest(t, "local_fs", "write_file", map[string]any{"path": "/etc/passwd"})
	resp, err := answerer(hitl, missions, "")(context.Background(), unattended(req))
	require.NoError(t, err)
	require.Equal(t, "no", resp.Outcome.OptionID)
	require.Zero(t, hitl.askCount())
}

// THE GAP RULE, half one: a request whose contenox tool identity cannot be
// established is never evaluated and never allowed — it is escalated.
func TestUnit_Unattended_UnmappableRequestEscalates(t *testing.T) {
	hitl := &fakeHITL{
		// Deliberately hostile: an evaluator that would ALLOW anything. If the
		// answerer consulted it for an unnamed request, this test would grant.
		verdict:  hitlservice.EvaluationResult{Action: hitlservice.ActionAllow},
		approved: false,
	}
	missions := missionsWith(&missionservice.Mission{ID: "mission-1", InstanceID: "inst-1", HITLPolicyName: "envelope.json"})

	// A foreign agent's request: a title and arguments, no contenox envelope.
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

// THE GAP RULE, half two: an ALLOW verdict reached without arguments is not
// trusted, because condition-bearing deny rules could not have been evaluated.
func TestUnit_Unattended_AllowWithoutArgsEscalates(t *testing.T) {
	hitl := &fakeHITL{verdict: hitlservice.EvaluationResult{Action: hitlservice.ActionAllow}, approved: true}
	missions := missionsWith(&missionservice.Mission{ID: "mission-1", InstanceID: "inst-1", HITLPolicyName: "envelope.json"})

	req := namedRequest(t, "local_fs", "read_file", nil) // no rawInput at all
	resp, err := answerer(hitl, missions, "")(context.Background(), unattended(req))
	require.NoError(t, err)
	require.Equal(t, 1, hitl.askCount(), "an allow that could not see the arguments must still ask")
	require.Equal(t, "yes", resp.Outcome.OptionID, "and the human's approval is honored")
}

// A DENY verdict reached without arguments is honored as-is: the unsafe
// direction is permitting, not refusing.
func TestUnit_Unattended_DenyWithoutArgsStandsWithoutAsking(t *testing.T) {
	hitl := &fakeHITL{verdict: hitlservice.EvaluationResult{Action: hitlservice.ActionDeny}}
	missions := missionsWith(&missionservice.Mission{ID: "mission-1", InstanceID: "inst-1", HITLPolicyName: "envelope.json"})

	req := namedRequest(t, "local_fs", "write_file", nil)
	resp, err := answerer(hitl, missions, "")(context.Background(), unattended(req))
	require.NoError(t, err)
	require.Equal(t, "no", resp.Outcome.OptionID)
	require.Zero(t, hitl.askCount())
}

// An evaluator that ERRORS escalates rather than guessing.
func TestUnit_Unattended_PolicyErrorEscalates(t *testing.T) {
	hitl := &fakeHITL{evalErr: fmt.Errorf("policy source unavailable"), approved: false}
	missions := missionsWith(&missionservice.Mission{ID: "mission-1", InstanceID: "inst-1", HITLPolicyName: "envelope.json"})

	req := namedRequest(t, "local_fs", "write_file", map[string]any{"path": "/x"})
	resp, err := answerer(hitl, missions, "")(context.Background(), unattended(req))
	require.NoError(t, err)
	require.Equal(t, 1, hitl.askCount())
	require.Equal(t, "no", resp.Outcome.OptionID)
}

// The ask carries the full attribution set, so an inbox row can name the unit,
// the session, the agent and the mission — not just the tool.
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

// A half-wired answerer refuses rather than allowing. A wiring defect must not
// become an open gate.
func TestUnit_Unattended_UnwiredDepsRefuse(t *testing.T) {
	fallback := NewUnattendedPermissionAnswerer(UnattendedPermissionDeps{})
	req := namedRequest(t, "local_fs", "write_file", map[string]any{"path": "/x"})
	resp, err := fallback(context.Background(), unattended(req))
	require.NoError(t, err)
	require.Equal(t, "no", resp.Outcome.OptionID)
}

// An ask the requester's context outlived (a cancelled turn) refuses, and the
// refusal is reported as an answer rather than as an error the kernel would
// have to interpret.
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
