package fleetservice

// Tests for the fleet-width admission gate (admission.go) and the process
// counters (counters.go). They drive Dispatch through the fakeManager, whose
// List is the board the gate counts over, against the real sqlite-backed
// registries — the same shape as the rest of this package's policy tests.
//
// The counters are process-lifetime package atomics shared across every test in
// the binary, so every counter assertion here reads DELTAS around the action
// under test, never absolute values.

import (
	"context"
	"testing"

	"github.com/contenox/beam/internal/errdefs"
	"github.com/contenox/beam/internal/kernel/agentinstance"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/stretchr/testify/require"
)

// TestFleetService_Dispatch_RefusedAtWidthCap is the gate's core: with the cap
// reached, Dispatch refuses BEFORE bringing anything up, and the refusal
// teaches — it names the cap's config key, its value, and both remedies.
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

	// The teaching message: the greppable lead, the count it saw, the cap's
	// config key and value, and both remedies.
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

// TestFleetService_Dispatch_DefaultCapEnforcedWithoutWiring is the pando lesson
// made a regression test: a fleetservice constructed with NO cap option still
// enforces DefaultMaxParallel. The knob cannot exist un-enforced.
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

// TestFleetService_Dispatch_CapZeroIsUnlimited pins the documented opt-out:
// fleet-max-parallel=0 admits regardless of how wide the fleet already is.
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

// TestFleetService_Dispatch_CapClearsAsUnitsConclude is the decrement half of
// the gate: the same service that refused at the cap admits again once a unit
// concludes, because the count is read live off the kernel's board rather than
// from any ledger of its own.
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

	// The open unit concludes (the kernel drops it from its board); the SAME
	// dispatch now passes the SAME gate.
	man.setListStates()
	res, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "admitted after the wall cleared", HITLPolicyName: "default",
	})
	require.NoError(t, err, "the count is live: a concluded unit frees its slot")
	require.Equal(t, []string{"runner"}, man.starts(), "exactly the second dispatch allocated")
	waitMissionSettled(t, missions, res.MissionID)
}

// TestFleetService_Dispatch_CapCountsOnlyLiveStates pins the liveness
// predicate: starting and running hold a slot; error/warning wreckage does not
// (dead subprocesses must never permanently wall off new dispatches).
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

	// A unit still mid-spawn DOES hold a slot: spend is committed at starting.
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

// TestFleetService_Dispatch_CapFailsClosedOnUnreadableBoard: a gate that cannot
// count must refuse, not wave through — and the failure surfaces as itself, not
// as a cap refusal.
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

// TestFleetService_Counters pins the ledger's three tallies end to end:
// a successful dispatch counts, a cap refusal counts, and the exported
// verification-downgrade hook counts — each independently, all nil-safe
// package-level calls needing no service handle.
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
