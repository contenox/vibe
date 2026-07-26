package fleetservice

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// This is the acceptance for the constitutional piece: a mission's envelope is the
// unit's TOTAL boundary, bounding COMPUTE as well as actions. It fires a real ACP
// subprocess under an envelope whose compute block sets a tiny maxTurns, and proves
// the whole slice end to end — nothing mocked below the service layer, no LLM, GPU,
// or network anywhere:
//
//   - the downstream is a REAL subprocess (the repo's ACP stub agent) speaking real
//     ACP over stdio, deterministic (its plain scenario just acks and never files a
//     mission report, so it never "reaches" its operator);
//   - dispatch goes through the real fleetservice → agentinstance kernel →
//     agenthost spawn path, with the real registry, mission store, and hitlservice
//     under it, and the compute reader wired exactly as serve would wire it;
//   - the bound is enforced at the drive-loop seam: the intent turn runs, the turn
//     budget (1) forbids the nudge, and the mission is finished STUCK through the
//     real terminal machinery with a reason naming the bound;
//   - the board tells the truth (stuck + reason), liveness is stamped, no third
//     prompt ever runs, and the subprocess is reaped on Stop.
//
// maxToolCalls' per-call teaching refusal is proven at the answerer in
// compute_test.go (the stub raises at most one gated call per mission, so a
// subprocess acceptance for it awaits a fixture that makes repeated gated calls —
// named as a follow-up in the blueprint). This e2e covers the turn seam, which the
// stub CAN drive deterministically.
func TestFleetE2E_ComputeBounds_MaxTurnsLandsMissionStuck(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping compute-bounds e2e: builds and spawns a real ACP agent")
	}

	bin := buildStubAgentBinary(t)
	ctx := context.Background()

	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "compute-e2e.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	store := runtimetypes.New(db.WithoutTransaction())
	agents := agentregistryservice.New(db)
	missions := missionservice.New(db)

	policyDir := t.TempDir()
	hitl := hitlservice.New(hitlservice.NewFSPolicySource(policyDir), runtimetypes.LocalTenantID, store, libtracker.NoopTracker{})

	// The declared unit: the stub with NO gated-action env, so its intent turn runs
	// the plain scenario — it acks and files no mission report, the "never reaches
	// its operator" unit whose turns the compute bound then rations.
	agent := &runtimetypes.Agent{Name: "compute-fixture", Enabled: true}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   bin,
	}))
	require.NoError(t, agents.Create(ctx, agent))

	stderr := &lockedBuffer{}
	instances := agentinstance.New(agents, agentinstance.WithStderr(stderr))
	t.Cleanup(func() { _ = instances.Close() })

	// The compute reader wired the way serve would wire it: the same hitlservice
	// that governs actions also carries the compute ceiling (its concrete type
	// implements ComputeBoundsReader).
	reader, ok := hitl.(hitlservice.ComputeBoundsReader)
	require.True(t, ok)
	svc := New(instances, agents, missions, nil, t.TempDir(), libtracker.NoopTracker{}, WithComputeBounds(reader))

	// The envelope's COMPUTE half: one turn total. The action rules are irrelevant
	// here (the plain scenario gates nothing); the bound rations the TURN budget.
	envelope := writePolicy(t, policyDir, "envelope-maxturns.json", map[string]any{
		"default_action": "approve",
		"rules":          []map[string]any{},
		"compute": map[string]any{
			"maxTurns":    1,
			"onExhausted": "finish_stuck",
		},
	})

	result, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:      "compute-fixture",
		Intent:         "run the mission and report in", // no stub trigger → plain ack
		HITLPolicyName: envelope,
	})
	require.NoError(t, err, "dispatch stderr:\n%s", stderr.String())
	require.NotEmpty(t, result.MissionID)

	// Observe the unit's own stream so we can count the TURNS it actually ran: the
	// plain scenario acks once per turn, so an "ack" per turn.
	viewer := &recordingViewer{id: "compute-observer"}
	_, err = instances.Attach(ctx, result.InstanceID, libacp.SessionID(result.SessionID), viewer)
	require.NoError(t, err)

	// The bound bites: the mission lands STUCK, through the real terminal machinery,
	// with a reason naming the bound.
	require.Eventually(t, func() bool {
		m, gerr := missions.Get(ctx, result.MissionID)
		return gerr == nil && m.Status == missionservice.StatusStuck
	}, 60*time.Second, 100*time.Millisecond,
		"the mission never landed stuck on its turn budget\nstderr:\n%s", stderr.String())

	m, err := missions.Get(ctx, result.MissionID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusStuck, m.Status)
	require.Contains(t, m.StatusReason, "maxTurns=1", "the terminal reason names the bound it crossed")
	require.Contains(t, m.StatusReason, computeBoundLead)
	require.NotNil(t, m.LastHeartbeat, "the completed intent turn still stamps liveness")

	// Exactly ONE turn ran: the intent turn acked once, and the turn budget forbade
	// the nudge, so a second "ack" must never appear.
	require.Equal(t, 1, strings.Count(viewer.messageText(), "ack"),
		"exactly one turn ran: maxTurns=1 stopped the mission before the nudge")
	require.Never(t, func() bool {
		return strings.Count(viewer.messageText(), "ack") > 1
	}, 2*time.Second, 100*time.Millisecond, "a second turn ran — maxTurns must forbid the nudge")

	// The board is truthful and the subprocess is reaped on Stop.
	require.NoError(t, svc.Stop(ctx, result.InstanceID))
	_, err = svc.Get(ctx, result.InstanceID)
	require.ErrorIs(t, err, agentinstance.ErrNotFound, "Stop reaps the subprocess")
}
