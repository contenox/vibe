package fleetservice

// Tests for the fleet-width admission gate (admission.go) and the process
// counters (counters.go). Counters are shared package atomics, so assertions
// read deltas, never absolute values.

import (
	"context"
	"testing"

	"github.com/contenox/contenox/internal/errdefs"
	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// TestFleetService_Dispatch_RefusedAtWidthCap: at the cap, Dispatch refuses
// before bringing anything up, with a teaching message.
func TestFleetService_Dispatch_RefusedAtWidthCap(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)

	man := &fakeManager{startID: "inst-cap", openID: "sess-cap"}
	man.setListStates(agentinstance.StateRunning, agentinstance.StateRunning)
	svc := New(man, agents, nil, nil, "/project/root", nil, WithMaxParallel(2))

	before := Counters()
	_, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "one too many", HITLPolicyName: "default",
	})
	require.Error(t, err)
	require.ErrorIs(t, err, errdefs.ErrConflict, "a cap refusal is a 409: fleet state, not request shape")

	require.Contains(t, err.Error(), admissionRefusalLead)
	require.Contains(t, err.Error(), "2 units are already open")
	require.Contains(t, err.Error(), "fleet-max-parallel=2", "the refusal names the knob and its value")
	require.Contains(t, err.Error(), "Wait for a unit to conclude", "the refusal names the first remedy")
	require.Contains(t, err.Error(), "contenox config set fleet-max-parallel", "the refusal names the second remedy")
	require.Contains(t, err.Error(), "0 = unlimited", "the opt-out is documented where the operator hits the wall")

	require.Empty(t, man.starts(), "a refused dispatch must never bring an instance up")

	after := Counters()
	require.Equal(t, before.CapRefusals+1, after.CapRefusals, "a cap refusal is counted")
	require.Equal(t, before.Dispatches, after.Dispatches, "a refused dispatch is not a dispatch")
}

// TestFleetService_Dispatch_DefaultCapEnforcedWithoutWiring: a fleetservice
// built with no cap option still enforces DefaultMaxParallel.
func TestFleetService_Dispatch_DefaultCapEnforcedWithoutWiring(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)

	states := make([]string, DefaultMaxParallel)
	for i := range states {
		states[i] = agentinstance.StateRunning
	}
	man := &fakeManager{startID: "inst-def", openID: "sess-def"}
	man.setListStates(states...)
	svc := New(man, agents, nil, nil, "/project/root", nil) // no WithMaxParallel

	_, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "past the default", HITLPolicyName: "default",
	})
	require.Error(t, err)
	require.ErrorIs(t, err, errdefs.ErrConflict)
	require.Contains(t, err.Error(), admissionRefusalLead)
	require.Contains(t, err.Error(), "fleet-max-parallel=8", "the default ships enforced at its documented value")
	require.Empty(t, man.starts())
}

// TestFleetService_Dispatch_CapZeroIsUnlimited: fleet-max-parallel=0 admits
// regardless of fleet width.
func TestFleetService_Dispatch_CapZeroIsUnlimited(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	missions := missionservice.New(db)

	states := make([]string, 50)
	for i := range states {
		states[i] = agentinstance.StateRunning
	}
	man := &fakeManager{startID: "inst-unl", openID: "sess-unl"}
	man.setListStates(states...)
	svc := New(man, agents, missions, nil, "/project/root", libtracker.NoopTracker{}, WithMaxParallel(0))

	res, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "wide open", HITLPolicyName: "default",
	})
	require.NoError(t, err, "cap 0 is unlimited: 50 open units refuse nothing")
	require.Equal(t, []string{"runner"}, man.starts())
	waitMissionSettled(t, missions, res.MissionID)
}

// TestFleetService_Dispatch_CapClearsAsUnitsConclude: a service that refused
// at the cap admits again once a unit concludes.
func TestFleetService_Dispatch_CapClearsAsUnitsConclude(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	missions := missionservice.New(db)

	man := &fakeManager{startID: "inst-seq", openID: "sess-seq"}
	man.setListStates(agentinstance.StateRunning)
	svc := New(man, agents, missions, nil, "/project/root", libtracker.NoopTracker{}, WithMaxParallel(1))

	_, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "refused at the wall", HITLPolicyName: "default",
	})
	require.Error(t, err)
	require.ErrorIs(t, err, errdefs.ErrConflict)

	// The open unit concludes; the same gate now admits.
	man.setListStates()
	res, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "admitted after the wall cleared", HITLPolicyName: "default",
	})
	require.NoError(t, err, "the count is live: a concluded unit frees its slot")
	require.Equal(t, []string{"runner"}, man.starts(), "exactly the second dispatch allocated")
	waitMissionSettled(t, missions, res.MissionID)
}

// TestFleetService_Dispatch_CapCountsOnlyLiveStates: starting/running hold a
// slot; error/warning wreckage does not.
func TestFleetService_Dispatch_CapCountsOnlyLiveStates(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	missions := missionservice.New(db)

	// A board full of wreckage: crashed and restart-exhausted instances.
	man := &fakeManager{startID: "inst-wreck", openID: "sess-wreck"}
	man.setListStates(agentinstance.StateError, agentinstance.StateWarning)
	svc := New(man, agents, missions, nil, "/project/root", libtracker.NoopTracker{}, WithMaxParallel(1))

	res, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "wreckage does not hold a slot", HITLPolicyName: "default",
	})
	require.NoError(t, err, "error/warning instances are wreckage, not open units")
	waitMissionSettled(t, missions, res.MissionID)

	// A unit still mid-spawn holds a slot: spend is committed at starting.
	man2 := &fakeManager{startID: "inst-starting", openID: "sess-starting"}
	man2.setListStates(agentinstance.StateStarting)
	svc2 := New(man2, agents, missions, nil, "/project/root", libtracker.NoopTracker{}, WithMaxParallel(1))

	_, err = svc2.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "starting counts", HITLPolicyName: "default",
	})
	require.Error(t, err)
	require.ErrorIs(t, err, errdefs.ErrConflict)
	require.Empty(t, man2.starts())
}

// TestFleetService_Dispatch_CapFailsClosedOnUnreadableBoard: an unreadable
// board refuses (fail-closed), surfacing as itself, not a cap refusal.
func TestFleetService_Dispatch_CapFailsClosedOnUnreadableBoard(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)

	man := &fakeManager{startID: "inst-err", openID: "sess-err", listErr: context.DeadlineExceeded}
	svc := New(man, agents, nil, nil, "/project/root", nil, WithMaxParallel(4))

	before := Counters()
	_, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "unreadable board", HITLPolicyName: "default",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "count open units", "the error names the count, not a phantom refusal")
	require.NotContains(t, err.Error(), admissionRefusalLead)
	require.Empty(t, man.starts(), "fail-closed: nothing is brought up past an unreadable board")
	require.Equal(t, before.CapRefusals, Counters().CapRefusals, "a board failure is not a cap refusal")
}

// TestFleetService_Counters: dispatches, cap refusals, and verification
// downgrades each tally independently.
func TestFleetService_Counters(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	missions := missionservice.New(db)

	man := &fakeManager{startID: "inst-cnt", openID: "sess-cnt"}
	svc := New(man, agents, missions, nil, "/project/root", libtracker.NoopTracker{}, WithMaxParallel(1))

	before := Counters()

	res, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "count me", HITLPolicyName: "default",
	})
	require.NoError(t, err)
	waitMissionSettled(t, missions, res.MissionID)
	require.Equal(t, before.Dispatches+1, Counters().Dispatches, "an admitted dispatch is counted")

	man.setListStates(agentinstance.StateRunning)
	_, err = svc.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "refuse me", HITLPolicyName: "default",
	})
	require.Error(t, err)
	require.Equal(t, before.CapRefusals+1, Counters().CapRefusals, "a cap refusal is counted")
	require.Equal(t, before.Dispatches+1, Counters().Dispatches, "a refusal never counts as a dispatch")

	RecordVerificationDowngrade()
	require.Equal(t, before.VerificationDowngrades+1, Counters().VerificationDowngrades,
		"the exported downgrade hook counts with no service handle at all")
}
