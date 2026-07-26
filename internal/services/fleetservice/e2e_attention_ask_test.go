package fleetservice

import (
	"context"
	"encoding/json"
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
	"github.com/contenox/beam/internal/services/chainagents"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

const (
	askFixtureQuestion = "which project did you mean?"
	askFixtureAnswer   = "the contenox runtime repo, and only its docs/ tree"
	askFixtureEchoLead = "UNIT HEARD:"
)

// writeAskChainAgentFixture writes a deterministic, model-free chain unit that
// ASKS its operator a question and then echoes the answer it was given. The echo
// is the proof: the second task's print interpolates the FIRST task's output,
// which is the mission tool's result — so the operator's own words can only
// appear in the transcript if they travelled all the way back into the unit.
func writeAskChainAgentFixture(t *testing.T, contenoxDir string) string {
	t.Helper()
	chain := map[string]any{
		"id":          "agent-ask-fixture",
		"description": "Deterministic no-model chain unit: ask the operator, echo the answer, report.",
		"tasks": []map[string]any{
			{
				"id":      "ask",
				"handler": "tools",
				"tools": map[string]any{
					"name":      "mission",
					"tool_name": "mission_ask_attention",
					"args":      map[string]any{"summary": askFixtureQuestion},
				},
				"transition": map[string]any{
					"branches": []map[string]any{{"operator": "default", "goto": "echo"}},
				},
			},
			{
				"id":      "echo",
				"handler": "noop",
				"print":   askFixtureEchoLead + " {{.ask}}",
				"transition": map[string]any{
					"branches": []map[string]any{{"operator": "default", "goto": "report"}},
				},
			},
			{
				// Reporting ends the drive loop's interest in this unit, so the
				// nudge never fires and the test observes exactly the turn it cares
				// about.
				"id":      "report",
				"handler": "tools",
				"tools": map[string]any{
					"name":      "mission",
					"tool_name": "mission_report",
					"args":      map[string]any{"kind": "result", "summary": "answered using the operator's reply"},
				},
				"transition": map[string]any{
					"branches": []map[string]any{{"operator": "default", "goto": "end"}},
				},
			},
		},
	}
	data, err := json.Marshal(chain)
	require.NoError(t, err)
	path := filepath.Join(contenoxDir, "agent-ask-fixture.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

// TestFleetE2E_AttentionAsk_OperatorAnswerReachesTheUnit is the acceptance for
// the ask channel: an unattended unit asks a human a question, the question
// lands where an operator can see AND answer it, and the operator's own words
// come back to the unit as the result of the tool call it is parked on — so it
// continues with them instead of guessing or giving up.
//
// Before this existed, mission_ask_attention was a one-way flare: the tool was
// described to the model as "ask a question", every question was downgraded to a
// blocker report, and no surface could answer one. A unit could ask; nobody
// could reply.
//
// It also pins the CROSS-PROCESS shape, which is the whole reason the wait
// watches the durable row and not just an in-process channel: the unit raises
// its ask inside its own spawned process, while this test — standing in for
// `contenox serve`, where the API lives — answers it from another. Nothing is
// mocked: a real spawned unit, the real ask store, no LLM or network.
func TestFleetE2E_AttentionAsk_OperatorAnswerReachesTheUnit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping attention-ask e2e: builds and boots the full contenox binary")
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
	runContenoxCLI(t, bin, home, "config", "set", "default-model", "ask-fixture-model")
	runContenoxCLI(t, bin, home, "config", "set", "update-check", "false")

	contenoxDir := filepath.Join(home, ".contenox")
	require.DirExists(t, contenoxDir)
	writeAskChainAgentFixture(t, contenoxDir)

	ctx := context.Background()

	// THE UNIT'S OWN DATABASE — $HOME/.contenox/local.db, the one its spawned
	// process boots against and raises its ask into. The operator's side of this
	// test reads and answers through it, which is exactly the crossing serve makes
	// (the unit is a different process; the shared file is the meeting point).
	unitDBPath := filepath.Join(contenoxDir, "local.db")
	unitDB, err := libdb.NewSQLiteDBManager(ctx, unitDBPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unitDB.Close() })
	operatorHITL := hitlservice.NewWithDefaultPolicy(
		hitlservice.NewFSPolicySource(contenoxDir), runtimetypes.LocalTenantID,
		runtimetypes.New(unitDB.WithoutTransaction()), libtracker.NoopTracker{}, "")

	agents := agentregistryservice.New(unitDB)
	bus := libbus.NewInMem()
	t.Cleanup(func() { _ = bus.Close() })
	missions := missionservice.New(unitDB, missionservice.WithEventPublisher(bus))

	res, err := chainagents.Discover(ctx, agents, contenoxDir)
	require.NoError(t, err)
	require.Contains(t, res.Created, "agent-ask-fixture")

	stderr := &lockedBuffer{}
	instances := agentinstance.New(agents,
		agentinstance.WithSelfExecutable(bin),
		agentinstance.WithStderr(stderr),
	)
	t.Cleanup(func() { _ = instances.Close() })

	svc := New(instances, agents, missions, nil, home, libtracker.NoopTracker{})

	dispatched, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:      "agent-ask-fixture",
		Intent:         "explain this project's core moat",
		HITLPolicyName: "default",
	})
	require.NoError(t, err, "dispatch stderr:\n%s", stderr.String())

	viewer := &recordingViewer{id: "ask-observer"}
	_, err = instances.Attach(ctx, dispatched.InstanceID, libacp.SessionID(dispatched.SessionID), viewer)
	require.NoError(t, err)

	// (1) The unit's question reaches the operator's queue — visible, attributed
	// to its mission, and recognisable as a question rather than a gate.
	var ask *runtimetypes.HITLApproval
	require.Eventually(t, func() bool {
		rows, err := operatorHITL.ListPending(ctx, 20)
		if err != nil {
			return false
		}
		for _, row := range rows {
			if hitlservice.IsAttentionAsk(row) {
				ask = row
				return true
			}
		}
		return false
	}, 120*time.Second, 200*time.Millisecond,
		"the unit's question never reached the operator's ask queue\nstderr:\n%s", stderr.String())

	require.Equal(t, askFixtureQuestion, ask.ArgsSummary)
	require.NotNil(t, ask.MissionID, "the question must name the mission it came from")
	require.Equal(t, dispatched.MissionID, *ask.MissionID)

	// (2) The operator answers with words — from this process, not the unit's.
	require.NoError(t, operatorHITL.Answer(ctx, ask.ID, askFixtureAnswer))

	// (3) THE POINT: the answer comes back to the unit as its tool result, and the
	// unit's next step runs with it.
	require.Eventually(t, func() bool {
		return strings.Contains(viewer.messageText(), askFixtureEchoLead+" "+askFixtureAnswer)
	}, 120*time.Second, 100*time.Millisecond,
		"the operator's answer never reached the asking unit; transcript=%q\nstderr:\n%s",
		viewer.messageText(), stderr.String())

	// (4) And the mission carries on: the unit reported a result, so the question
	// was a step in the work rather than the end of it.
	require.Eventually(t, func() bool {
		reports, err := missions.ListReports(ctx, dispatched.MissionID, 10)
		if err != nil {
			return false
		}
		for _, r := range reports {
			if r.Kind == missionservice.ReportKindResult {
				return true
			}
		}
		return false
	}, 60*time.Second, 100*time.Millisecond, "the unit never reported after being answered")
}
