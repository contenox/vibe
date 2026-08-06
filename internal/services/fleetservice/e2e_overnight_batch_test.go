package fleetservice

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	libbus "github.com/contenox/contenox/internal/libbus"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/chainagents"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/operatorinbox"
	"github.com/contenox/contenox/internal/services/reportrouter"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// TestFleetE2E_OvernightBatch dispatches four deterministic units onto one
// board over one isolated HOME: reporter (files a result, lands), mute
// (nudged once, then blocked), gated (asks permission, blocks, then
// continues once answered), and planner (revises its plan twice). Bring-ups
// are serialized to avoid contention seeding one SQLite file concurrently.

// obReporterChain files a result then finishes landed in one deterministic
// turn; it is never nudged.
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

// obPlannerChain sets an initial plan then revises it in the same turn; a
// plan revision counts as reaching the operator, so it is never nudged.
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
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(contenoxDir, "local.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	agents := agentregistryservice.New(db)
	store := runtimetypes.New(db.WithoutTransaction())

	bus := libbus.NewSQLite(db.WithoutTransaction())
	t.Cleanup(func() { _ = bus.Close() })
	missions := missionservice.New(db, missionservice.WithEventPublisher(bus))
	inbox := operatorinbox.New(db)

	policyDir := t.TempDir()
	hitl := hitlservice.New(hitlservice.NewFSPolicySource(policyDir), runtimetypes.LocalTenantID, store, libtracker.NoopTracker{})
	gatedEnvelope := writePolicy(t, policyDir, "overnight-gated-envelope.json", map[string]any{
		"default_action": "approve",
		"rules": []map[string]any{
			{"tools": probeToolsName, "tool": probeToolName, "action": "approve"},
		},
	})

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

	writeChainAgentFixture(t, contenoxDir)
	res, err := chainagents.Discover(ctx, agents, contenoxDir)
	require.NoError(t, err)
	require.Contains(t, res.Created, "agent-fleet-fixture")

	reporterChainPath := filepath.Join(contenoxDir, "overnight-reporter-chain.json")
	require.NoError(t, os.WriteFile(reporterChainPath, []byte(obReporterChain), 0o644))
	obCreateAcpAgent(t, ctx, agents, "overnight-reporter", contenoxBin, reporterChainPath)

	plannerChainPath := filepath.Join(contenoxDir, "overnight-planner-chain.json")
	require.NoError(t, os.WriteFile(plannerChainPath, []byte(obPlannerChain), 0o644))
	obCreateAcpAgent(t, ctx, agents, "overnight-planner", contenoxBin, plannerChainPath)

	// gated: the hermetic acp-stub-agent, told which tool call to ask about
	// and where to write its outcome file — inside `home`, the only root the
	// agent sandbox lets a unit write to (Landlock denies elsewhere).
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

	// (1) reporter: result report + landed status.
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

	// (2) mute: nudged once, then the runtime's silent-turn blocker.
	mute, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "agent-fleet-fixture", Intent: "do the batch mission", HITLPolicyName: "default",
	})
	require.NoError(t, err, "mute dispatch stderr:\n%s", stderr.String())
	obWaitReport(t, ctx, missions, mute.MissionID, stderr, 120*time.Second, func(r *missionservice.Report) bool {
		return r.Kind == missionservice.ReportKindBlocker && strings.Contains(r.Summary, silentTurnBlockerLead)
	})

	// (3) gated: durable ask lands and blocks; answering it releases the unit.
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

	require.Empty(t, obReadFile(gatedReportPath), "the gated unit must still be parked on its permission ask")

	require.NoError(t, hitl.Respond(ctx, ask.ID, true))

	require.Eventually(t, func() bool {
		return obReadFile(gatedReportPath) == "gated-action outcome=selected option=allow-once"
	}, 60*time.Second, 100*time.Millisecond,
		"the answered gated unit never continued past its gate\nstderr:\n%s", stderr.String())

	answered, err := store.GetHITLApproval(ctx, ask.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, answered.State, "the answered ask is durably approved")
	require.Empty(t, obPendingForMission(t, ctx, hitl, gated.MissionID), "no ask stays pending for the gated mission")

	obWaitReport(t, ctx, missions, gated.MissionID, stderr, 60*time.Second, func(r *missionservice.Report) bool {
		return r.Kind == missionservice.ReportKindBlocker && strings.Contains(r.Summary, silentTurnBlockerLead)
	})

	// (4) planner: the living plan revised past its initial snapshot.
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

	mReporter, err := missions.Get(ctx, reporter.MissionID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusLanded, mReporter.Status)
	require.NotNil(t, mReporter.LastHeartbeat, "a completed turn stamps mission liveness")
	require.Equal(t, 0, mReporter.Plan.Revision, "the reporter never touched a plan")
	rReporter, err := missions.ListReports(ctx, reporter.MissionID, 10)
	require.NoError(t, err)
	require.Len(t, rReporter, 1)
	require.Equal(t, missionservice.ReportKindResult, rReporter[0].Kind)

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

	mGated, err := missions.Get(ctx, gated.MissionID)
	require.NoError(t, err)
	require.NotNil(t, mGated.LastHeartbeat)
	require.Equal(t, "gated-action outcome=selected option=allow-once", obReadFile(gatedReportPath))
	answeredAgain, err := store.GetHITLApproval(ctx, ask.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, answeredAgain.State)
	require.NotNil(t, answeredAgain.MissionID)
	require.Equal(t, gated.MissionID, *answeredAgain.MissionID, "attribution survives the answer")

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

	require.NotEqual(t, missionservice.StatusLanded, mMute.Status)
	require.NotEqual(t, missionservice.StatusLanded, mPlanner.Status)

	// ── The operator inbox ──────────────────────────────────────────────────────

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
