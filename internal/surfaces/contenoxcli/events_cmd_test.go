package contenoxcli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/eventlog"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestUnit_eventsIsReservedSubcommand(t *testing.T) {
	require.True(t, reservedSubcommands["events"], `"events" must be reserved so it dispatches as a subcommand, not chat input`)
	require.True(t, firstNonFlagIsReserved([]string{"events", "dispatch"}))
}

// TestUnit_EventsCmd_HiddenWithoutBeta pins the gating Main applies: without
// opt-in-beta the events verb is Hidden (absent from help), exactly like the
// agent roster; execution is never refused by the gate.
func TestUnit_EventsCmd_HiddenWithoutBeta(t *testing.T) {
	prev := eventsCmd.Hidden
	defer func() { eventsCmd.Hidden = prev }()

	t.Setenv(envOptInBeta, "0")
	eventsCmd.Hidden = !betaEnabledGlobal()
	require.True(t, eventsCmd.Hidden, "events must be invisible for stable users")

	t.Setenv(envOptInBeta, "1")
	eventsCmd.Hidden = !betaEnabledGlobal()
	require.False(t, eventsCmd.Hidden, "opt-in-beta reveals the verb")
}

// TestUnit_EventsFiringsCmd_RidesTheEventsBetaGate pins that the firings verb
// invents no gate of its own: it hangs off the events group, so the group's
// beta Hidden is what keeps it out of help.
func TestUnit_EventsFiringsCmd_RidesTheEventsBetaGate(t *testing.T) {
	prev := eventsCmd.Hidden
	defer func() { eventsCmd.Hidden = prev }()

	require.Same(t, eventsCmd, eventsFiringsCmd.Parent(), "firings must be registered on the events group")
	require.False(t, eventsFiringsCmd.Hidden, "the verb carries no separate gate")

	t.Setenv(envOptInBeta, "0")
	eventsCmd.Hidden = !betaEnabledGlobal()
	require.True(t, eventsCmd.Hidden, "events firings must be invisible for stable users")
	require.False(t, eventsCmd.IsAvailableCommand(), "a hidden group keeps firings out of help")

	t.Setenv(envOptInBeta, "1")
	eventsCmd.Hidden = !betaEnabledGlobal()
	require.True(t, eventsCmd.IsAvailableCommand(), "opt-in-beta reveals the verb")
}

// firingsTestCmd builds a cobra command wired to dbPath with the firings
// flag set, and a temp data-dir so the workspace resolves to the default.
func firingsTestCmd(t *testing.T, dbPath string) *cobra.Command {
	t.Helper()
	cmd := testCobraCmd()
	require.NoError(t, cmd.Root().PersistentFlags().Set("db", dbPath))
	require.NoError(t, cmd.Root().PersistentFlags().Set("data-dir", t.TempDir()))
	cmd.Flags().Int64("since", 0, "")
	cmd.Flags().String("status", "", "")
	cmd.Flags().String("trigger", "", "")
	cmd.Flags().Int("limit", 50, "")
	return cmd
}

// runFirings executes the verb and returns its stdout.
func runFirings(t *testing.T, cmd *cobra.Command) string {
	t.Helper()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	require.NoError(t, runEventsFirings(cmd, nil))
	return out.String()
}

// TestUnit_EventsFirings_Roundtrip drives the verb against a real database:
// newest-first table, each filter narrowing it, and an unknown status refused
// rather than silently answered with nothing.
func TestUnit_EventsFirings_Roundtrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "firings-cli.db")
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	store := mustFiringStore(t, db.WithoutTransaction(), DefaultWorkspaceID)
	for _, f := range []struct {
		trigger string
		nid     int64
		status  string
		errMsg  string
	}{
		{"on-report", 1, runtimetypes.EventFiringStatusOK, ""},
		{"on-status", 2, runtimetypes.EventFiringStatusError, "chain\nblew up"},
		{"on-report", 3, runtimetypes.EventFiringStatusRefused, "hop 5 exceeds limit 4"},
	} {
		claimed, err := store.BeginEventFiring(ctx, f.trigger, f.nid, "evt-cli")
		require.NoError(t, err)
		require.True(t, claimed)
		require.NoError(t, store.FinishEventFiring(ctx, f.trigger, f.nid, f.status, f.errMsg))
	}
	require.NoError(t, db.Close())

	all := runFirings(t, firingsTestCmd(t, dbPath))
	for _, column := range []string{"NID", "TRIGGER", "STATUS", "REQUEST", "TIME", "ERROR"} {
		require.Contains(t, all, column, "the header is the events-list register")
	}
	require.Less(t, strings.Index(all, "refused"), strings.Index(all, "error"), "newest first: nid 3 before nid 2")
	require.Contains(t, all, "on-report")
	require.Contains(t, all, "evt-cli")
	require.Contains(t, all, "chain blew up", "a multi-line chain error is collapsed onto its row")

	byStatus := firingsTestCmd(t, dbPath)
	require.NoError(t, byStatus.Flags().Set("status", "refused"))
	refused := runFirings(t, byStatus)
	require.Contains(t, refused, "hop 5 exceeds limit 4")
	require.NotContains(t, refused, "on-status")

	byTrigger := firingsTestCmd(t, dbPath)
	require.NoError(t, byTrigger.Flags().Set("trigger", "on-status"))
	oneTrigger := runFirings(t, byTrigger)
	require.Contains(t, oneTrigger, "on-status")
	require.NotContains(t, oneTrigger, "on-report")

	bySince := firingsTestCmd(t, dbPath)
	require.NoError(t, bySince.Flags().Set("since", "2"))
	since := runFirings(t, bySince)
	require.Contains(t, since, "refused")
	require.NotContains(t, since, "on-status", "--since is strictly greater than the cursor")

	bad := firingsTestCmd(t, dbPath)
	require.NoError(t, bad.Flags().Set("status", "borked"))
	err = runEventsFirings(bad, nil)
	require.ErrorContains(t, err, `unknown --status "borked"`)
	require.ErrorContains(t, err, "ok, error, refused, running")
}

// TestUnit_EventsFirings_EmptyIsEmptyOutputNotAnError pins that nothing to
// show is an answer: the empty marker on stdout and a nil error, exactly like
// 'events list' with an empty log.
func TestUnit_EventsFirings_EmptyIsEmptyOutputNotAnError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "empty-firings.db")
	db, err := libdb.NewSQLiteDBManager(context.Background(), dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	require.Equal(t, "(no firings)\n", runFirings(t, firingsTestCmd(t, dbPath)))

	filtered := firingsTestCmd(t, dbPath)
	require.NoError(t, filtered.Flags().Set("trigger", "typo-in-listen-for"))
	require.Equal(t, "(no firings)\n", runFirings(t, filtered))
}

// TestUnit_PrintFiringTrouble_SilentUntilSomethingBroke pins doctor's line:
// nothing on a clean or empty window, one compact count when firings failed.
func TestUnit_PrintFiringTrouble_SilentUntilSomethingBroke(t *testing.T) {
	ctx := context.Background()
	db := setupEventsTestDB(t)
	exec := db.WithoutTransaction()
	store := mustFiringStore(t, exec, DefaultWorkspaceID)

	var quiet bytes.Buffer
	printFiringTrouble(ctx, &quiet, exec, DefaultWorkspaceID)
	require.Empty(t, quiet.String(), "no firings at all says nothing")

	claimed, err := store.BeginEventFiring(ctx, "on-report", 1, "evt-1")
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, store.FinishEventFiring(ctx, "on-report", 1, runtimetypes.EventFiringStatusOK, ""))
	quiet.Reset()
	printFiringTrouble(ctx, &quiet, exec, DefaultWorkspaceID)
	require.Empty(t, quiet.String(), "a clean window says nothing")

	for nid, status := range map[int64]string{2: runtimetypes.EventFiringStatusError, 3: runtimetypes.EventFiringStatusRefused} {
		claimed, err := store.BeginEventFiring(ctx, "on-report", nid, "evt-x")
		require.NoError(t, err)
		require.True(t, claimed)
		require.NoError(t, store.FinishEventFiring(ctx, "on-report", nid, status, "nope"))
	}
	var loud bytes.Buffer
	printFiringTrouble(ctx, &loud, exec, DefaultWorkspaceID)
	require.Contains(t, loud.String(), "2 of the last 3 ended in error/refused")
	// The pointer dropped `--status error` when stranded firings joined the
	// count: a stranded row is still `running`, so the filtered view would
	// hide exactly the failure the line just reported.
	require.Contains(t, loud.String(), "contenox events firings")

	var other bytes.Buffer
	printFiringTrouble(ctx, &other, exec, "ws-elsewhere")
	require.Empty(t, other.String(), "another workspace's failures are not this workspace's")
}

func setupEventsTestDB(t *testing.T) libdb.DBManager {
	t.Helper()
	db, err := libdb.NewSQLiteDBManager(context.Background(), filepath.Join(t.TempDir(), "test.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

// TestUnit_MissionEventPublisher_BetaGatesDualWrite pins that beta off means
// the exact pre-event-log path — the bus itself, no wrapper, no rows — while
// beta on returns the dual-writing publisher.
func TestUnit_MissionEventPublisher_BetaGatesDualWrite(t *testing.T) {
	ctx := context.Background()
	db := setupEventsTestDB(t)
	bus := libbus.NewInMem()
	t.Cleanup(func() { _ = bus.Close() })

	t.Setenv(envOptInBeta, "0")
	var asPublisher missionservice.EventPublisher = bus
	require.Equal(t, asPublisher, missionEventPublisher(ctx, db, bus, DefaultWorkspaceID, nil, nil), "beta off returns the bus unchanged")

	t.Setenv(envOptInBeta, "1")
	pub := missionEventPublisher(ctx, db, bus, DefaultWorkspaceID, nil, nil)
	require.IsType(t, &eventlog.DualPublisher{}, pub)

	require.NoError(t, pub.Publish(ctx, missionservice.ReportAddedSubject, []byte(`{"missionId":"m-9"}`)))
	rows, err := runtimetypes.NewEventStore(db).ListEventsSince(ctx, DefaultWorkspaceID, 0, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, missionservice.ReportAddedSubject, rows[0].Type)
	require.Equal(t, "m-9", rows[0].Subject)
	require.Equal(t, DefaultWorkspaceID, rows[0].WorkspaceID)
}

// TestSystem_MissionService_DualWritesReportAddedToLogAndBus drives the real
// producer: AddReport under the dual-write publisher must reach existing bus
// subscribers with the unchanged ReportAddedEvent payload AND leave a durable
// event_log row.
func TestSystem_MissionService_DualWritesReportAddedToLogAndBus(t *testing.T) {
	ctx := libtracker.WithNewRequestID(context.Background())
	db := setupEventsTestDB(t)
	bus := libbus.NewSQLiteWithOptions(db.WithoutTransaction(), libbus.SQLiteBusOptions{EventPoll: time.Millisecond, RequestPoll: time.Millisecond})
	t.Cleanup(func() { _ = bus.Close() })

	ch := make(chan []byte, 4)
	sub, err := bus.Stream(ctx, missionservice.ReportAddedSubject, ch)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	t.Setenv(envOptInBeta, "1")
	missions := missionservice.New(db, missionservice.WithEventPublisher(missionEventPublisher(ctx, db, bus, DefaultWorkspaceID, nil, nil)))

	mission := &missionservice.Mission{Intent: "test the dual write", AgentName: "tester", HITLPolicyName: "hitl-policy-default.json"}
	require.NoError(t, missions.Create(ctx, mission))
	require.NoError(t, missions.AddReport(ctx, mission.ID, &missionservice.Report{
		Kind:    missionservice.ReportKindProgress,
		Summary: "half way",
	}))

	select {
	case payload := <-ch:
		var ev missionservice.ReportAddedEvent
		require.NoError(t, json.Unmarshal(payload, &ev), "bus subscribers still get the raw domain payload")
		require.Equal(t, mission.ID, ev.MissionID)
	case <-time.After(5 * time.Second):
		t.Fatal("existing bus subscriber saw no publish")
	}

	rows, err := runtimetypes.NewEventStore(db).ListEventsSince(ctx, DefaultWorkspaceID, 0, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1, "the same event is durable in the log")
	require.Equal(t, missionservice.ReportAddedSubject, rows[0].Type)
	require.Equal(t, "missionservice", rows[0].Source)
	require.Equal(t, DefaultWorkspaceID, rows[0].WorkspaceID, "the producer's resolved workspace is stamped on the row")
	require.Equal(t, mission.ID, rows[0].Subject)
	var logged missionservice.ReportAddedEvent
	require.NoError(t, json.Unmarshal(rows[0].Data, &logged))
	require.Equal(t, "half way", logged.Report.Summary)
}

// TestUnit_Vet_TriggerFilesAreBetaGated pins vet's stable behavior: without
// the opt-in a trigger file stays in the skip class exactly as before; with
// it, shape and reference defects fail vet.
func TestUnit_Vet_TriggerFilesAreBetaGated(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "trigger-bad.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{
		"name": "bad",
		"listen_for": {"type": "x"},
		"type": "not_a_thing",
		"chain": "c.json"
	}`), 0o600))

	var stable strings.Builder
	require.Equal(t, 0, runVetOnFiles(&stable, []string{bad}, vetOpts{}))
	require.Contains(t, stable.String(), "skip "+bad, "stable vet must not change for trigger files")

	var beta strings.Builder
	require.Equal(t, 1, runVetOnFiles(&beta, []string{bad}, vetOpts{triggers: true, contenoxDir: dir}))
	require.Contains(t, beta.String(), "FAIL "+bad)
	require.Contains(t, beta.String(), `unknown type "not_a_thing"`)
}

// TestUnit_Vet_TriggerReferencesResolveOnSystemPath pins that a trigger's
// chain must exist in the workspace/home resolution path and a named policy
// must too.
func TestUnit_Vet_TriggerReferencesResolveOnSystemPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "chain-here.json"), []byte(`{"id":"c","tasks":[]}`), 0o600))

	good := filepath.Join(workspace, "trigger-good.json")
	require.NoError(t, os.WriteFile(good, []byte(`{
		"name": "good",
		"listen_for": {"type": "missionservice.events.report_added"},
		"type": "fire_chain",
		"chain": "chain-here.json"
	}`), 0o600))
	dangling := filepath.Join(workspace, "trigger-dangling.json")
	require.NoError(t, os.WriteFile(dangling, []byte(`{
		"name": "dangling",
		"listen_for": {"type": "missionservice.events.report_added"},
		"type": "fire_chain",
		"chain": "chain-nowhere.json"
	}`), 0o600))

	vo := vetOpts{triggers: true, contenoxDir: workspace}
	var out strings.Builder
	require.Equal(t, 1, runVetOnFiles(&out, []string{good, dangling}, vo))
	require.Contains(t, out.String(), "ok   "+good)
	require.Contains(t, out.String(), "FAIL "+dangling)
	require.Contains(t, out.String(), `chain "chain-nowhere.json"`)
}

// TestUnit_TriggerShadowNames_BetaOnly pins doctor's shadow-set extension:
// nothing without the opt-in, the trigger basenames with it.
func TestUnit_TriggerShadowNames_BetaOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	homeContenox := filepath.Join(home, ".contenox")
	require.NoError(t, os.MkdirAll(homeContenox, 0o750))
	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "trigger-ws.json"), []byte(`{}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(homeContenox, "trigger-home.json"), []byte(`{}`), 0o600))

	require.Nil(t, triggerShadowNames(false, workspace), "doctor stays silent about triggers for stable users")
	require.Equal(t, []string{"trigger-home.json", "trigger-ws.json"}, triggerShadowNames(true, workspace))
}

// TestUnit_LoadTriggersKept_BetaOff pins the discovery gate: without the
// opt-in nothing loads, no matter what trigger files exist.
func TestUnit_LoadTriggersKept_BetaOff(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "trigger-x.json"), []byte(`{
		"name": "x",
		"listen_for": {"type": "t"},
		"type": "fire_chain",
		"chain": "c.json"
	}`), 0o600))

	off, err := loadTriggersKept(context.Background(), nil, workspace, false)
	require.NoError(t, err)
	require.Empty(t, off.Triggers)

	on, err := loadTriggersKept(context.Background(), nil, workspace, true)
	require.NoError(t, err)
	require.Len(t, on.Triggers, 1)
}
