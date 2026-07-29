package fleetservice

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/operatorinbox"
	"github.com/contenox/contenox/internal/services/reportrouter"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// missionRevisionChain is a deterministic planner chain: `gate` writes the
// initial plan on turn 1 and ends there; any later turn falls through to
// `revise`, which writes the revised plan with a new `bench` entry.
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

// TestFleetService_E2E_MissionRevisionLoop: a planner revises its living plan
// on the turn after a child mission's report is delivered into its
// supervising session. The next turn is driven explicitly, since a
// delivered message does not itself start one.
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

	// Observe the planner's stream so the delivered report can be seen
	// landing in the supervising session.
	viewer := &recordingViewer{id: "planner-observer"}
	_, err = kernel.Attach(ctx, planner.InstanceID, libacp.SessionID(planner.SessionID), viewer)
	require.NoError(t, err)

	// ── Deliver a worker's report into the planner's session (reportrouter) ─────
	// A child mission supervised by the planner's session, whose report
	// carries a typed hand-off riding the real publish→route path.
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

	// The report reached the planner session's transcript.
	require.Eventuallyf(t, func() bool {
		return strings.Contains(viewer.messageText(), workerSummary)
	}, 30*time.Second, 50*time.Millisecond,
		"the worker report never reached the planner session; transcript=%q\nstderr:\n%s", viewer.messageText(), stderr.String())

	// ── Turn 2: drive the next turn explicitly; the planner revises its plan ────
	_, err = kernel.Prompt(ctx, planner.InstanceID, libacp.SessionID(planner.SessionID),
		[]libacp.ContentBlock{libacp.NewTextContent("A worker reported in. Reconcile your plan with what has landed.")})
	require.NoError(t, err, "turn 2 prompt stderr:\n%s", stderr.String())

	// The revised plan persists: the revision advanced and the new benchmark
	// entry is now on the plan, durably observable across the subprocess
	// boundary.
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

	// The plan_revised event's added/removed shape is asserted separately by
	// missionservice's own unit tests; the planner's writes happen inside the
	// dispatched subprocess, whose bus is not shared with this parent test,
	// so only the durable delta is cross-process observable here.

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
