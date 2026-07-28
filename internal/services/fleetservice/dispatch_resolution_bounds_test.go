package fleetservice

import (
	"errors"
	"testing"

	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/stretchr/testify/require"
)

// TestFleetService_Dispatch_CarriesEnvelopeAllowlistsIntoTheUnitSession: the
// envelope's model/backend allowlist must reach the unit's session/new `_meta`.
func TestFleetService_Dispatch_CarriesEnvelopeAllowlistsIntoTheUnitSession(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	missions := missionservice.New(db)

	man := &fakeManager{startID: "inst-1", openID: "sess-1"}
	svc := New(man, agents, missions, nil, t.TempDir(), nil,
		WithComputeBounds(fakeBoundsReader{bounds: hitlservice.ComputeBounds{
			MaxToolCalls:     10,
			ModelAllowlist:   []string{"gemini-2.5-flash"},
			BackendAllowlist: []string{"my-ollama"},
		}}))

	_, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:      "runner",
		Intent:         "do the thing",
		HITLPolicyName: "hitl-policy-bounded.json",
	})
	require.NoError(t, err)

	require.Len(t, man.openSpecs, 1)
	meta, ok := missionservice.ParseMissionMetaFull(man.openSpecs[0].Meta)
	require.True(t, ok, "the unit's session must carry its mission meta")
	require.NotEmpty(t, meta.MissionID)
	require.Equal(t, []string{"gemini-2.5-flash"}, meta.ModelAllowlist,
		"an envelope's modelAllowlist must reach the unit that resolves the model")
	require.Equal(t, []string{"my-ollama"}, meta.BackendAllowlist)
}

// TestFleetService_Dispatch_UnboundedEnvelopeSendsNoAllowlist: an envelope
// with no allowlist produces exactly the `_meta` it produced before bounds
// existed.
func TestFleetService_Dispatch_UnboundedEnvelopeSendsNoAllowlist(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	missions := missionservice.New(db)

	man := &fakeManager{startID: "inst-1", openID: "sess-1"}
	svc := New(man, agents, missions, nil, t.TempDir(), nil,
		WithComputeBounds(fakeBoundsReader{bounds: hitlservice.ComputeBounds{MaxToolCalls: 10}}))

	_, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:      "runner",
		Intent:         "do the thing",
		HITLPolicyName: "hitl-policy-default.json",
	})
	require.NoError(t, err)

	require.Len(t, man.openSpecs, 1)
	meta, ok := missionservice.ParseMissionMetaFull(man.openSpecs[0].Meta)
	require.True(t, ok)
	require.Nil(t, meta.ModelAllowlist)
	require.Nil(t, meta.BackendAllowlist)
}

// TestFleetService_Dispatch_BoundsReadFailureStillDispatches: fail-to-unbounded
// on a bounds-read failure; the unit still comes up.
func TestFleetService_Dispatch_BoundsReadFailureStillDispatches(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	missions := missionservice.New(db)

	man := &fakeManager{startID: "inst-1", openID: "sess-1"}
	svc := New(man, agents, missions, nil, t.TempDir(), nil,
		WithComputeBounds(fakeBoundsReader{err: errors.New("policy read exploded")}))

	res, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:      "runner",
		Intent:         "do the thing",
		HITLPolicyName: "hitl-policy-default.json",
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.MissionID)

	require.Len(t, man.openSpecs, 1)
	meta, ok := missionservice.ParseMissionMetaFull(man.openSpecs[0].Meta)
	require.True(t, ok)
	require.Nil(t, meta.ModelAllowlist)
}

// TestFleetService_Dispatch_NoBoundsReaderIsUnbounded: a fleet built without
// a bounds reader behaves exactly as before compute bounds existed.
func TestFleetService_Dispatch_NoBoundsReaderIsUnbounded(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	missions := missionservice.New(db)

	man := &fakeManager{startID: "inst-1", openID: "sess-1"}
	svc := New(man, agents, missions, nil, t.TempDir(), nil)

	_, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:      "runner",
		Intent:         "do the thing",
		HITLPolicyName: "hitl-policy-default.json",
	})
	require.NoError(t, err)

	require.Len(t, man.openSpecs, 1)
	meta, ok := missionservice.ParseMissionMetaFull(man.openSpecs[0].Meta)
	require.True(t, ok)
	require.Nil(t, meta.ModelAllowlist)
	require.Nil(t, meta.BackendAllowlist)
}
