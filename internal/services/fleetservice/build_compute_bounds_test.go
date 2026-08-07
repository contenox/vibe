package fleetservice

// The composition seam: an envelope's compute half is inert unless
// BuildInProcess wires a reader over the host's PolicySource. These pin the
// wiring itself; the enforcement seams it feeds are pinned in compute_test.go
// and dispatch_resolution_bounds_test.go.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// boundsFixture builds an in-process fleet over policyDir and returns it with
// the mission store it shares. A nil-returning policyDir ("") means no
// PolicySource at all.
func boundsFixture(t *testing.T, policyDir string) (context.Context, *service, missionservice.Service) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "fleet-bounds.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	missions := missionservice.New(db)
	deps := InProcessDeps{DB: db, Bus: libbus.NewInMem(), Missions: missions}
	if policyDir != "" {
		deps.PolicySource = hitlservice.NewFSPolicySource(policyDir)
	}
	fleet, _, stop, err := BuildInProcess(ctx, deps)
	require.NoError(t, err)
	t.Cleanup(stop)

	svc, ok := fleet.(*service)
	require.True(t, ok)
	return ctx, svc, missions
}

// boundedEnvelope is the compute half every case here reads back.
func boundedEnvelope(t *testing.T, dir, name string) string {
	t.Helper()
	return writePolicy(t, dir, name, map[string]any{
		"default_action": "approve",
		"rules":          []map[string]any{},
		"compute": map[string]any{
			"maxTurns":         1,
			"maxToolCalls":     7,
			"maxTokens":        1234,
			"modelAllowlist":   []string{"gemini-2.5-flash"},
			"backendAllowlist": []string{"my-ollama"},
			"onExhausted":      "finish_stuck",
		},
	})
}

// TestUnit_BuildInProcess_WiresComputeBoundsFromPolicySource: the same
// PolicySource that backs the creation-time validator must also back the
// bounds reader, or every compute bound is silently zero.
func TestUnit_BuildInProcess_WiresComputeBoundsFromPolicySource(t *testing.T) {
	policyDir := t.TempDir()
	envelope := boundedEnvelope(t, policyDir, "envelope-bounded.json")
	ctx, svc, missions := boundsFixture(t, policyDir)

	require.NotNil(t, svc.computeBounds, "a host with a PolicySource must have a bounds reader")

	m := &missionservice.Mission{Intent: "run", AgentName: "unit", HITLPolicyName: envelope}
	require.NoError(t, missions.Create(ctx, m))

	for _, tc := range []struct {
		name  string
		bound hitlservice.ComputeBounds
	}{
		{"drive loop", svc.computeBoundsFor(ctx, m.ID)},
		{"dispatch", svc.dispatchResolutionBounds(ctx, envelope)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, 1, tc.bound.MaxTurns)
			require.Equal(t, 7, tc.bound.MaxToolCalls)
			require.Equal(t, 1234, tc.bound.MaxTokens)
			require.Equal(t, []string{"gemini-2.5-flash"}, tc.bound.ModelAllowlist)
			require.Equal(t, []string{"my-ollama"}, tc.bound.BackendAllowlist)
		})
	}
}

// TestUnit_BuildInProcess_NoPolicySourceStaysUnbounded: the pre-existing
// fail-to-unbounded shape for a host that names no policy source.
func TestUnit_BuildInProcess_NoPolicySourceStaysUnbounded(t *testing.T) {
	ctx, svc, missions := boundsFixture(t, "")

	require.Nil(t, svc.computeBounds)

	m := &missionservice.Mission{Intent: "run", AgentName: "unit", HITLPolicyName: "envelope-bounded.json"}
	require.NoError(t, missions.Create(ctx, m))

	require.Equal(t, hitlservice.ComputeBounds{}, svc.computeBoundsFor(ctx, m.ID))
	require.Equal(t, hitlservice.ComputeBounds{}, svc.dispatchResolutionBounds(ctx, "envelope-bounded.json"))
}

// TestUnit_BuildInProcess_UnknownEnvelopeStaysUnbounded: a bounds read that
// cannot load its policy is unbounded, never a phantom ceiling. Dispatch
// itself refuses such a name (the validator); this pins the reader's own
// register.
func TestUnit_BuildInProcess_UnknownEnvelopeStaysUnbounded(t *testing.T) {
	policyDir := t.TempDir()
	boundedEnvelope(t, policyDir, "envelope-bounded.json")
	ctx, svc, _ := boundsFixture(t, policyDir)

	require.Equal(t, hitlservice.ComputeBounds{}, svc.dispatchResolutionBounds(ctx, "nothing-here.json"))
}

// TestUnit_BuildInProcess_WiredBoundsLandMissionStuckOnMaxTokens drives a
// mission through the reader BuildInProcess wired, over a real envelope on
// disk: the token ceiling must bite rather than run unbounded. The kernel is
// faked because the bound is read between turns, not inside one.
func TestUnit_BuildInProcess_WiredBoundsLandMissionStuckOnMaxTokens(t *testing.T) {
	policyDir := t.TempDir()
	envelope := writePolicy(t, policyDir, "envelope-tokens.json", map[string]any{
		"default_action": "approve",
		"rules":          []map[string]any{},
		"compute": map[string]any{
			"maxTokens":   100,
			"onExhausted": "finish_stuck",
		},
	})
	ctx, built, missions := boundsFixture(t, policyDir)
	require.NotNil(t, built.computeBounds)

	m := &missionservice.Mission{ID: "mission-wired", Intent: "run the mission", AgentName: "unit", HITLPolicyName: envelope}
	require.NoError(t, missions.Create(ctx, m))
	_, err := missions.Bind(ctx, m.ID, "sess-1", "inst-1")
	require.NoError(t, err)

	mgr := &journalManager{fakeManager: &fakeManager{openID: "sess-1"}, journal: []libacp.SessionNotification{usageNote(500)}}
	driven := New(mgr, nil, missions, nil, t.TempDir(), libtracker.NoopTracker{},
		WithComputeBounds(built.computeBounds)).(*service)

	driven.driveUnattendedMission(ctx, missionRun{
		instanceID: "inst-1", sessionID: "sess-1", missionID: m.ID, agentName: "unit", intent: m.Intent,
	})

	require.Equal(t, 1, mgr.promptCalls, "the token budget is spent on turn 1; no nudge follows")
	got, err := missions.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusStuck, got.Status)
	require.Contains(t, got.StatusReason, "maxTokens=100", "the terminal reason names the bound it crossed")
	require.Contains(t, got.StatusReason, computeBoundLead)
}

// TestUnit_BuildInProcess_WiredBoundsCarryAllowlistsIntoTheUnitSession: the
// envelope's allowlists must reach the unit's session/new `_meta`, where the
// unit's own resolution seam binds them.
func TestUnit_BuildInProcess_WiredBoundsCarryAllowlistsIntoTheUnitSession(t *testing.T) {
	policyDir := t.TempDir()
	envelope := boundedEnvelope(t, policyDir, "envelope-bounded.json")
	ctx, built, missions := boundsFixture(t, policyDir)

	agentCtx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, agentCtx, agents, "runner", true)

	mgr := &fakeManager{startID: "inst-1", openID: "sess-1"}
	dispatcher := New(mgr, agents, missions, nil, t.TempDir(), libtracker.NoopTracker{},
		WithComputeBounds(built.computeBounds))

	_, err := dispatcher.Dispatch(ctx, DispatchRequest{
		AgentName:      "runner",
		Intent:         "do the thing",
		HITLPolicyName: envelope,
	})
	require.NoError(t, err)

	require.Len(t, mgr.openSpecs, 1)
	meta, ok := missionservice.ParseMissionMetaFull(mgr.openSpecs[0].Meta)
	require.True(t, ok, "the unit's session must carry its mission meta")
	require.Equal(t, []string{"gemini-2.5-flash"}, meta.ModelAllowlist)
	require.Equal(t, []string{"my-ollama"}, meta.BackendAllowlist)
}
