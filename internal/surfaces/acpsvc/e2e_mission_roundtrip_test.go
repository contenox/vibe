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

	"github.com/contenox/beam/internal/kernel/agentinstance"
	libbus "github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/chainagents"
	"github.com/contenox/beam/internal/services/fleetservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/operatorinbox"
	"github.com/contenox/beam/internal/services/reportrouter"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// This file is the composed ACCEPTANCE for fleet mission-mode — the single test
// that stands where the three slices meet and proves the flagship loop end to
// end through the SAME components `contenox serve` wires, nothing mocked below
// the service layer:
//
//	a live session fires `/mission`  →  fleetservice dispatches a REAL unit that
//	runs unattended and files a report through its granted mission_report tool  →
//	the report is a durable, mission-scoped fact  →  the report-router delivers a
//	report on that supervision edge back into the FIRING session's own stream
//	(and to the operator inbox when an operator fired directly).
//
// Each existing e2e proves one seam of this in isolation:
//
//   - e2e_mission_report_test.go (mission-tools): a dispatched `contenox acp`
//     subprocess files a report through the mission tool into the shared store.
//   - e2e_report_routing_test.go (report-routing): a report ADDED through the
//     publisher-wired mission service routes to a live parent session's viewer
//     or to the operator inbox.
//   - e2e_chain_dispatch_test.go (chains-as-agents): the house idiom — a real
//     binary, HOME isolated to t.TempDir(), a deterministic no-model chain, a
//     fake default-model so any accidental resolution fails loudly.
//
// What no isolated slice can show — and what an acceptance MUST — is that these
// halves COMPOSE across the process boundary a fleet unit really is. That
// composition is the whole point of mission mode, and it is exactly where a
// walking skeleton either stands up or falls over.
//
// # Why the firing session is a KERNEL-hosted unit, not a synthetic id
//
// The router delivers via agentinstance.Manager.DeliverToSession, which reaches
// only sessions a live kernel instance OWNS. So a firing session that a report
// can actually be routed BACK into has to be a real kernel session with a viewer
// attached THROUGH the kernel — a coordinating unit's session, precisely the
// shape e2e_report_routing_test.go used for its supervising parent. We bring one
// up as a chain unit, attach a recording viewer, and thread ITS downstream
// session id into the real `/mission` handler as the firing session. That is
// what makes DeliverToSession(ParentSessionID) resolve to a viewer we can assert
// against, rather than fall to the inbox as parent-gone.
//
// # Why the delivered report on the edge is added through the mission service
//
// Assertion (a) below runs the WHOLE unit path: the real `/mission` handler
// dispatches a real reporter subprocess that files a real report through its
// real mission_report tool, and we read that durable report back — the mission-
// tools slice composed with the `/mission` command slice, unmocked. The DELIVERY
// assertion (b) then drives a report on the SAME real sub-mission — carrying the
// SAME supervision edge the `/mission` handler recorded — through the publisher-
// wired mission service (the exact path serve's REST report-add takes) and proves
// it lands in the firing viewer's stream with the `contenox.missionReport` _meta.
// Splitting it this way preserves the history of a real seam gap this test
// SURFACED on first composition (a publisher-less mission service in the unit
// process — since fixed in acp_cmd.go): (b) proves the delivery half over the
// serve-side report-add path, while the sibling test
// TestFleetE2E_MissionRoundTrip_UnitReportRoutesToSupervisor proves the unit's
// OWN filed report routes end to end and keeps that seam closed.
//
// HOME is isolated to a per-test temp dir, and BOTH the units and this test's DB
// handle resolve to the one $HOME/.contenox/local.db — that shared file is how a
// unit's report row is visible here (assertion a) and how the SQLite event bus
// (durable, cross-process) carries a routing event from a publisher over the same
// file to the router polling it. The default-model is a name that resolves to no
// backend: the chains are model-free by construction, so a resolution would be a
// bug, and it must fail loudly rather than reach out to a real model.

// mrtChainReply is the byte-exact reply the firing session's fixture chain
// streams — one noop task whose `print` is the whole answer, no model involved.
// It is only used to confirm the firing unit is fully up before we fire at it.
const mrtChainReply = "contenox mission roundtrip firing-session fixture reply"

// mrtMissionReportChain is the deterministic, model-free chain the dispatched
// REPORTER unit runs as its first and only turn: a `tools` task that calls its
// granted mission_report tool with static args, then a noop terminator. It never
// touches a model — the fake default-model would fail loudly if it tried —
// proving the report path, not inference. Copied from the mission-tools slice's
// e2e (e2e_mission_report_test.go) so the two acceptances file identical reports.
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

	// Redirect the spawned units' state to an isolated HOME and neutralize every
	// ambient CONTENOX_* override so a value in the developer's shell can neither
	// redirect a unit's boot nor its chain. Empty reads as unset, falling through
	// to the seeded DB configuration — and a unit answering at all is then proof it
	// inherited THIS process's environment, the point of a same-HOME unit.
	t.Setenv("HOME", home)
	for _, k := range []string{
		"CONTENOX_DEFAULT_MODEL", "CONTENOX_DEFAULT_PROVIDER",
		"CONTENOX_DEFAULT_ALT_MODEL", "CONTENOX_DEFAULT_ALT_PROVIDER",
		"CONTENOX_DEFAULT_MAX_TOKENS", "CONTENOX_DEFAULT_THINK",
		"CONTENOX_ACP_CHAIN_PATH",
	} {
		t.Setenv(k, "")
	}

	// Seed the isolated state through the REAL CLI, the same surface an operator
	// configures with. The default-model name is deliberately fake (no backend);
	// update-check=false keeps startup off the network. default-mission-agent and
	// default-mission-policy are what the `/mission` handler resolves the fired
	// mission's agent and envelope from.
	mrtRunCLI(t, bin, home, "config", "set", "default-model", "mission-roundtrip-fake-model")
	mrtRunCLI(t, bin, home, "config", "set", "update-check", "false")
	mrtRunCLI(t, bin, home, "config", "set", "default-mission-agent", "reporter")
	mrtRunCLI(t, bin, home, "config", "set", "default-mission-policy", "default")

	contenoxDir := filepath.Join(home, ".contenox")
	require.DirExists(t, contenoxDir, "the CLI seeding run must have created the isolated state directory")

	// The one shared store: the units resolve $HOME/.contenox/local.db, and so does
	// this handle. A unit's report row is visible here through it, and the SQLite
	// bus below rides the same file cross-process.
	dbPath := filepath.Join(contenoxDir, "local.db")
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// ── The composed stack, wired the way serve_cmd.go wires it ────────────────
	agentRegistry := agentregistryservice.New(db)

	// The bus is the supervision edge's producer transport; the publisher-wired
	// mission service publishes ReportAddedEvent on AddReport. SQLite-backed so the
	// event survives the process boundary a fleet unit is.
	bus := libbus.NewSQLite(db.WithoutTransaction())
	t.Cleanup(func() { _ = bus.Close() })
	missions := missionservice.New(db, missionservice.WithEventPublisher(bus))
	operatorInbox := operatorinbox.New(db)

	// The kernel: WithSelfExecutable points a CHAIN unit's re-exec at the freshly
	// built binary (under `go test` os.Executable() is the test binary, which
	// serves no ACP). Stderr is captured so a subprocess failure can be quoted.
	stderr := &mrtLockedBuffer{}
	kernel := agentinstance.New(agentRegistry,
		agentinstance.WithSelfExecutable(bin),
		agentinstance.WithStderr(stderr),
	)
	t.Cleanup(func() { _ = kernel.Close() })

	// The report router, wired exactly as serve wires it: the kernel is the
	// SessionDeliverer, the inbox is the fallback.
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

	// ── Declare the two agents ─────────────────────────────────────────────────

	// The firing/supervising session is a chain unit declared BY CONVENTION (the
	// agent-*.json filename is the whole declaration), discovered the way serve
	// discovers operator chains.
	mrtWriteChainAgentFixture(t, contenoxDir)
	res, err := chainagents.Discover(ctx, agentRegistry, contenoxDir)
	require.NoError(t, err)
	require.Contains(t, res.Created, "agent-fleet-fixture", "the agent-*.json file must declare the chain agent")

	// The reporter is the fired unit: a `contenox acp --auto` subprocess (auto = no
	// HITL, so its mission_report tool runs unattended) bound to the deterministic
	// mission chain and sharing this HOME/DB.
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

	// ── Bring up the FIRING session as a live kernel unit ──────────────────────

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

	// Wait until the firing unit has fully booted and answered, so its startup
	// seeding does not overlap the reporter's below (two full runtimes seeding one
	// SQLite file concurrently is the contention the report-routing slice flagged;
	// serializing their bring-ups avoids it).
	require.Eventually(t, func() bool {
		return strings.Contains(viewer.messageText(), mrtChainReply)
	}, 120*time.Second, 100*time.Millisecond,
		"firing session never came up; transcript=%q\nstderr:\n%s", viewer.messageText(), stderr.String())

	// ── Fire the REAL /mission command from the firing session ─────────────────

	// A genuine transport with the SAME collaborators serve gives it: the fleet to
	// dispatch through and the registry to resolve the agent name against. conn is
	// nil, so command output updates are dropped harmlessly (sendUpdate no-ops) —
	// the handler's real work (resolve, dispatch, record the edge) is what runs.
	tr := &Transport{deps: Deps{DB: db, Fleet: fleet, Agents: agentRegistry}}
	sess := &sessionEntry{InternalSessionID: firing.SessionID}

	// The parse+dispatch path recognizes and routes /mission (not forwarded as
	// prompt text), then the handler runs — exercising the real code path.
	name, args, ok := parseCommand("/mission reporter run the mission and report in")
	require.True(t, ok, "/mission must be recognized as a command")
	require.Equal(t, "mission", name)

	out, err := tr.handleMission(ctx, sess, args)
	require.NoError(t, err, "the /mission handler must fire successfully")
	require.Contains(t, out, "reporter", "the confirmation names the fired agent")

	// The `/mission` handler recorded the supervision edge from the firing session:
	// find the sub-mission it dispatched (the reporter mission parented on the
	// firing session).
	sub := mrtFindReporterSubmission(t, ctx, missions, firing.SessionID)
	require.Equal(t, firing.SessionID, sub.ParentSessionID,
		"the fired mission's parent is the firing session — the supervision edge /mission records")
	require.NotEmpty(t, sub.InstanceID, "the fired mission is bound to its unit's instance")
	t.Cleanup(func() { _ = fleet.Stop(ctx, sub.InstanceID) })

	// ── (a) The reporter unit files a real report through its mission tool ──────

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

	// ── (b) A report on that edge routes back into the FIRING session's stream ──

	// Driven through the publisher-wired mission service (serve's REST report path)
	// because the unit's own filed report currently emits no routing event — the
	// documented seam gap. The edge, the sub-mission, and the firing session are
	// all the real ones the loop produced.
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

	// It did NOT also fall into the operator inbox — a supervised report has a home.
	inboxItems, err := operatorInbox.List(ctx, 100)
	require.NoError(t, err)
	for _, it := range inboxItems {
		require.NotEqual(t, deliveredSummary, it.Report.Summary,
			"a report delivered to its supervising session must not also fall into the operator inbox")
	}

	// ── (c) An operator-fired report lands in the inbox, reason operator_fired ──

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

	// ── Teardown: stop both units and prove neither subprocess leaks ────────────

	require.NoError(t, fleet.Stop(ctx, sub.InstanceID))
	_, err = fleet.Get(ctx, sub.InstanceID)
	require.ErrorIs(t, err, agentinstance.ErrNotFound, "the reporter unit is gone after Stop")

	require.NoError(t, fleet.Stop(ctx, firing.InstanceID))
	_, err = fleet.Get(ctx, firing.InstanceID)
	require.ErrorIs(t, err, agentinstance.ErrNotFound, "the firing unit is gone after Stop")
}

// TestFleetE2E_MissionRoundTrip_UnitReportRoutesToSupervisor closes the loop the
// composed acceptance above splits into (a)+(b): the unit's OWN filed report —
// through mission_report, over a REAL subprocess — routes back into its firing
// session's viewer, with no control-published stand-in anywhere.
//
// History, kept because this test exists BECAUSE of it: the composed e2e
// surfaced a seam gap neither slice's isolated acceptance could see — `contenox
// acp` wired the dispatched unit's mission tools against a PUBLISHER-LESS
// mission service, so a unit-filed report was stored durably but emitted no
// ReportAddedEvent, and the router (which routes purely off that bus event)
// never delivered it. The two slices did not compose across the process boundary
// a fleet unit is. The fix is the publisher wiring in
// runtime/contenoxcli/acp_cmd.go (see the comment there); THIS test is what
// keeps that seam closed — if the publisher wiring regresses, this is the test
// that goes red.
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

	// The whole point: the UNIT's own report — filed by the subprocess through
	// its mission_report tool, published by ITS OWN publisher-wired service over
	// the shared SQLite bus — arrives in the firing session's stream, attributed
	// to its mission. No stand-in publish anywhere in this test.
	require.Eventually(t, func() bool {
		return viewer.receivedMissionReport(sub.ID, "unit reporting from the field")
	}, 90*time.Second, 150*time.Millisecond,
		"the unit's own report never routed back to the firing session; transcript=%q\nstderr:\n%s",
		viewer.messageText(), stderr.String())

	// Supervised means NOT inboxed: the routed report had a home.
	items, err := operatorInbox.List(ctx, 100)
	require.NoError(t, err)
	for _, it := range items {
		require.NotEqual(t, sub.ID, it.MissionID,
			"a routed supervised report must not also fall into the operator inbox")
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

// mrtBuildContenoxBinary compiles cmd/contenox into t.TempDir(); the go build
// cache makes reruns cheap.
func mrtBuildContenoxBinary(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "contenox")
	out, err := exec.Command("go", "build", "-o", binPath, "github.com/contenox/beam/cmd/contenox").CombinedOutput()
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

// mrtWriteChainAgentFixture writes the deterministic no-model chain under a name
// that DECLARES it as an agent (the agent-*.json convention). One leaf noop task
// whose print is the entire streamed reply.
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

// mrtFindReporterSubmission returns the sub-mission the /mission handler
// dispatched: the reporter mission whose parent is the firing session. Polls
// briefly because Dispatch records the mission synchronously before returning,
// but the list read is over the shared store.
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

// receivedMissionReport reports whether an agent_message_chunk was delivered
// carrying both the summary text and the mission-report _meta attribution for
// missionID — i.e. the routed report the firing session must see, recognizable as
// a mission report rather than an ordinary agent message.
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
