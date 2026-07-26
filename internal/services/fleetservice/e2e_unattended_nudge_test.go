package fleetservice

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	libbus "github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/chainagents"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/operatorinbox"
	"github.com/contenox/beam/internal/services/reportrouter"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// This is the acceptance for the CURE — the misbehaving fixture is the point.
// It reuses the print-only chain unit (writeChainAgentFixture: one noop task
// whose print is its whole reply, and which NEVER calls a mission tool). That is
// exactly a "confused unit that talks into the void": under the mission doctrine
// its turn ends bare, reaching no operator. This test dispatches it through the
// same real path serve wires — fleetservice → agentinstance kernel → agenthost
// spawn, with the real registry, mission store, bus, report router, and operator
// inbox — and proves the whole cure end to end:
//
//  1. liveness is stamped after the first turn (a mission whose "never reported"
//     status once meant nothing now carries a heartbeat);
//  2. a SECOND, nudged turn runs (the confused unit gets taught, once);
//  3. after the second bare turn a BLOCKER report exists on the mission and,
//     because the mission was operator-fired, REACHES the operator inbox;
//  4. NO third prompt is ever sent (the nudge loop is hard-capped at one);
//  5. the mission is NOT terminal — it is blocked, not done.
//
// Nothing is mocked and no LLM/GPU/network is involved: the reply is the fixture
// chain's deterministic print, so counting its occurrences in the session
// transcript counts the TURNS the unit actually ran.
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
	// The print-only fixture: a unit that answers in prose and NEVER touches a
	// mission tool — the confused unit this whole cure exists for.
	writeChainAgentFixture(t, contenoxDir)

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "unattended-nudge-e2e.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	agents := agentregistryservice.New(db)
	// The bus is the producer half; missionservice publishes on AddReport, so the
	// runtime-filed blocker rides the same report machinery a unit's own report
	// would.
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

	// The report router, wired exactly as serve wires it: the Manager is the
	// SessionDeliverer, the operator inbox is the fallback for a report that
	// reaches no live supervisor.
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

	// Fired by an operator (no ParentSessionID): a runtime-filed blocker on this
	// mission must therefore route to the operator INBOX.
	dispatched, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:      "agent-fleet-fixture",
		Intent:         "do the mission and report in",
		HITLPolicyName: "default",
	})
	require.NoError(t, err, "dispatch stderr:\n%s", stderr.String())
	require.NotEmpty(t, dispatched.MissionID)

	// Observe the unit's own stream: the fixture prints its reply once PER turn, so
	// the count of the reply in the transcript is the count of turns that ran.
	viewer := &recordingViewer{id: "nudge-observer"}
	_, err = instances.Attach(ctx, dispatched.InstanceID, libacp.SessionID(dispatched.SessionID), viewer)
	require.NoError(t, err)

	// (1) + (2): the first turn stamps liveness, and a SECOND (nudged) turn runs —
	// the fixture prints its reply twice.
	require.Eventually(t, func() bool {
		return strings.Count(viewer.messageText(), chainFixtureReply) >= 2
	}, 120*time.Second, 100*time.Millisecond,
		"the mute unit was never nudged into a second turn; transcript=%q\nstderr:\n%s",
		viewer.messageText(), stderr.String())

	m, err := missions.Get(ctx, dispatched.MissionID)
	require.NoError(t, err)
	require.NotNil(t, m.LastHeartbeat, "turn completion is liveness: the mission must carry a heartbeat")

	// (3): a BLOCKER lands on the mission and reaches the operator inbox.
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

	// (5): the mission is blocked, not done — never moved to a terminal state.
	m, err = missions.Get(ctx, dispatched.MissionID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusOpen, m.Status, "a nudged-then-blocked mission stays open, not terminal")

	// (4): NO third prompt. Two turns produced exactly two prints; the nudge loop
	// is hard-capped at one, so once the blocker exists the transcript must hold
	// the reply EXACTLY twice and never a third time.
	require.Never(t, func() bool {
		return strings.Count(viewer.messageText(), chainFixtureReply) > 2
	}, 2*time.Second, 100*time.Millisecond,
		"a third prompt ran — the nudge loop must be hard-capped at one")
	require.Equal(t, 2, strings.Count(viewer.messageText(), chainFixtureReply),
		"exactly two turns ran: the intent turn and one nudge")

	require.NoError(t, svc.Stop(ctx, dispatched.InstanceID))
}
