package fleetservice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	libbus "github.com/contenox/contenox/internal/libbus"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/chainagents"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/libacp"
	"github.com/stretchr/testify/require"
)

const (
	askFixtureQuestion = "which project did you mean?"
	askFixtureAnswer   = "the contenox runtime repo, and only its docs/ tree"
	askFixtureEchoLead = "UNIT HEARD:"
)

// writeAskChainAgentFixture writes a chain that asks its operator a
// question, then echoes the answer via its print template.
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
				// Reporting ends the drive loop's interest, so the nudge never fires.
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

// TestFleetE2E_AttentionAsk_OperatorAnswerReachesTheUnit: an unattended
// unit's question lands where an operator can answer it, and the answer
// comes back as the result of the tool call the unit is parked on.
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

	// The unit's own database ($HOME/.contenox/local.db) is what its spawned
	// process boots against and raises its ask into; the operator's side
	// reads and answers through the same file.
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

	// (1) The unit's question reaches the operator's queue, attributed to
	// its mission.
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

	// (3) The answer comes back to the unit as its tool result.
	require.Eventually(t, func() bool {
		return strings.Contains(viewer.messageText(), askFixtureEchoLead+" "+askFixtureAnswer)
	}, 120*time.Second, 100*time.Millisecond,
		"the operator's answer never reached the asking unit; transcript=%q\nstderr:\n%s",
		viewer.messageText(), stderr.String())

	// (4) The mission carries on: the unit reported a result after being
	// answered.
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
