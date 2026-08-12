package fleetservice

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

const (
	probeToolsName = "local_fs"
	probeToolName  = "write_file"
)

func buildStubAgentBinary(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("external agent spawn runs through the sandbox, which is Landlock-based and Linux-only")
	}
	binPath := filepath.Join(t.TempDir(), "acp-stub-agent")
	cmd := exec.Command("go", "build", "-o", binPath, "github.com/contenox/contenox/libacp/cmd/acp-stub-agent")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build stub agent: %v\n%s", err, out)
	}
	return binPath
}

func writePolicy(t *testing.T, dir, name string, policy map[string]any) string {
	t.Helper()
	data, err := json.Marshal(policy)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	return name
}

type unattendedFixture struct {
	ctx        context.Context
	agents     agentregistryservice.Service
	missions   missionservice.Service
	hitl       hitlservice.Service
	store      runtimetypes.Store
	instances  agentinstance.Manager
	svc        Service
	stderr     *lockedBuffer
	policyDir  string
	reportPath string
}

func newUnattendedFixture(t *testing.T, bin string, args string) *unattendedFixture {
	t.Helper()
	ctx := context.Background()

	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "unattended-e2e.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	store := runtimetypes.New(db.WithoutTransaction())
	agents := agentregistryservice.New(db)
	missions := missionservice.New(db)

	policyDir := t.TempDir()
	hitl := hitlservice.New(hitlservice.NewFSPolicySource(policyDir), runtimetypes.LocalTenantID, store, libtracker.NoopTracker{})

	// workspace doubles as the report location: Landlock makes it the unit's only writable root.
	workspace := t.TempDir()
	fx := &unattendedFixture{
		ctx:        ctx,
		agents:     agents,
		missions:   missions,
		hitl:       hitl,
		store:      store,
		stderr:     &lockedBuffer{},
		policyDir:  policyDir,
		reportPath: filepath.Join(workspace, "gated-action-report.txt"),
	}

	agent := &runtimetypes.Agent{Name: "unattended-fixture", Enabled: true}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   bin,
		Env: map[string]string{
			"ACP_STUB_GATED_TOOLS_NAME":   probeToolsName,
			"ACP_STUB_GATED_TOOL_NAME":    probeToolName,
			"ACP_STUB_GATED_ARGS_JSON":    args,
			"ACP_STUB_GATED_REPORT_PATH":  fx.reportPath,
			"ACP_STUB_ADVERTISE_COMMANDS": "",
		},
	}))
	require.NoError(t, agents.Create(ctx, agent))

	fx.instances = agentinstance.New(agents,
		agentinstance.WithStderr(fx.stderr),
		agentinstance.WithPermissionFallback(NewUnattendedPermissionAnswerer(UnattendedPermissionDeps{
			HITL:     hitl,
			Missions: missions,
			Sink:     taskengine.NoopTaskEventSink{},
			Tracker:  libtracker.NoopTracker{},
		})),
	)
	t.Cleanup(func() { _ = fx.instances.Close() })

	fx.svc = New(fx.instances, agents, missions, nil, workspace, libtracker.NoopTracker{})
	return fx
}

func (fx *unattendedFixture) dispatch(t *testing.T, policyName string) DispatchResult {
	t.Helper()
	result, err := fx.svc.Dispatch(fx.ctx, DispatchRequest{
		AgentName:       "unattended-fixture",
		Intent:          "run the gated_action scenario",
		HITLPolicyName:  policyName,
		ParentSessionID: "upstream-session-fixture",
	})
	require.NoError(t, err, "unit stderr:\n%s", fx.stderr.String())
	require.NotEmpty(t, result.MissionID)
	return result
}

func (fx *unattendedFixture) pending(t *testing.T) []*runtimetypes.HITLApproval {
	t.Helper()
	rows, err := fx.hitl.ListPending(fx.ctx, 100)
	require.NoError(t, err)
	return rows
}

func (fx *unattendedFixture) awaitPending(t *testing.T) *runtimetypes.HITLApproval {
	t.Helper()
	var row *runtimetypes.HITLApproval
	require.Eventually(t, func() bool {
		rows := fx.pending(t)
		if len(rows) != 1 {
			return false
		}
		row = rows[0]
		return true
	}, 60*time.Second, 50*time.Millisecond,
		"an unattended permission request never reached the durable ask store\nstderr:\n%s", fx.stderr.String())
	return row
}

func (fx *unattendedFixture) report() string {
	// no viewer is attached anywhere in this file: one would become the session's controller and steal the permission request from the fallback.
	data, err := os.ReadFile(fx.reportPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (fx *unattendedFixture) awaitReport(t *testing.T) string {
	t.Helper()
	var line string
	require.Eventually(t, func() bool {
		line = fx.report()
		return line != ""
	}, 60*time.Second, 50*time.Millisecond,
		"the unit never reported its permission outcome\nstderr:\n%s", fx.stderr.String())
	return line
}

func (fx *unattendedFixture) requireUnattended(t *testing.T, result DispatchResult) {
	t.Helper()
	status, err := fx.svc.Get(fx.ctx, result.InstanceID)
	require.NoError(t, err)
	require.Zero(t, status.Viewers, "the dispatched unit must have no viewer attached")
	require.Contains(t, status.SessionIDs, result.SessionID)
}

// TestFleetE2E_UnattendedPermission_ReachesInboxAndUnblocksOnAnswer: a
// mission's unit with no viewer attached asks permission for an action its
// envelope does not pre-authorize; the ask lands durably with attribution,
// and answering it through the service releases the unit.
func TestFleetE2E_UnattendedPermission_ReachesInboxAndUnblocksOnAnswer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping unattended-permission e2e: builds and spawns a real ACP agent")
	}

	bin := buildStubAgentBinary(t)
	fx := newUnattendedFixture(t, bin, `{"path":"/tmp/unattended-fixture.txt"}`)

	envelope := writePolicy(t, fx.policyDir, "envelope-ask.json", map[string]any{
		"default_action": "approve",
		"rules": []map[string]any{
			{"tools": probeToolsName, "tool": probeToolName, "action": "approve"},
		},
	})

	result := fx.dispatch(t, envelope)

	mission, err := fx.missions.Get(fx.ctx, result.MissionID)
	require.NoError(t, err)
	require.Equal(t, envelope, mission.HITLPolicyName)
	require.Equal(t, "upstream-session-fixture", mission.ParentSessionID,
		"the mission must name the session that fired it")
	require.Equal(t, result.InstanceID, mission.InstanceID)

	fx.requireUnattended(t, result)

	row := fx.awaitPending(t)
	require.Equal(t, probeToolsName, row.ToolsName)
	require.Equal(t, probeToolName, row.ToolName)
	require.Equal(t, envelope, row.PolicyName, "the row names the envelope that escalated it")
	require.Equal(t, result.InstanceID, row.InstanceID)
	require.Equal(t, result.SessionID, row.SessionID)
	require.Equal(t, "unattended-fixture", row.AgentName)
	require.NotNil(t, row.MissionID, "an ask raised on a mission must carry its mission id")
	require.Equal(t, result.MissionID, *row.MissionID)
	require.Equal(t, "/tmp/unattended-fixture.txt", row.ArgsSummary,
		"the ask must carry the arguments the downstream sent, so a human can decide")

	require.Empty(t, fx.report(), "the unit must still be parked on its permission request")

	require.NoError(t, fx.hitl.Respond(fx.ctx, row.ID, true))

	require.Equal(t, "gated-action outcome=selected option=allow-once", fx.awaitReport(t))
	require.Empty(t, fx.pending(t), "the answered ask must leave the pending inbox")

	answered, err := fx.store.GetHITLApproval(fx.ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, answered.State)
	require.NotNil(t, answered.MissionID, "attribution survives the answer")
	require.Equal(t, result.MissionID, *answered.MissionID)

	require.NoError(t, fx.svc.Stop(fx.ctx, result.InstanceID))
}

// TestFleetE2E_UnattendedPermission_RuleAllowedNeedsNoHuman: an action the
// mission's policy allows is answered immediately, unattended, with no
// durable ask created.
func TestFleetE2E_UnattendedPermission_RuleAllowedNeedsNoHuman(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping unattended-permission e2e: builds and spawns a real ACP agent")
	}

	bin := buildStubAgentBinary(t)
	fx := newUnattendedFixture(t, bin, `{"path":"/tmp/unattended-fixture.txt"}`)

	envelope := writePolicy(t, fx.policyDir, "envelope-allow.json", map[string]any{
		"default_action": "approve",
		"rules": []map[string]any{
			{"tools": probeToolsName, "tool": probeToolName, "action": "allow"},
		},
	})

	result := fx.dispatch(t, envelope)
	fx.requireUnattended(t, result)

	require.Equal(t, "gated-action outcome=selected option=allow-once", fx.awaitReport(t))
	require.Empty(t, fx.pending(t),
		"an action the envelope allows must cost nobody's attention: no durable ask may be created")

	all, err := fx.store.ListHITLApprovals(fx.ctx, runtimetypes.HITLApprovalApproved, nil, 100)
	require.NoError(t, err)
	require.Empty(t, all)

	require.NoError(t, fx.svc.Stop(fx.ctx, result.InstanceID))
}

// TestFleetE2E_UnattendedPermission_UnansweredExpiresByPolicy: an unanswered
// ask expires by policy, a bounded wait with a declared on-timeout, rather
// than hanging forever.
func TestFleetE2E_UnattendedPermission_UnansweredExpiresByPolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping unattended-permission e2e: builds and spawns a real ACP agent")
	}

	bin := buildStubAgentBinary(t)
	fx := newUnattendedFixture(t, bin, `{"path":"/tmp/unattended-fixture.txt"}`)

	envelope := writePolicy(t, fx.policyDir, "envelope-timeout.json", map[string]any{
		"default_action": "approve",
		"rules": []map[string]any{
			{"tools": probeToolsName, "tool": probeToolName, "action": "approve", "timeout_s": 2, "on_timeout": "deny"},
		},
	})

	result := fx.dispatch(t, envelope)
	fx.requireUnattended(t, result)
	row := fx.awaitPending(t)
	require.WithinDuration(t, row.CreatedAt.Add(2*time.Second), row.ExpiresAt, time.Second,
		"the row's deadline must come from the rule, not from the serve-level ceiling")

	require.Equal(t, "gated-action outcome=selected option=reject-once", fx.awaitReport(t))

	pending := fx.pending(t)
	require.Len(t, pending, 1)
	require.Equal(t, row.ID, pending[0].ID)

	require.Eventually(t, func() bool {
		n, err := fx.hitl.SweepExpired(fx.ctx)
		require.NoError(t, err)
		return n == 1
	}, 30*time.Second, 100*time.Millisecond, "the expiry sweeper must resolve the abandoned ask")

	expired, err := fx.store.GetHITLApproval(fx.ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalExpired, expired.State)
	require.Empty(t, fx.pending(t))

	require.NoError(t, fx.svc.Stop(fx.ctx, result.InstanceID))
}
