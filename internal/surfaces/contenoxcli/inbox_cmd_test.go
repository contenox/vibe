package contenoxcli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/operatorinbox"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func TestUnit_InboxCommandIsReserved(t *testing.T) {
	require.True(t, reservedSubcommands["inbox"], `"inbox" must be reserved so it dispatches as a subcommand, not chat input`)
}

// ─── rendering ──────────────────────────────────────────────────────────────

func TestUnit_RenderInboxTable_Empty(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, renderInboxTable(&buf, nil, false, time.Now().UTC()))
	require.Contains(t, buf.String(), "No unacknowledged inbox items")
	require.Contains(t, buf.String(), "--all")

	var allBuf bytes.Buffer
	require.NoError(t, renderInboxTable(&allBuf, nil, true, time.Now().UTC()))
	require.Contains(t, allBuf.String(), "Operator inbox is empty")
}

func TestUnit_RenderInboxTable_Rows(t *testing.T) {
	now := time.Now().UTC()
	items := []*operatorinbox.Item{
		{
			ID: "inbox-1", MissionID: "m-1", AgentName: "agent-a", Reason: operatorinbox.ReasonOperatorFired,
			Report:    missionservice.Report{Kind: missionservice.ReportKindResult, Summary: "landed cleanly"},
			CreatedAt: now.Add(-5 * time.Minute), Acked: false,
		},
		{
			ID: "inbox-2", MissionID: "m-2", Reason: operatorinbox.ReasonParentGone, ParentSessionID: "sess-gone",
			Report:    missionservice.Report{Kind: missionservice.ReportKindBlocker, Summary: "stuck on auth"},
			CreatedAt: now.Add(-2 * time.Hour), Acked: true,
		},
	}
	var buf bytes.Buffer
	require.NoError(t, renderInboxTable(&buf, items, true, now))
	out := buf.String()
	for _, want := range []string{
		"ID", "REASON", "MISSION", "KIND", "SUMMARY", "AGE", "ACKED",
		"inbox-1", "m-1", "result", "landed cleanly", "5m", "no",
		"inbox-2", "m-2", "blocker", "stuck on auth", "2h", "yes",
		"operator-fired", "parent session gone",
		"inbox show", "inbox ack",
	} {
		require.Contains(t, out, want)
	}
}

func TestUnit_RenderInboxItem_FullDetailAndHandover(t *testing.T) {
	now := time.Now().UTC()
	item := &operatorinbox.Item{
		ID: "inbox-3", MissionID: "m-3", AgentName: "agent-b", Intent: "ship the feature",
		Reason: operatorinbox.ReasonParentGone, ParentSessionID: "sess-9",
		Report: missionservice.Report{
			Kind: missionservice.ReportKindResult, Summary: "shipped", Detail: "full detail here",
			Refs: []string{"/tmp/out.txt"},
			Handover: &missionservice.Handover{
				Outcome: "done", Artifacts: []string{"a.txt"}, HandoverForNext: "pick up here", Caveats: "unverified",
			},
		},
		CreatedAt: now.Add(-3 * time.Hour),
	}
	var buf bytes.Buffer
	renderInboxItem(&buf, item, now)
	out := buf.String()
	for _, want := range []string{
		"inbox-3", "m-3", "agent-b", "ship the feature",
		"parent session gone", "sess-9",
		"shipped", "full detail here", "/tmp/out.txt",
		"Outcome:", "done", "a.txt", "pick up here", "unverified",
		"mission show m-3",
	} {
		require.Contains(t, out, want)
	}
}

func TestUnit_InboxReasonLabel(t *testing.T) {
	require.Equal(t, "operator-fired (no session)", inboxReasonLabel(operatorinbox.ReasonOperatorFired))
	require.Equal(t, "parent session gone", inboxReasonLabel(operatorinbox.ReasonParentGone))
}

// ─── CLI round trip against a real SQLite db ───────────────────────────────

func TestUnit_InboxListShowAckCmd_Roundtrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "inbox-cli.db")
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	inbox := operatorinbox.New(db)
	item := &operatorinbox.Item{
		MissionID: "m-cli", AgentName: "agent-cli", Reason: operatorinbox.ReasonOperatorFired,
		Report: missionservice.Report{Kind: missionservice.ReportKindResult, Summary: "cli roundtrip"},
	}
	require.NoError(t, inbox.Add(ctx, item))
	require.NoError(t, db.Close())

	cmd := testCobraCmd()
	require.NoError(t, cmd.Root().PersistentFlags().Set("db", dbPath))
	cmd.Flags().Int("limit", 50, "")
	cmd.Flags().Bool("all", false, "")

	// list (unacked only, default) — the item is there.
	listOut := &bytes.Buffer{}
	cmd.SetOut(listOut)
	require.NoError(t, runInboxList(cmd, nil))
	require.Contains(t, listOut.String(), item.ID)
	require.Contains(t, listOut.String(), "cli roundtrip")

	// show — full detail.
	showOut := &bytes.Buffer{}
	cmd.SetOut(showOut)
	require.NoError(t, runInboxShow(cmd, []string{item.ID}))
	require.Contains(t, showOut.String(), "m-cli")
	require.Contains(t, showOut.String(), "cli roundtrip")

	// ack — succeeds and confirms.
	ackOut := &bytes.Buffer{}
	cmd.SetOut(ackOut)
	require.NoError(t, runInboxAck(cmd, []string{item.ID}))
	require.Contains(t, ackOut.String(), item.ID)
	require.Contains(t, ackOut.String(), "acknowledged")

	// list (unacked only) now excludes it; list --all still includes it, acked.
	unackedOut := &bytes.Buffer{}
	cmd.SetOut(unackedOut)
	require.NoError(t, runInboxList(cmd, nil))
	require.NotContains(t, unackedOut.String(), item.ID, "an acked item must not appear in the default unacked-only view")

	require.NoError(t, cmd.Flags().Set("all", "true"))
	allOut := &bytes.Buffer{}
	cmd.SetOut(allOut)
	require.NoError(t, runInboxList(cmd, nil))
	require.Contains(t, allOut.String(), item.ID)
}

func TestUnit_InboxShowCmd_NotFound(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "inbox-cli-404.db")
	db, err := libdb.NewSQLiteDBManager(context.Background(), dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	cmd := testCobraCmd()
	require.NoError(t, cmd.Root().PersistentFlags().Set("db", dbPath))
	cmd.SetOut(&bytes.Buffer{})

	err = runInboxShow(cmd, []string{"no-such-id"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "inbox list")
}

func TestUnit_InboxAckCmd_NotFound(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "inbox-cli-404-ack.db")
	db, err := libdb.NewSQLiteDBManager(context.Background(), dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	cmd := testCobraCmd()
	require.NoError(t, cmd.Root().PersistentFlags().Set("db", dbPath))
	cmd.SetOut(&bytes.Buffer{})

	err = runInboxAck(cmd, []string{"no-such-id"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "inbox list")
}
