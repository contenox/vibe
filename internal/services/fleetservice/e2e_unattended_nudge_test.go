package fleetservice

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	libbus "github.com/contenox/contenox/internal/libbus"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/chainagents"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/operatorinbox"
	"github.com/contenox/contenox/internal/services/reportrouter"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// TestFleetE2E_UnattendedNudge_MuteUnitHeartbeatsNudgedAndBlocked: a
// print-only unit that never calls a mission tool gets liveness stamped,
// one nudged turn, then a blocker report in the operator inbox, no third
// prompt, and stays open (not terminal).
func TestFleetE2E_UnattendedNudge_MuteUnitHeartbeatsNudgedAndBlocked(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping unattended-nudge e2e: builds and boots the full contenox binary")
	}

	bin := buildContenoxBinary(t)
	home := t.TempDir()

	t.Setenv("HOME", home)
	for _, k := range []string{
		"CONTENOX_DEFAULT_MODEL", "CONTENOX_DEFAULT_PROVIDER",
		"CONTENOX_DEFAULT_ALT_MODEL", "CONTENOX_DEFAULT_ALT_PROVIDER",
		"CONTENOX_DEFAULT_MAX_TOKENS", "CONTENOX_DEFAULT_THINK",
		"CONTENOX_ACP_CHAIN_PATH",
	} {
		t.Setenv(k, "")
	}
	runContenoxCLI(t, bin, home, "config", "set", "default-model", "chain-unit-fixture-model")
	runContenoxCLI(t, bin, home, "config", "set", "update-check", "false")

	contenoxDir := filepath.Join(home, ".contenox")
	require.DirExists(t, contenoxDir)
	// The print-only fixture never touches a mission tool.
	writeChainAgentFixture(t, contenoxDir)

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "unattended-nudge-e2e.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	agents := agentregistryservice.New(db)
	// missionservice publishes on AddReport, so the runtime-filed blocker
	// rides the same report machinery a unit's own report would.
	bus := libbus.NewInMem()
	t.Cleanup(func() { _ = bus.Close() })
	missions := missionservice.New(db, missionservice.WithEventPublisher(bus))
	inbox := operatorinbox.New(db)

	res, err := chainagents.Discover(ctx, agents, contenoxDir)
	require.NoError(t, err)
	require.Equal(t, []string{"agent-fleet-fixture"}, res.Created)

	stderr := &lockedBuffer{}
	instances := agentinstance.New(agents,
		agentinstance.WithSelfExecutable(bin),
		agentinstance.WithStderr(stderr),
	)
	t.Cleanup(func() { _ = instances.Close() })

	// Wired as serve wires it: the Manager is the SessionDeliverer, the
	// operator inbox the fallback.
	router, err := reportrouter.New(reportrouter.Deps{
		Bus:      bus,
		Sessions: instances,
		Inbox:    inbox,
		Tracker:  libtracker.NoopTracker{},
	})
	require.NoError(t, err)
	stopRouter, err := router.Start(ctx)
	require.NoError(t, err)
	t.Cleanup(stopRouter)

	workDir := t.TempDir()
	svc := New(instances, agents, missions, nil, workDir, libtracker.NoopTracker{})

	// Fired by an operator (no ParentSessionID): the blocker must route to
	// the operator inbox.
	dispatched, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:      "agent-fleet-fixture",
		Intent:         "do the mission and report in",
		HITLPolicyName: "default",
	})
	require.NoError(t, err, "dispatch stderr:\n%s", stderr.String())
	require.NotEmpty(t, dispatched.MissionID)

	// The fixture prints its reply once per turn, so counting occurrences
	// counts turns.
	viewer := &recordingViewer{id: "nudge-observer"}
	_, err = instances.Attach(ctx, dispatched.InstanceID, libacp.SessionID(dispatched.SessionID), viewer)
	require.NoError(t, err)

	// (1)+(2): the first turn stamps liveness, and a second nudged turn runs.
	require.Eventually(t, func() bool {
		return strings.Count(viewer.messageText(), chainFixtureReply) >= 2
	}, 120*time.Second, 100*time.Millisecond,
		"the mute unit was never nudged into a second turn; transcript=%q\nstderr:\n%s",
		viewer.messageText(), stderr.String())

	m, err := missions.Get(ctx, dispatched.MissionID)
	require.NoError(t, err)
	require.NotNil(t, m.LastHeartbeat, "turn completion is liveness: the mission must carry a heartbeat")

	// (3): a blocker lands on the mission and reaches the operator inbox.
	require.Eventually(t, func() bool {
		reps, lerr := missions.ListReports(ctx, dispatched.MissionID, 5)
		return lerr == nil && len(reps) == 1 && reps[0].Kind == missionservice.ReportKindBlocker
	}, 30*time.Second, 100*time.Millisecond,
		"the runtime never filed its blocker for the mute unit\nstderr:\n%s", stderr.String())

	require.Eventually(t, func() bool {
		items, lerr := inbox.List(ctx, 100)
		if lerr != nil {
			return false
		}
		for _, it := range items {
			if it.MissionID == dispatched.MissionID {
				require.Equal(t, operatorinbox.ReasonOperatorFired, it.Reason,
					"an operator-fired mission's blocker routes to the inbox as operator-fired")
				require.Equal(t, missionservice.ReportKindBlocker, it.Report.Kind)
				return true
			}
		}
		return false
	}, 30*time.Second, 100*time.Millisecond,
		"the runtime blocker never reached the operator inbox")

	// (5): the mission is blocked, not done.
	m, err = missions.Get(ctx, dispatched.MissionID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusOpen, m.Status, "a nudged-then-blocked mission stays open, not terminal")

	// (4): no third prompt; the transcript must hold the reply exactly twice.
	require.Never(t, func() bool {
		return strings.Count(viewer.messageText(), chainFixtureReply) > 2
	}, 2*time.Second, 100*time.Millisecond,
		"a third prompt ran — the nudge loop must be hard-capped at one")
	require.Equal(t, 2, strings.Count(viewer.messageText(), chainFixtureReply),
		"exactly two turns ran: the intent turn and one nudge")

	require.NoError(t, svc.Stop(ctx, dispatched.InstanceID))
}
