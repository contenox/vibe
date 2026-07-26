package fleetservice

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	"github.com/contenox/beam/internal/kernel/taskengine"
	libbus "github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/chainagents"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/operatorinbox"
	"github.com/contenox/beam/internal/services/reportrouter"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// This is the COMPOSED acceptance the validation scheme owed: the overnight-batch
// scenario in miniature. Every isolated e2e in this package proves ONE seam of
// mission mode against a real subprocess; none proves that the four ways a
// dispatched unit can END coexist on ONE board, over ONE isolated HOME, without
// interfering — which is the whole promise of a fleet an operator fires and walks
// away from. This test dispatches four deterministic units, each destined for a
// different terminal shape, and asserts the FULL board and inbox truth an operator
// would wake up to:
//
//   - reporter  → files a `result` and finishes LANDED (mission_report + mission_finish).
//   - mute      → never touches a mission tool: the confused unit. It is nudged
//                 exactly once, then the runtime files a BLOCKER on its behalf and
//                 the mission stays OPEN (the auto-blocker cure, e2e_unattended_nudge).
//   - gated     → asks permission for a gated tool under a strict envelope: the
//                 durable ask LANDS, BLOCKS the unit, and answering it through the
//                 real HITL service RELEASES the unit, which then CONTINUES past the
//                 gate (e2e_unattended_permission's stub/policy approach).
//   - planner   → revises its living plan (two mission_plan calls): revision > 0
//                 with a carried-forward entry set (e2e_mission_revision_loop).
//
// Nothing is mocked below the service layer and no LLM/GPU/network is involved.
// The three chain/acp units are REAL `contenox` subprocesses sharing one
// HOME-isolated $HOME/.contenox/local.db; the gated unit is the hermetic
// acp-stub-agent. Both binaries are freshly built.
//
// # Why the bus is SQLite-backed (not in-mem)
//
// The reporter files its own `result` from INSIDE its subprocess, through its
// publisher-wired mission service (acp_cmd.go). For that report's ReportAddedEvent
// to reach THIS process's report router, the bus must be durable and cross-process
// — the exact mechanism the mission-roundtrip e2e leans on. The mute/gated blockers
// are filed in-process by the runtime and would ride an in-mem bus too, but one bus
// serves all producers, so SQLite it is.
//
// # Why the bring-ups are SERIALIZED
//
// Two full contenox runtimes SEEDING one SQLite file concurrently at startup is the
// contention the report-routing and mission-roundtrip e2es both flagged as flaky.
// fleetservice.Dispatch blocks until the unit's session is open — i.e. until its
// subprocess has finished booting — so calling the four Dispatches in sequence, and
// settling each unit to a stable fact before dispatching the next, keeps the heavy
// boots from overlapping. The units still COEXIST on the board (none is stopped
// until the end); the final board assertion is what proves the four ran without
// cross-unit interference, which no single-unit e2e can show.

// obReporterChain files a result then finishes the mission landed — both mission
// tools in one deterministic turn, no model. The unit "reaches its operator" on
// turn 1 (a report exists AND the status leaves open), so it is never nudged.
const obReporterChain = `{
  "id": "e2e-overnight-reporter",
  "tasks": [
    {
      "id": "report",
      "handler": "tools",
      "tools": {
        "name": "mission",
        "tool_name": "mission_report",
        "args": {"kind": "result", "summary": "overnight batch reporter landed"}
      },
      "transition": {"branches": [{"operator": "default", "goto": "finish"}]}
    },
    {
      "id": "finish",
      "handler": "tools",
      "tools": {
        "name": "mission",
        "tool_name": "mission_finish",
        "args": {"status": "landed", "reason": "batch reporter complete"}
      },
      "transition": {"branches": [{"operator": "default", "goto": "done"}]}
    },
    {
      "id": "done",
      "handler": "noop",
      "transition": {"branches": [{"operator": "default", "goto": "end"}]}
    }
  ]
}`

// obPlannerChain sets an initial plan then revises it in the SAME turn: two
// mission_plan calls, so the durable revision reaches 2 and a `bench` entry the
// first snapshot lacked is carried onto the plan. A plan revision counts as
// "reaching the operator" (Plan.Revision > 0), so the unit is never nudged.
const obPlannerChain = `{
  "id": "e2e-overnight-planner",
  "tasks": [
    {
      "id": "plan",
      "handler": "tools",
      "tools": {
        "name": "mission",
        "tool_name": "mission_plan",
        "args": {
          "entries": "[{\"id\":\"survey\",\"content\":\"survey the codebase\",\"status\":\"in_progress\",\"priority\":\"high\"},{\"id\":\"port\",\"content\":\"port the hot loop\",\"status\":\"pending\",\"priority\":\"medium\"}]",
          "explanation": "initial plan"
        }
      },
      "transition": {"branches": [{"operator": "default", "goto": "revise"}]}
    },
    {
      "id": "revise",
      "handler": "tools",
      "tools": {
        "name": "mission",
        "tool_name": "mission_plan",
        "args": {
          "entries": "[{\"id\":\"survey\",\"content\":\"survey the codebase\",\"status\":\"completed\",\"priority\":\"high\"},{\"id\":\"port\",\"content\":\"port the hot loop\",\"status\":\"in_progress\",\"priority\":\"medium\"},{\"id\":\"bench\",\"content\":\"benchmark against the baseline\",\"status\":\"pending\",\"priority\":\"low\"}]",
          "explanation": "revised after the worker report"
        }
      },
      "transition": {"branches": [{"operator": "default", "goto": "done"}]}
    },
    {
      "id": "done",
      "handler": "noop",
      "transition": {"branches": [{"operator": "default", "goto": "end"}]}
    }
  ]
}`

func TestFleetE2E_OvernightBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping overnight-batch e2e: builds two binaries and boots several real contenox runtimes")
	}

	contenoxBin := buildContenoxBin(t)
	stubBin := buildStubAgentBinary(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	// Neutralize ambient CONTENOX_* so a developer's shell can neither redirect a
	// unit's boot nor its chain; each agent sets its own chain path via its Env.
	for _, k := range []string{
		"CONTENOX_DEFAULT_MODEL", "CONTENOX_DEFAULT_PROVIDER",
		"CONTENOX_DEFAULT_ALT_MODEL", "CONTENOX_DEFAULT_ALT_PROVIDER",
		"CONTENOX_DEFAULT_MAX_TOKENS", "CONTENOX_DEFAULT_THINK",
		"CONTENOX_ACP_CHAIN_PATH",
	} {
		t.Setenv(k, "")
	}
	runContenoxCLI(t, contenoxBin, home, "config", "set", "default-model", "overnight-batch-fake-model")
	runContenoxCLI(t, contenoxBin, home, "config", "set", "update-check", "false")

	contenoxDir := filepath.Join(home, ".contenox")
	require.DirExists(t, contenoxDir)

	ctx := context.Background()
	// The ONE shared store: every chain/acp unit resolves $HOME/.contenox/local.db,
	// and so does this handle. A unit's report row is visible here through it, and
	// the SQLite bus rides the same file cross-process.
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(contenoxDir, "local.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	agents := agentregistryservice.New(db)
	store := runtimetypes.New(db.WithoutTransaction())

	// SQLite bus: the reporter unit publishes its own report from a SEPARATE
	// process, so the routing event must survive the process boundary.
	bus := libbus.NewSQLite(db.WithoutTransaction())
	t.Cleanup(func() { _ = bus.Close() })
	missions := missionservice.New(db, missionservice.WithEventPublisher(bus))
	inbox := operatorinbox.New(db)

	// The gated unit's envelope machinery: a real HITL service over an FS policy
	// dir, exactly as serve wires the unattended answerer.
	policyDir := t.TempDir()
	hitl := hitlservice.New(hitlservice.NewFSPolicySource(policyDir), runtimetypes.LocalTenantID, store, libtracker.NoopTracker{})
	gatedEnvelope := writePolicy(t, policyDir, "overnight-gated-envelope.json", map[string]any{
		"default_action": "approve",
		"rules": []map[string]any{
			{"tools": probeToolsName, "tool": probeToolName, "action": "approve"},
		},
	})

	// The kernel, wired for ALL three unit kinds this board holds at once:
	//   - WithSelfExecutable: the mute CHAIN unit re-execs this contenox binary.
	//   - WithPermissionFallback: the gated unit's viewer-less permission ask is
	//     answered against its mission's envelope (the unattended answerer).
	//   - WithStderr: a subprocess failure can be quoted.
	stderr := &lockedBuffer{}
	kernel := agentinstance.New(agents,
		agentinstance.WithSelfExecutable(contenoxBin),
		agentinstance.WithStderr(stderr),
		agentinstance.WithPermissionFallback(NewUnattendedPermissionAnswerer(UnattendedPermissionDeps{
			HITL:     hitl,
			Missions: missions,
			Sink:     taskengine.NoopTaskEventSink{},
			Tracker:  libtracker.NoopTracker{},
		})),
	)
	t.Cleanup(func() { _ = kernel.Close() })

	// The report router, wired exactly as serve wires it: the kernel is the
	// SessionDeliverer, the inbox the fallback. Every mission here is operator-fired
	// (no ParentSessionID), so every routed report lands in the operator inbox.
	router, err := reportrouter.New(reportrouter.Deps{
		Bus:      bus,
		Sessions: kernel,
		Inbox:    inbox,
		Tracker:  libtracker.NoopTracker{},
	})
	require.NoError(t, err)
	stopRouter, err := router.Start(ctx)
	require.NoError(t, err)
	t.Cleanup(stopRouter)

	svc := New(kernel, agents, missions, nil, home, libtracker.NoopTracker{})

	// ── Declare the four agents ─────────────────────────────────────────────────

	// mute: the print-only CHAIN unit, discovered by convention.
	writeChainAgentFixture(t, contenoxDir)
	res, err := chainagents.Discover(ctx, agents, contenoxDir)
	require.NoError(t, err)
	require.Contains(t, res.Created, "agent-fleet-fixture")

	// reporter + planner: `contenox acp --auto` units bound to deterministic
	// mission chains, sharing this HOME/DB.
	reporterChainPath := filepath.Join(contenoxDir, "overnight-reporter-chain.json")
	require.NoError(t, os.WriteFile(reporterChainPath, []byte(obReporterChain), 0o644))
	obCreateAcpAgent(t, ctx, agents, "overnight-reporter", contenoxBin, reporterChainPath)

	plannerChainPath := filepath.Join(contenoxDir, "overnight-planner-chain.json")
	require.NoError(t, os.WriteFile(plannerChainPath, []byte(obPlannerChain), 0o644))
	obCreateAcpAgent(t, ctx, agents, "overnight-planner", contenoxBin, plannerChainPath)

	// gated: the hermetic acp-stub-agent, told which named tool call to ask about
	// and where to write its out-of-band outcome file (the unattended answerer maps
	// the ask onto the mission's envelope).
	// Inside `home` — the workspace this batch's units are dispatched into (see
	// the New below), and therefore the only root the agent sandbox lets a unit
	// write to. An outcome file anywhere else is denied by Landlock and the unit
	// goes silent, which reads here as "the gated unit never continued past its
	// gate" long after the gate itself worked.
	gatedReportPath := filepath.Join(home, "overnight-gated-outcome.txt")
	gatedAgent := &runtimetypes.Agent{Name: "overnight-gated", Enabled: true}
	require.NoError(t, gatedAgent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   stubBin,
		Env: map[string]string{
			"ACP_STUB_GATED_TOOLS_NAME":   probeToolsName,
			"ACP_STUB_GATED_TOOL_NAME":    probeToolName,
			"ACP_STUB_GATED_ARGS_JSON":    `{"path":"/tmp/overnight-gated.txt"}`,
			"ACP_STUB_GATED_REPORT_PATH":  gatedReportPath,
			"ACP_STUB_ADVERTISE_COMMANDS": "",
		},
	}))
	require.NoError(t, agents.Create(ctx, gatedAgent))

	// ── Serialized bring-ups: settle each unit before dispatching the next ───────

	// (1) reporter → result report + LANDED status.
	reporter, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "overnight-reporter", Intent: "report and land", HITLPolicyName: "default",
	})
	require.NoError(t, err, "reporter dispatch stderr:\n%s", stderr.String())
	obWaitReport(t, ctx, missions, reporter.MissionID, stderr, 60*time.Second, func(r *missionservice.Report) bool {
		return r.Kind == missionservice.ReportKindResult && strings.Contains(r.Summary, "reporter landed")
	})
	obWaitMission(t, ctx, missions, reporter.MissionID, stderr, 30*time.Second, func(m *missionservice.Mission) bool {
		return m.Status == missionservice.StatusLanded
	})

	// (2) mute → nudged once, then the runtime's silent-turn BLOCKER.
	mute, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "agent-fleet-fixture", Intent: "do the batch mission", HITLPolicyName: "default",
	})
	require.NoError(t, err, "mute dispatch stderr:\n%s", stderr.String())
	obWaitReport(t, ctx, missions, mute.MissionID, stderr, 120*time.Second, func(r *missionservice.Report) bool {
		return r.Kind == missionservice.ReportKindBlocker && strings.Contains(r.Summary, silentTurnBlockerLead)
	})

	// (3) gated → durable ask lands + BLOCKS; answering it RELEASES the unit.
	gated, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "overnight-gated", Intent: "run the gated_action scenario", HITLPolicyName: gatedEnvelope,
	})
	require.NoError(t, err, "gated dispatch stderr:\n%s", stderr.String())

	var ask *runtimetypes.HITLApproval
	require.Eventually(t, func() bool {
		rows, lerr := hitl.ListPending(ctx, 100)
		if lerr != nil {
			return false
		}
		for _, r := range rows {
			if r.MissionID != nil && *r.MissionID == gated.MissionID {
				ask = r
				return true
			}
		}
		return false
	}, 60*time.Second, 100*time.Millisecond,
		"the gated unit's viewer-less permission ask never reached the durable store\nstderr:\n%s", stderr.String())
	require.Equal(t, probeToolsName, ask.ToolsName)
	require.Equal(t, probeToolName, ask.ToolName)
	require.Equal(t, gatedEnvelope, ask.PolicyName, "the ask names the mission's envelope")
	require.Equal(t, gated.InstanceID, ask.InstanceID)

	// Still blocked: the unit has written no outcome yet.
	require.Empty(t, obReadFile(gatedReportPath), "the gated unit must still be parked on its permission ask")

	// Answer it through the SAME service the REST inbox / `contenox approvals answer` call.
	require.NoError(t, hitl.Respond(ctx, ask.ID, true))

	// The unit CONTINUES past the gate: its out-of-band outcome file appears.
	require.Eventually(t, func() bool {
		return obReadFile(gatedReportPath) == "gated-action outcome=selected option=allow-once"
	}, 60*time.Second, 100*time.Millisecond,
		"the answered gated unit never continued past its gate\nstderr:\n%s", stderr.String())

	answered, err := store.GetHITLApproval(ctx, ask.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, answered.State, "the answered ask is durably approved")
	require.Empty(t, obPendingForMission(t, ctx, hitl, gated.MissionID), "no ask stays pending for the gated mission")

	// The stub uses no mission tool, so after continuing it is (correctly) treated
	// as mute by the doctrine: the runtime files it a silent-turn blocker too, which
	// — the mission being operator-fired — reaches the inbox. Waiting for it here
	// makes the final inbox deterministic.
	obWaitReport(t, ctx, missions, gated.MissionID, stderr, 60*time.Second, func(r *missionservice.Report) bool {
		return r.Kind == missionservice.ReportKindBlocker && strings.Contains(r.Summary, silentTurnBlockerLead)
	})

	// (4) planner → the living plan revised past its initial snapshot.
	planner, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "overnight-planner", Intent: "plan and revise", HITLPolicyName: "default",
	})
	require.NoError(t, err, "planner dispatch stderr:\n%s", stderr.String())
	obWaitMission(t, ctx, missions, planner.MissionID, stderr, 60*time.Second, func(m *missionservice.Mission) bool {
		return m.Plan.Revision > 0 && planHasEntry(m.Plan, "bench")
	})

	// ── The board truth an operator wakes up to ─────────────────────────────────

	ids := []string{reporter.MissionID, mute.MissionID, gated.MissionID, planner.MissionID}
	require.Len(t, obUnique(ids), 4, "the four missions must be distinct — no board collision")

	// reporter: LANDED, one result, heartbeat stamped.
	mReporter, err := missions.Get(ctx, reporter.MissionID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusLanded, mReporter.Status)
	require.NotNil(t, mReporter.LastHeartbeat, "a completed turn stamps mission liveness")
	require.Equal(t, 0, mReporter.Plan.Revision, "the reporter never touched a plan")
	rReporter, err := missions.ListReports(ctx, reporter.MissionID, 10)
	require.NoError(t, err)
	require.Len(t, rReporter, 1)
	require.Equal(t, missionservice.ReportKindResult, rReporter[0].Kind)

	// mute: OPEN, one runtime blocker, heartbeat stamped, plan untouched.
	mMute, err := missions.Get(ctx, mute.MissionID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusOpen, mMute.Status, "a nudged-then-blocked mission stays open, not terminal")
	require.NotNil(t, mMute.LastHeartbeat)
	require.Equal(t, 0, mMute.Plan.Revision)
	rMute, err := missions.ListReports(ctx, mute.MissionID, 10)
	require.NoError(t, err)
	require.Len(t, rMute, 1)
	require.Equal(t, missionservice.ReportKindBlocker, rMute[0].Kind)
	require.Contains(t, rMute[0].Summary, silentTurnBlockerLead)

	// gated: CONTINUED — its ask is durably approved, its outcome file was written,
	// heartbeat stamped.
	mGated, err := missions.Get(ctx, gated.MissionID)
	require.NoError(t, err)
	require.NotNil(t, mGated.LastHeartbeat)
	require.Equal(t, "gated-action outcome=selected option=allow-once", obReadFile(gatedReportPath))
	answeredAgain, err := store.GetHITLApproval(ctx, ask.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, answeredAgain.State)
	require.NotNil(t, answeredAgain.MissionID)
	require.Equal(t, gated.MissionID, *answeredAgain.MissionID, "attribution survives the answer")

	// planner: revised plan, carried-forward ids, heartbeat stamped, no report.
	mPlanner, err := missions.Get(ctx, planner.MissionID)
	require.NoError(t, err)
	require.NotNil(t, mPlanner.LastHeartbeat)
	require.Greater(t, mPlanner.Plan.Revision, 0, "the planner revised its living plan")
	require.True(t, planHasEntry(mPlanner.Plan, "bench"), "the revision carried a new benchmark entry")
	survey := planEntryByID(mPlanner.Plan, "survey")
	require.NotNil(t, survey, "the carried-forward survey entry kept its id")
	rPlanner, err := missions.ListReports(ctx, planner.MissionID, 10)
	require.NoError(t, err)
	require.Empty(t, rPlanner, "a plan revision files no report")

	// No cross-unit interference: no mission carries another's terminal verdict, and
	// only the two blocker-filing units left a blocker.
	require.NotEqual(t, missionservice.StatusLanded, mMute.Status)
	require.NotEqual(t, missionservice.StatusLanded, mPlanner.Status)

	// ── The operator inbox ──────────────────────────────────────────────────────

	// The reporter's own filed report routes cross-process into the inbox (the
	// mission-roundtrip seam). Wait for it so the inbox is fully settled.
	require.Eventually(t, func() bool {
		return obInboxHas(t, ctx, inbox, reporter.MissionID, missionservice.ReportKindResult)
	}, 60*time.Second, 150*time.Millisecond,
		"the reporter's own result report never routed to the operator inbox\nstderr:\n%s", stderr.String())

	items, err := inbox.List(ctx, 100)
	require.NoError(t, err)
	byMission := map[string][]*operatorinbox.Item{}
	for _, it := range items {
		require.Equal(t, operatorinbox.ReasonOperatorFired, it.Reason,
			"every batch mission was operator-fired, so every routed report is operator_fired")
		byMission[it.MissionID] = append(byMission[it.MissionID], it)
	}
	// Exactly the three report-filing units reached the inbox; the planner (which
	// filed no report) never did.
	require.Contains(t, byMission, reporter.MissionID)
	require.Contains(t, byMission, mute.MissionID)
	require.Contains(t, byMission, gated.MissionID)
	require.NotContains(t, byMission, planner.MissionID, "a plan revision is not a report and does not reach the inbox")
	require.Equal(t, missionservice.ReportKindResult, byMission[reporter.MissionID][0].Report.Kind)
	require.Equal(t, missionservice.ReportKindBlocker, byMission[mute.MissionID][0].Report.Kind)
	require.Equal(t, missionservice.ReportKindBlocker, byMission[gated.MissionID][0].Report.Kind)

	// ── Teardown: stop every unit and prove no subprocess leaks ──────────────────
	for _, id := range []string{reporter.InstanceID, mute.InstanceID, gated.InstanceID, planner.InstanceID} {
		require.NoError(t, svc.Stop(ctx, id))
		_, gerr := svc.Get(ctx, id)
		require.ErrorIs(t, gerr, agentinstance.ErrNotFound, "unit %s must be gone after Stop", id)
	}
}

// obCreateAcpAgent declares a `contenox acp --auto` unit bound to a deterministic
// mission chain, sharing the caller's HOME/DB. --auto disables HITL so the chain's
// mission-tool calls run unattended.
func obCreateAcpAgent(t *testing.T, ctx context.Context, agents agentregistryservice.Service, name, bin, chainPath string) {
	t.Helper()
	agent := &runtimetypes.Agent{Name: name, Enabled: true}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   bin,
		Args:      []string{"acp", "--auto"},
		Env:       map[string]string{"CONTENOX_ACP_CHAIN_PATH": chainPath},
	}))
	require.NoError(t, agents.Create(ctx, agent))
}

// obWaitReport blocks until missionID carries a report satisfying pred, quoting the
// spawned units' stderr on timeout.
func obWaitReport(t *testing.T, ctx context.Context, missions missionservice.Service, missionID string, stderr *lockedBuffer, timeout time.Duration, pred func(*missionservice.Report) bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		reports, err := missions.ListReports(ctx, missionID, 20)
		if err != nil {
			return false
		}
		for _, r := range reports {
			if pred(r) {
				return true
			}
		}
		return false
	}, timeout, 150*time.Millisecond,
		"mission %s never carried the expected report\nstderr:\n%s", missionID, stderr.String())
}

// obWaitMission blocks until missionID satisfies pred.
func obWaitMission(t *testing.T, ctx context.Context, missions missionservice.Service, missionID string, stderr *lockedBuffer, timeout time.Duration, pred func(*missionservice.Mission) bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		m, err := missions.Get(ctx, missionID)
		if err != nil {
			return false
		}
		return pred(m)
	}, timeout, 150*time.Millisecond,
		"mission %s never reached the expected state\nstderr:\n%s", missionID, stderr.String())
}

// obInboxHas reports whether the inbox holds an item for missionID of the given kind.
func obInboxHas(t *testing.T, ctx context.Context, inbox operatorinbox.Service, missionID string, kind missionservice.ReportKind) bool {
	t.Helper()
	items, err := inbox.List(ctx, 100)
	if err != nil {
		return false
	}
	for _, it := range items {
		if it.MissionID == missionID && it.Report.Kind == kind {
			return true
		}
	}
	return false
}

// obPendingForMission returns the asks still pending for missionID.
func obPendingForMission(t *testing.T, ctx context.Context, hitl hitlservice.Service, missionID string) []*runtimetypes.HITLApproval {
	t.Helper()
	rows, err := hitl.ListPending(ctx, 100)
	require.NoError(t, err)
	out := make([]*runtimetypes.HITLApproval, 0)
	for _, r := range rows {
		if r.MissionID != nil && *r.MissionID == missionID {
			out = append(out, r)
		}
	}
	return out
}

// obReadFile returns the trimmed contents of path, or "" when it does not exist yet.
func obReadFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// obUnique returns the distinct values in ids.
func obUnique(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
