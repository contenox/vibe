package fleetservice

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	libbus "github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/operatorinbox"
	"github.com/contenox/beam/internal/services/reportrouter"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// missionRevisionChain is a deterministic, model-free PLANNER chain that shows the
// planner heartbeat's plan-revision half without inference. It runs each turn and
// branches on the plan REVISION the mission_plan tool echoes back — the only
// turn-distinguishing state a deterministic chain can read, because none of the
// mission tools are read-only (a real resident planner, being a model, branches on
// its conversation instead; see the test's own comment on the machinery gap):
//
//   - `gate` writes the INITIAL snapshot S1 and renders the stored revision as its
//     transition eval (output_template "{{.Revision}}"). On the FIRST turn that
//     write is revision 1, so the chain ends with the initial plan in place. On any
//     LATER turn the same write lands as revision 2+, so the chain falls through to
//     `revise`.
//   - `revise` writes the REVISED snapshot S2 — survey completed, port advanced,
//     and a NEW `bench` entry the initial plan did not have (the +1 delta a
//     PlanRevisedEvent reports). S2 carries the same stable ids forward, so the
//     revision is a genuine carry-forward, not a fresh plan.
//
// The gate re-writing S1 on the second turn is a benign artifact of that
// read-only-probe limitation (it is a zero-delta bump: same ids, same content); the
// meaningful revision is `revise`'s S1->S2 write, and the durable final state is S2.
const missionRevisionChain = `{
  "id": "e2e-mission-revision-loop",
  "tasks": [
    {
      "id": "gate",
      "handler": "tools",
      "tools": {
        "name": "mission",
        "tool_name": "mission_plan",
        "args": {
          "entries": "[{\"id\":\"survey\",\"content\":\"survey the codebase\",\"status\":\"in_progress\",\"priority\":\"high\"},{\"id\":\"port\",\"content\":\"port the hot loop\",\"status\":\"pending\",\"priority\":\"medium\"}]",
          "explanation": "initial plan"
        }
      },
      "output_template": "{{.Revision}}",
      "transition": {
        "branches": [
          {"operator": "equals", "when": "1", "goto": "done"},
          {"operator": "default", "when": "", "goto": "revise"}
        ]
      }
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
      "transition": {"branches": [{"operator": "default", "when": "", "goto": "done"}]}
    },
    {
      "id": "done",
      "handler": "noop",
      "transition": {"branches": [{"operator": "default", "when": "", "goto": "end"}]}
    }
  ]
}`

// TestFleetService_E2E_MissionRevisionLoop is the composed acceptance for the
// planner heartbeat's plan-revision half — a real dispatched planner unit revises
// its living plan on the turn AFTER a worker's report is delivered into its
// supervising session, through the SAME real components serve wires:
//
//	dispatch → planner unit sets the initial plan (turn 1, revision 1)
//	  → a CHILD mission's report (with a typed handover) is added through the
//	    publisher-wired missionservice → ReportAddedEvent on a real bus →
//	    reportrouter → agentinstance.DeliverToSession into the planner's session
//	  → the planner's NEXT turn revises the plan (revision advances, a new entry
//	    persists) — the report-in → plan-revised beat of the resident loop.
//
// # The honest variant, and the machinery gap it documents
//
// The blueprint's resident planner WAKES on a delivered report and takes its next
// turn autonomously. That autonomous wake does not exist yet: DeliverToSession
// injects a report into the session's fan-out (its replay journal and any attached
// viewer), but a delivered message does NOT start a turn on its own, and it is not
// folded into the dispatched subprocess's own conversation. So this test drives the
// next turn EXPLICITLY with a kernel Prompt after delivering the report — proving
// the plan-revision-off-report SEQUENCE (report delivered, then the next turn
// revises) while the wake-on-report trigger and report-into-conversation folding
// remain the resident-planner follow-up. The deterministic chain revises off the
// plan's own revision state rather than the report's text for the same reason a
// read-only mission probe does not exist; a model planner reads the report from its
// conversation, which this walking skeleton stands in for.
func TestFleetService_E2E_MissionRevisionLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: builds the contenox binary and spawns a real ACP subprocess")
	}

	bin := buildContenoxBin(t)

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	contenoxDir := filepath.Join(tmpHome, ".contenox")
	require.NoError(t, os.MkdirAll(contenoxDir, 0o700))
	dbPath := filepath.Join(contenoxDir, "local.db")

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	runContenox(t, bin, "config", "set", "default-model", "fake-e2e-model-does-not-exist")

	chainPath := filepath.Join(contenoxDir, "mission-chain.json")
	require.NoError(t, os.WriteFile(chainPath, []byte(missionRevisionChain), 0o644))

	agents := agentregistryservice.New(db)
	agent := &runtimetypes.Agent{Name: "planner", Enabled: true}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   bin,
		Args:      []string{"acp", "--auto"},
		Env:       map[string]string{"CONTENOX_ACP_CHAIN_PATH": chainPath},
	}))
	require.NoError(t, agents.Create(ctx, agent))

	stderr := &lockedBuffer{}
	kernel := agentinstance.New(agents, agentinstance.WithStderr(stderr))
	t.Cleanup(func() { _ = kernel.Close() })

	// The mission store is publisher-wired (the bus serve uses), so a report added
	// to any mission publishes a ReportAddedEvent the router consumes.
	bus := libbus.NewInMem()
	t.Cleanup(func() { _ = bus.Close() })
	missions := missionservice.New(db, missionservice.WithEventPublisher(bus))
	inbox := operatorinbox.New(db)

	// The routing service under test, wired exactly as serve wires it: the kernel
	// is the SessionDeliverer, the inbox is the fallback.
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

	svc := New(kernel, agents, missions, nil, tmpHome, libtracker.NoopTracker{})

	// ── Turn 1: dispatch the planner; it sets the initial plan (revision 1) ──────
	planner, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:      "planner",
		Intent:         "plan the migration and hold the plan",
		HITLPolicyName: "default",
	})
	require.NoError(t, err, "planner dispatch stderr:\n%s", stderr.String())
	require.NotEmpty(t, planner.MissionID)

	var m *missionservice.Mission
	require.Eventuallyf(t, func() bool {
		m, err = missions.Get(ctx, planner.MissionID)
		return err == nil && m.Plan.Revision >= 1
	}, 45*time.Second, 150*time.Millisecond,
		"the planner never set its initial plan.\nstderr:\n%s", stderr.String())

	require.Equal(t, 1, m.Plan.Revision, "turn 1 sets the initial plan as revision 1")
	require.Len(t, m.Plan.Entries, 2)
	require.Equal(t, "survey", m.Plan.Entries[0].ID)
	require.Equal(t, missionservice.PlanEntryInProgress, m.Plan.Entries[0].Status)
	require.Equal(t, "port", m.Plan.Entries[1].ID)
	require.False(t, planHasEntry(m.Plan, "bench"), "the initial plan has no benchmark step yet")

	// Observe the planner's stream so the delivered report can be seen landing in
	// the supervising session, exactly as a coordinating agent or an operator would.
	viewer := &recordingViewer{id: "planner-observer"}
	_, err = kernel.Attach(ctx, planner.InstanceID, libacp.SessionID(planner.SessionID), viewer)
	require.NoError(t, err)

	// ── Deliver a worker's report INTO the planner's session (reportrouter) ──────
	// A child mission supervised by the planner's session — the same supervision
	// edge fleetservice.Dispatch records and AddReport publishes. Its report carries
	// a typed hand-off, so the additive Handover rides the real publish→route path.
	child := &missionservice.Mission{
		Intent:          "the worker sub-unit that reports back",
		AgentName:       "worker",
		HITLPolicyName:  "default",
		ParentSessionID: planner.SessionID,
	}
	require.NoError(t, missions.Create(ctx, child))

	const workerSummary = "worker ported the hot loop"
	require.NoError(t, missions.AddReport(ctx, child.ID, &missionservice.Report{
		Kind:    missionservice.ReportKindResult,
		Summary: workerSummary,
		Handover: &missionservice.Handover{
			Outcome:         "hot loop ported; benchmarks pending",
			Artifacts:       []string{"src/hotloop.rs"},
			HandoverForNext: "pick up the benchmark harness against the baseline",
			Caveats:         "SIMD path untested on aarch64",
		},
	}))

	// The hand-off round-tripped through the real store (scope A, end to end).
	childReports, err := missions.ListReports(ctx, child.ID, 10)
	require.NoError(t, err)
	require.Len(t, childReports, 1)
	require.NotNil(t, childReports[0].Handover, "the typed hand-off survives the real AddReport")
	require.Equal(t, "hot loop ported; benchmarks pending", childReports[0].Handover.Outcome)

	// The report reached the PLANNER session's transcript (the reportrouter path).
	require.Eventuallyf(t, func() bool {
		return strings.Contains(viewer.messageText(), workerSummary)
	}, 30*time.Second, 50*time.Millisecond,
		"the worker report never reached the planner session; transcript=%q\nstderr:\n%s", viewer.messageText(), stderr.String())

	// ── Turn 2: drive the next turn explicitly; the planner revises its plan ─────
	// (the honest variant — see the test doc; a delivered message does not itself
	// start a turn, so we prompt the resident planner's next beat.)
	_, err = kernel.Prompt(ctx, planner.InstanceID, libacp.SessionID(planner.SessionID),
		[]libacp.ContentBlock{libacp.NewTextContent("A worker reported in. Reconcile your plan with what has landed.")})
	require.NoError(t, err, "turn 2 prompt stderr:\n%s", stderr.String())

	// The revised plan persists: the revision advanced and the NEW benchmark entry
	// (which revision 1 did not have) is now on the plan — a genuine +1 revision off
	// the report, durably observable across the subprocess boundary.
	require.Eventuallyf(t, func() bool {
		m, err = missions.Get(ctx, planner.MissionID)
		return err == nil && m.Plan.Revision > 1 && planHasEntry(m.Plan, "bench")
	}, 45*time.Second, 150*time.Millisecond,
		"the planner never revised its plan after the report.\nplan=%+v\nstderr:\n%s", m.Plan, stderr.String())

	require.Greater(t, m.Plan.Revision, 1, "the plan revision advanced past the initial plan")
	require.Len(t, m.Plan.Entries, 3, "the revised plan carries the new benchmark step")
	require.True(t, planHasEntry(m.Plan, "bench"), "the revision added a benchmark step off the report")
	survey := planEntryByID(m.Plan, "survey")
	require.NotNil(t, survey, "the carried-forward survey entry kept its id")
	require.Equal(t, missionservice.PlanEntryCompleted, survey.Status, "the revision advanced survey to completed")

	// The plan_revised event's added/removed SHAPE — the "+1/−0" the inbox renders —
	// is asserted in-process by missionservice's own unit tests
	// (TestUnit_MissionService_SetPlanPublishesPlanRevisedEvent /
	// TestUnit_MissionService_PlanRevisedEventCountsRemoved): the planner's plan
	// writes happen inside the dispatched SUBPROCESS, whose bus is not shared with
	// this parent test, so here the revision's durable delta (the new `bench` entry
	// that revision 1 lacked) is what is cross-process observable.

	require.NoError(t, svc.Stop(ctx, planner.InstanceID))
}

// planHasEntry reports whether the plan carries an entry with the given id.
func planHasEntry(plan missionservice.Plan, id string) bool {
	return planEntryByID(plan, id) != nil
}

// planEntryByID returns the plan entry with the given id, or nil.
func planEntryByID(plan missionservice.Plan, id string) *missionservice.PlanEntry {
	for i := range plan.Entries {
		if plan.Entries[i].ID == id {
			return &plan.Entries[i]
		}
	}
	return nil
}
