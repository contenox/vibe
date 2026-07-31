package fleetservice

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// TestFleetE2E_ComputeBounds_MaxTurnsLandsMissionStuck fires a real ACP
// subprocess under an envelope with maxTurns=1: the turn budget forbids the
// nudge and the mission finishes stuck with a reason naming the bound.
// maxToolCalls' per-call refusal is proven separately in compute_test.go.
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

	// The stub with no gated-action env runs the plain scenario: it acks and
	// files no mission report.
	agent := &runtimetypes.Agent{Name: "compute-fixture", Enabled: true}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   bin,
	}))
	require.NoError(t, agents.Create(ctx, agent))

	stderr := &lockedBuffer{}
	instances := agentinstance.New(agents, agentinstance.WithStderr(stderr))
	t.Cleanup(func() { _ = instances.Close() })

	// The same hitlservice that governs actions also carries the compute
	// ceiling.
	reader, ok := hitl.(hitlservice.ComputeBoundsReader)
	require.True(t, ok)
	svc := New(instances, agents, missions, nil, t.TempDir(), libtracker.NoopTracker{}, WithComputeBounds(reader))

	// The envelope's compute half: one turn total; action rules are
	// irrelevant here.
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

	// The plain scenario acks once per turn, so counting "ack" counts turns.
	viewer := &recordingViewer{id: "compute-observer"}
	_, err = instances.Attach(ctx, result.InstanceID, libacp.SessionID(result.SessionID), viewer)
	require.NoError(t, err)

	// The bound bites: the mission lands stuck, with a reason naming it.
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

	// Exactly one turn ran: the turn budget forbade the nudge.
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
