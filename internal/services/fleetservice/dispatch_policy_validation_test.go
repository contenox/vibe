package fleetservice

import (
	"context"
	"errors"
	"testing"

	"github.com/contenox/contenox/errdefs"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/stretchr/testify/require"
)

type fakePolicyValidator struct {
	err       error
	validated []string
}

func (v *fakePolicyValidator) ValidatePolicy(_ context.Context, name string) error {
	v.validated = append(v.validated, name)
	return v.err
}

// TestFleetService_Dispatch_NonexistentPolicyRefused: a policy that does not
// load is refused as a 4xx before any instance is brought up.
func TestFleetService_Dispatch_NonexistentPolicyRefused(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)

	man := &fakeManager{startID: "inst-1", openID: "sess-1"}
	val := &fakePolicyValidator{err: errors.New("read hitl policy \"no-such-policy.json\": not found")}
	svc := New(man, agents, nil, nil, "/project/root", nil, WithPolicyValidator(val))

	_, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:      "runner",
		Intent:         "do the thing",
		HITLPolicyName: "no-such-policy.json",
	})
	require.Error(t, err)
	require.ErrorIs(t, err, errdefs.ErrInvalidParameter,
		"a nonexistent envelope is a 4xx invalid-parameter, not a silent default-gate substitution")
	require.Contains(t, err.Error(), "hitlPolicyName", "the error names the offending parameter")
	require.Contains(t, err.Error(), "no-such-policy.json")
	require.Equal(t, []string{"no-such-policy.json"}, val.validated, "the named envelope must be validated")
	require.Empty(t, man.starts(), "a refused dispatch must never bring an instance up")
}

// TestFleetService_Dispatch_ValidPolicyPassesGate: a policy the validator
// accepts flows past the gate to StartResolved.
func TestFleetService_Dispatch_ValidPolicyPassesGate(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)

	man := &fakeManager{startErr: errors.New("stop after the gate")}
	val := &fakePolicyValidator{err: nil}
	svc := New(man, agents, nil, nil, "/project/root", nil, WithPolicyValidator(val))

	_, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:      "runner",
		Intent:         "do the thing",
		HITLPolicyName: "hitl-policy-strict.json",
	})
	require.ErrorContains(t, err, "stop after the gate",
		"a validated envelope must flow past the gate to the instance-bring-up step")
	require.Equal(t, []string{"hitl-policy-strict.json"}, val.validated)
	require.Equal(t, []string{"runner"}, man.starts(), "a validated envelope lets the dispatch reach StartResolved")
}
