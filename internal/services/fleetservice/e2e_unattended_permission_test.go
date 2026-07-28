package fleetservice

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// This file is the acceptance for a mission that fires a unit with no viewer
// attached: the unit asks permission, and the ask reaches a human via a real
// subprocess, real dispatch path, and real durable ask row, ending answered,
// pre-authorized (no ask created), or expired by policy.

// The tool identity the fixture unit asks permission for.
const (
	probeToolsName = "local_fs"
	probeToolName  = "write_file"
)

// buildStubAgentBinary compiles the repo's ACP stub agent into t.TempDir() and
// returns its path; the go build cache makes reruns cheap. Mirrors
// buildContenoxBinary in the chain-dispatch e2e.
func buildStubAgentBinary(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "acp-stub-agent")
	cmd := exec.Command("go", "build", "-o", binPath, "github.com/contenox/beam/libacp/cmd/acp-stub-agent")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build stub agent: %v\n%s", err, out)
	}
	return binPath
}

// writePolicy writes a HITL policy document into dir under name and returns the
// name, so a test can name it as a mission's envelope.
func writePolicy(t *testing.T, dir, name string, policy map[string]any) string {
	t.Helper()
	data, err := json.Marshal(policy)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	return name
}

// unattendedFixture is everything one case of this e2e needs, wired the way
// `contenox serve` wires it: one Manager whose permission fallback is the
// unattended answerer over this HITL service and this mission store.
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

// newUnattendedFixture builds the fixture: an isolated database, a policy
// directory, and the real kernel with the real fallback wired in. args names
// the tool call the spawned unit asks permission for.
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

	// One directory serves as both the unit's workspace and the place it
	// reports through: a dispatched unit's only writable root is its
	// workspace (Landlock), so a report file elsewhere would fail silently.
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

// dispatch fires a mission at the fixture unit under the named envelope; the
// intent doubles as the fixture agent's scenario trigger.
func (fx *unattendedFixture) dispatch(t *testing.T, policyName string) DispatchResult {
	t.Helper()
	result, err := fx.svc.Dispatch(fx.ctx, DispatchRequest{
		AgentName:      "unattended-fixture",
		Intent:         "run the gated_action scenario",
		HITLPolicyName: policyName,
		// The supervision edge: fired by a session, not an operator directly.
		ParentSessionID: "upstream-session-fixture",
	})
	require.NoError(t, err, "unit stderr:\n%s", fx.stderr.String())
	require.NotEmpty(t, result.MissionID)
	return result
}

// pending returns the currently pending asks, newest first.
func (fx *unattendedFixture) pending(t *testing.T) []*runtimetypes.HITLApproval {
	t.Helper()
	rows, err := fx.hitl.ListPending(fx.ctx, 100)
	require.NoError(t, err)
	return rows
}

// awaitPending waits for exactly one pending ask and returns it.
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

// report reads the outcome line the unit wrote out-of-band, or "" while it
// has written none. Nothing in this file ever attaches a viewer, since that
// would give the session a controller and route the permission request to
// it instead of the fallback, so the unit reports through a file instead.
func (fx *unattendedFixture) report() string {
	data, err := os.ReadFile(fx.reportPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// awaitReport waits for the unit's outcome line and returns it.
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

// requireUnattended asserts the dispatched unit has nobody watching it, the
// precondition every case here depends on.
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

	// The envelope pre-authorizes exactly this action; everything else still
	// needs a human (default_action approve).
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

	// The inbox is empty because no row was ever created, not because one
	// was cleaned up.
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

	// The envelope asks, but declares how long it is willing to wait and
	// what to do when that runs out.
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

	// Nobody answers; the unit is released by the rule's on_timeout.
	require.Equal(t, "gated-action outcome=selected option=reject-once", fx.awaitReport(t))

	// The row is still there, still pending, until the sweeper closes it out.
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
