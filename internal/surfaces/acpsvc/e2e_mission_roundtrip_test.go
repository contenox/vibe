package acpsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/chainagents"
	"github.com/contenox/contenox/internal/services/fleetservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/operatorinbox"
	"github.com/contenox/contenox/internal/services/reportrouter"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// This file is the composed acceptance for fleet mission-mode: a live session
// fires /mission, fleetservice dispatches a real unit that files a report
// through mission_report, and the report-router delivers it back onto the
// firing session's stream (or the operator inbox when fired directly), through
// the same components `contenox serve` wires. The firing session must be a
// kernel-hosted unit, not a synthetic id, since the router only delivers to
// sessions a live kernel instance owns. Assertion (a) drives the unit's own
// report through /mission end to end; assertion (b) adds a report on the same
// edge through the publisher-wired mission service, since a unit's own filed
// report does not yet emit a routing event (closed by the sibling test below).

// mrtChainReply is the byte-exact reply the firing session's fixture chain
// streams, confirming the firing unit is fully up before the mission fires.
const mrtChainReply = "contenox mission roundtrip firing-session fixture reply"

// mrtMissionReportChain is the deterministic, model-free chain the dispatched
// reporter unit runs: a `tools` task calling mission_report with static args,
// then a noop terminator, proving the report path rather than inference.
// Mirrors e2e_mission_report_test.go so both acceptances file identical reports.
const mrtMissionReportChain = `{
  "id": "e2e-mission-roundtrip-report",
  "tasks": [
    {
      "id": "report",
      "handler": "tools",
      "tools": {
        "name": "mission",
        "tool_name": "mission_report",
        "args": {"kind": "result", "summary": "unit reporting from the field"}
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

func TestFleetE2E_MissionRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping mission round-trip e2e: builds the contenox binary and spawns real ACP subprocesses")
	}

	bin := mrtBuildContenoxBinary(t)
	home := t.TempDir()

	// Neutralize every ambient CONTENOX_* override so a value in the developer's
	// shell can't redirect a unit's boot or its chain.
	t.Setenv("HOME", home)
	for _, k := range []string{
		"CONTENOX_DEFAULT_MODEL", "CONTENOX_DEFAULT_PROVIDER",
		"CONTENOX_DEFAULT_ALT_MODEL", "CONTENOX_DEFAULT_ALT_PROVIDER",
		"CONTENOX_DEFAULT_MAX_TOKENS", "CONTENOX_DEFAULT_THINK",
		"CONTENOX_ACP_CHAIN_PATH",
	} {
		t.Setenv(k, "")
	}

	// Seed state through the real CLI. default-mission-agent/-policy are what
	// the /mission handler resolves the fired mission's agent and envelope from.
	mrtRunCLI(t, bin, home, "config", "set", "default-model", "mission-roundtrip-fake-model")
	mrtRunCLI(t, bin, home, "config", "set", "update-check", "false")
	mrtRunCLI(t, bin, home, "config", "set", "default-mission-agent", "reporter")
	mrtRunCLI(t, bin, home, "config", "set", "default-mission-policy", "default")

	contenoxDir := filepath.Join(home, ".contenox")
	require.DirExists(t, contenoxDir, "the CLI seeding run must have created the isolated state directory")

	// The one shared store: units and this handle both resolve
	// $HOME/.contenox/local.db, so a unit's report row is visible here.
	dbPath := filepath.Join(contenoxDir, "local.db")
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// The composed stack, wired the way serve_cmd.go wires it.
	agentRegistry := agentregistryservice.New(db)

	// The bus is the supervision edge's producer transport, SQLite-backed so
	// ReportAddedEvent survives the process boundary a fleet unit is.
	bus := libbus.NewSQLite(db.WithoutTransaction())
	t.Cleanup(func() { _ = bus.Close() })
	missions := missionservice.New(db, missionservice.WithEventPublisher(bus))
	operatorInbox := operatorinbox.New(db)

	// WithSelfExecutable points a chain unit's re-exec at the freshly built
	// binary, since os.Executable() under `go test` is the test binary itself.
	stderr := &mrtLockedBuffer{}
	kernel := agentinstance.New(agentRegistry,
		agentinstance.WithSelfExecutable(bin),
		agentinstance.WithStderr(stderr),
	)
	t.Cleanup(func() { _ = kernel.Close() })

	router, err := reportrouter.New(reportrouter.Deps{
		Bus:      bus,
		Sessions: kernel,
		Inbox:    operatorInbox,
		Tracker:  libtracker.NoopTracker{},
	})
	require.NoError(t, err)
	stopRouter, err := router.Start(ctx)
	require.NoError(t, err)
	t.Cleanup(stopRouter)

	fleet := fleetservice.New(kernel, agentRegistry, missions, nil, home, libtracker.NoopTracker{})

	// The firing/supervising session is a chain unit declared by convention
	// (the agent-*.json filename), discovered the way serve discovers operator
	// chains.
	mrtWriteChainAgentFixture(t, contenoxDir)
	res, err := chainagents.Discover(ctx, agentRegistry, contenoxDir)
	require.NoError(t, err)
	require.Contains(t, res.Created, "agent-fleet-fixture", "the agent-*.json file must declare the chain agent")

	// The reporter is the fired unit: a `contenox acp --auto` subprocess (no
	// HITL, so mission_report runs unattended), sharing this HOME/DB.
	chainPath := filepath.Join(contenoxDir, "mission-chain.json")
	require.NoError(t, os.WriteFile(chainPath, []byte(mrtMissionReportChain), 0o644))
	reporterAgent := &runtimetypes.Agent{Name: "reporter", Enabled: true}
	require.NoError(t, reporterAgent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   bin,
		Args:      []string{"acp", "--auto"},
		Env:       map[string]string{"CONTENOX_ACP_CHAIN_PATH": chainPath},
	}))
	require.NoError(t, agentRegistry.Create(ctx, reporterAgent))

	firing, err := fleet.Dispatch(ctx, fleetservice.DispatchRequest{
		AgentName:      "agent-fleet-fixture",
		Intent:         "be the supervising session that fires the mission",
		HITLPolicyName: "default",
	})
	require.NoError(t, err, "firing-session dispatch stderr:\n%s", stderr.String())
	t.Cleanup(func() { _ = fleet.Stop(ctx, firing.InstanceID) })

	viewer := &mrtRecordingViewer{id: "supervisor-observer"}
	_, err = kernel.Attach(ctx, firing.InstanceID, libacp.SessionID(firing.SessionID), viewer)
	require.NoError(t, err)

	// Wait until the firing unit is fully up before the reporter's bring-up
	// below, so the two don't seed one sqlite file concurrently.
	require.Eventually(t, func() bool {
		return strings.Contains(viewer.messageText(), mrtChainReply)
	}, 120*time.Second, 100*time.Millisecond,
		"firing session never came up; transcript=%q\nstderr:\n%s", viewer.messageText(), stderr.String())

	// The same collaborators serve gives the transport; conn is nil so command
	// output updates no-op, leaving only the handler's real work to assert on.
	tr := &Transport{deps: Deps{DB: db, Fleet: fleet, Agents: agentRegistry}}
	sess := &sessionEntry{InternalSessionID: firing.SessionID}

	name, args, ok := parseCommand("/mission reporter run the mission and report in")
	require.True(t, ok, "/mission must be recognized as a command")
	require.Equal(t, "mission", name)

	out, err := tr.handleMission(ctx, sess, args)
	require.NoError(t, err, "the /mission handler must fire successfully")
	require.Contains(t, out, "reporter", "the confirmation names the fired agent")

	// Find the sub-mission /mission dispatched: the reporter mission parented
	// on the firing session.
	sub := mrtFindReporterSubmission(t, ctx, missions, firing.SessionID)
	require.Equal(t, firing.SessionID, sub.ParentSessionID,
		"the fired mission's parent is the firing session — the supervision edge /mission records")
	require.NotEmpty(t, sub.InstanceID, "the fired mission is bound to its unit's instance")
	t.Cleanup(func() { _ = fleet.Stop(ctx, sub.InstanceID) })

	// (a) The reporter unit files a real report through its mission tool.
	var reports []*missionservice.Report
	require.Eventually(t, func() bool {
		reports, err = missions.ListReports(ctx, sub.ID, 10)
		require.NoError(t, err)
		return len(reports) == 1
	}, 60*time.Second, 150*time.Millisecond,
		"the fired unit must file exactly one report on its own mission\nstderr:\n%s", stderr.String())
	require.Equal(t, missionservice.ReportKindResult, reports[0].Kind)
	require.Equal(t, "unit reporting from the field", reports[0].Summary)
	require.Equal(t, sub.ID, reports[0].MissionID,
		"the report is scoped to the unit's OWN mission, forwarded at session/new")

	// (b) A report on that edge routes back into the firing session's stream,
	// driven through the publisher-wired mission service (serve's REST report
	// path), since the unit's own filed report currently emits no routing event.
	const deliveredSummary = "sub-unit result routed to the firing session"
	require.NoError(t, missions.AddReport(ctx, sub.ID, &missionservice.Report{
		Kind:    missionservice.ReportKindResult,
		Summary: deliveredSummary,
	}))

	require.Eventually(t, func() bool {
		return viewer.receivedMissionReport(sub.ID, deliveredSummary)
	}, 30*time.Second, 50*time.Millisecond,
		"the report never reached the firing session's viewer with its mission-report _meta; transcript=%q",
		viewer.messageText())

	// It did not also fall into the operator inbox — a supervised report has a home.
	inboxItems, err := operatorInbox.List(ctx, 100)
	require.NoError(t, err)
	for _, it := range inboxItems {
		require.NotEqual(t, deliveredSummary, it.Report.Summary,
			"a report delivered to its supervising session must not also fall into the operator inbox")
	}

	// (c) An operator-fired report lands in the inbox, reason operator_fired.
	operatorMission := &missionservice.Mission{
		Intent:         "operator fired this directly",
		AgentName:      "reporter",
		HITLPolicyName: "default",
		// ParentSessionID deliberately empty: no supervising session.
	}
	require.NoError(t, missions.Create(ctx, operatorMission))
	const inboxSummary = "operator-fired result awaiting the inbox"
	require.NoError(t, missions.AddReport(ctx, operatorMission.ID, &missionservice.Report{
		Kind:    missionservice.ReportKindResult,
		Summary: inboxSummary,
	}))
	require.Eventually(t, func() bool {
		items, lerr := operatorInbox.List(ctx, 100)
		if lerr != nil {
			return false
		}
		for _, it := range items {
			if it.Report.Summary == inboxSummary {
				require.Equal(t, operatorinbox.ReasonOperatorFired, it.Reason)
				require.Equal(t, operatorMission.ID, it.MissionID)
				return true
			}
		}
		return false
	}, 30*time.Second, 50*time.Millisecond, "the operator-fired report never landed in the inbox")

	// Teardown: stop both units and prove neither subprocess leaks.
	require.NoError(t, fleet.Stop(ctx, sub.InstanceID))
	_, err = fleet.Get(ctx, sub.InstanceID)
	require.ErrorIs(t, err, agentinstance.ErrNotFound, "the reporter unit is gone after Stop")

	require.NoError(t, fleet.Stop(ctx, firing.InstanceID))
	_, err = fleet.Get(ctx, firing.InstanceID)
	require.ErrorIs(t, err, agentinstance.ErrNotFound, "the firing unit is gone after Stop")
}

// TestFleetE2E_MissionRoundTrip_UnitReportRoutesToSupervisor closes the loop
// the composed acceptance above splits into (a)+(b): the unit's own filed
// report, through mission_report over a real subprocess, routes back into its
// firing session's viewer with no control-published stand-in anywhere. This
// pins the fix in runtime/contenoxcli/acp_cmd.go for a seam gap where a
// dispatched unit's mission tools were wired against a publisher-less mission
// service, so a filed report was stored but never emitted a routing event.
func TestFleetE2E_MissionRoundTrip_UnitReportRoutesToSupervisor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping mission round-trip e2e: builds the contenox binary and spawns real ACP subprocesses")
	}

	bin := mrtBuildContenoxBinary(t)
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

	mrtRunCLI(t, bin, home, "config", "set", "default-model", "mission-roundtrip-fake-model")
	mrtRunCLI(t, bin, home, "config", "set", "update-check", "false")
	mrtRunCLI(t, bin, home, "config", "set", "default-mission-agent", "reporter")
	mrtRunCLI(t, bin, home, "config", "set", "default-mission-policy", "default")

	contenoxDir := filepath.Join(home, ".contenox")
	require.DirExists(t, contenoxDir)

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(contenoxDir, "local.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	agentRegistry := agentregistryservice.New(db)
	bus := libbus.NewSQLite(db.WithoutTransaction())
	t.Cleanup(func() { _ = bus.Close() })
	missions := missionservice.New(db, missionservice.WithEventPublisher(bus))
	operatorInbox := operatorinbox.New(db)

	stderr := &mrtLockedBuffer{}
	kernel := agentinstance.New(agentRegistry,
		agentinstance.WithSelfExecutable(bin),
		agentinstance.WithStderr(stderr),
	)
	t.Cleanup(func() { _ = kernel.Close() })

	router, err := reportrouter.New(reportrouter.Deps{
		Bus:      bus,
		Sessions: kernel,
		Inbox:    operatorInbox,
		Tracker:  libtracker.NoopTracker{},
	})
	require.NoError(t, err)
	stopRouter, err := router.Start(ctx)
	require.NoError(t, err)
	t.Cleanup(stopRouter)

	fleet := fleetservice.New(kernel, agentRegistry, missions, nil, home, libtracker.NoopTracker{})

	mrtWriteChainAgentFixture(t, contenoxDir)
	res, err := chainagents.Discover(ctx, agentRegistry, contenoxDir)
	require.NoError(t, err)
	require.Contains(t, res.Created, "agent-fleet-fixture")

	chainPath := filepath.Join(contenoxDir, "mission-chain.json")
	require.NoError(t, os.WriteFile(chainPath, []byte(mrtMissionReportChain), 0o644))
	reporterAgent := &runtimetypes.Agent{Name: "reporter", Enabled: true}
	require.NoError(t, reporterAgent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   bin,
		Args:      []string{"acp", "--auto"},
		Env:       map[string]string{"CONTENOX_ACP_CHAIN_PATH": chainPath},
	}))
	require.NoError(t, agentRegistry.Create(ctx, reporterAgent))

	firing, err := fleet.Dispatch(ctx, fleetservice.DispatchRequest{
		AgentName:      "agent-fleet-fixture",
		Intent:         "supervise the unit whose own report must come back",
		HITLPolicyName: "default",
	})
	require.NoError(t, err, "firing-session dispatch stderr:\n%s", stderr.String())
	t.Cleanup(func() { _ = fleet.Stop(ctx, firing.InstanceID) })

	viewer := &mrtRecordingViewer{id: "supervisor-observer"}
	_, err = kernel.Attach(ctx, firing.InstanceID, libacp.SessionID(firing.SessionID), viewer)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return strings.Contains(viewer.messageText(), mrtChainReply)
	}, 120*time.Second, 100*time.Millisecond,
		"firing session never came up; transcript=%q\nstderr:\n%s", viewer.messageText(), stderr.String())

	tr := &Transport{deps: Deps{DB: db, Fleet: fleet, Agents: agentRegistry}}
	sess := &sessionEntry{InternalSessionID: firing.SessionID}
	_, args, ok := parseCommand("/mission reporter run the mission and report in")
	require.True(t, ok)
	_, err = tr.handleMission(ctx, sess, args)
	require.NoError(t, err)

	sub := mrtFindReporterSubmission(t, ctx, missions, firing.SessionID)
	t.Cleanup(func() { _ = fleet.Stop(ctx, sub.InstanceID) })

	// The whole point: the unit's own report, filed and published by its own
	// publisher-wired service, arrives in the firing session's stream — no
	// stand-in publish anywhere in this test.
	require.Eventually(t, func() bool {
		return viewer.receivedMissionReport(sub.ID, "unit reporting from the field")
	}, 90*time.Second, 150*time.Millisecond,
		"the unit's own report never routed back to the firing session; transcript=%q\nstderr:\n%s",
		viewer.messageText(), stderr.String())

	// Supervised means not inboxed: the routed report had a home.
	items, err := operatorInbox.List(ctx, 100)
	require.NoError(t, err)
	for _, it := range items {
		require.NotEqual(t, sub.ID, it.MissionID,
			"a routed supervised report must not also fall into the operator inbox")
	}
}

// mrtBuildContenoxBinary compiles cmd/contenox into t.TempDir(); the go build
// cache makes reruns cheap.
func mrtBuildContenoxBinary(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "contenox")
	out, err := exec.Command("go", "build", "-o", binPath, "github.com/contenox/contenox/cmd/contenox").CombinedOutput()
	require.NoErrorf(t, err, "build contenox:\n%s", out)
	return binPath
}

// mrtRunCLI seeds state through the real CLI. Cwd is pinned to home so no
// cwd-walking config resolution can escape into the repo's own .contenox.
func mrtRunCLI(t *testing.T, bin, home string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "contenox %v:\n%s", args, out)
}

// mrtWriteChainAgentFixture writes the deterministic no-model chain under a
// name that declares it as an agent (the agent-*.json convention).
func mrtWriteChainAgentFixture(t *testing.T, contenoxDir string) {
	t.Helper()
	chain := map[string]any{
		"id":          "agent-fleet-fixture",
		"description": "Deterministic no-model chain unit: one noop task whose print is the reply.",
		"tasks": []map[string]any{{
			"id":      "reply",
			"handler": "noop",
			"print":   mrtChainReply,
		}},
	}
	data, err := json.Marshal(chain)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(contenoxDir, "agent-fleet-fixture.json"), data, 0o600))
}

// mrtFindReporterSubmission returns the reporter mission whose parent is the
// firing session; polls since the list read is over the shared store.
func mrtFindReporterSubmission(t *testing.T, ctx context.Context, missions missionservice.Service, firingSessionID string) *missionservice.Mission {
	t.Helper()
	var found *missionservice.Mission
	require.Eventually(t, func() bool {
		ms, err := missions.List(ctx, nil, 100)
		require.NoError(t, err)
		for _, m := range ms {
			if m.AgentName == "reporter" && m.ParentSessionID == firingSessionID {
				found = m
				return true
			}
		}
		return false
	}, 10*time.Second, 50*time.Millisecond, "the /mission handler must have recorded a reporter sub-mission parented on the firing session")
	return found
}

// mrtRecordingViewer records a session's stream. Deliver must not block (kernel
// contract), so it only appends under a mutex.
type mrtRecordingViewer struct {
	id      string
	mu      sync.Mutex
	updates []libacp.SessionNotification
}

func (v *mrtRecordingViewer) ID() string { return v.id }

func (v *mrtRecordingViewer) Deliver(_ context.Context, n libacp.SessionNotification) error {
	v.mu.Lock()
	v.updates = append(v.updates, n)
	v.mu.Unlock()
	return nil
}

func (v *mrtRecordingViewer) RequestPermission(_ context.Context, _ libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	return libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeCancelled},
	}, nil
}

// messageText concatenates the text of every agent_message_chunk observed.
func (v *mrtRecordingViewer) messageText() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var sb strings.Builder
	for _, n := range v.updates {
		if n.Update.SessionUpdate != libacp.SessionUpdateAgentMessageChunk {
			continue
		}
		if c := n.Update.Content; c != nil && c.Type == string(libacp.ContentKindText) {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// mrtReportMeta is the _meta envelope the router stamps on a delivered report
// (reportrouter.reportUpdateMeta's wire shape).
type mrtReportMeta struct {
	Report *struct {
		MissionID string `json:"missionId"`
	} `json:"contenox.missionReport"`
}

// receivedMissionReport reports whether a delivered agent_message_chunk
// carries both summary and the mission-report _meta attribution for missionID.
func (v *mrtRecordingViewer) receivedMissionReport(missionID, summary string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, n := range v.updates {
		if n.Update.SessionUpdate != libacp.SessionUpdateAgentMessageChunk {
			continue
		}
		if len(n.Update.Meta) == 0 {
			continue
		}
		var meta mrtReportMeta
		if json.Unmarshal(n.Update.Meta, &meta) != nil || meta.Report == nil {
			continue
		}
		if meta.Report.MissionID != missionID {
			continue
		}
		if c := n.Update.Content; c != nil && strings.Contains(c.Text, summary) {
			return true
		}
	}
	return false
}

// mrtLockedBuffer is a concurrency-safe sink for a spawned unit's stderr, so a
// failure message can quote it without racing the subprocess's writer.
type mrtLockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *mrtLockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *mrtLockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
