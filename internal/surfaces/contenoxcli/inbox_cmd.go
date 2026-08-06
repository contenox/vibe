// inbox_cmd.go holds the `contenox inbox` verbs: a thin read/ack surface over
// internal/services/operatorinbox — durable reports (and blocker questions)
// that reached no live session to answer them. Distinct from `contenox
// approvals`, the live ask queue still waiting on a verdict.
package contenoxcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/contenox/contenox/internal/services/operatorinbox"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

var inboxCmd = &cobra.Command{
	Use:   "inbox",
	Short: "The durable operator inbox: reports (and blockers) a mission left behind for nobody to read yet.",
	Long: `A mission dispatched directly by an operator has no chat session listening
for its reports; one fired from a session whose process later ended has none
anymore either. Either way, its reports land here instead of vanishing —
'inbox list' shows what is waiting, 'inbox show' prints one in full, and
'inbox ack' marks it read.

This is NOT the live ask queue: a mission's PENDING permission gates and
questions still waiting on a verdict live in 'contenox approvals' (and,
scoped to one mission, 'contenox mission asks'). The inbox is for reports
that already landed with nobody there to see them.

Examples:
  contenox inbox list
  contenox inbox list --all
  contenox inbox show <id>
  contenox inbox ack <id>`,
}

var inboxListCmd = &cobra.Command{
	Use:   "list",
	Short: "List inbox items, newest first (unacknowledged only, unless --all).",
	Args:  cobra.NoArgs,
	RunE:  runInboxList,
}

var inboxShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show one inbox item in full: its report and the mission it came from.",
	Args:  cobra.ExactArgs(1),
	RunE:  runInboxShow,
}

var inboxAckCmd = &cobra.Command{
	Use:   "ack <id>",
	Short: "Acknowledge an inbox item — marks it read, does not delete it.",
	Args:  cobra.ExactArgs(1),
	RunE:  runInboxAck,
}

func init() {
	inboxListCmd.Flags().Int("limit", 50, "Maximum number of inbox items to list")
	inboxListCmd.Flags().Bool("all", false, "Include acknowledged items too (default: unacknowledged only)")

	inboxCmd.AddCommand(inboxListCmd)
	inboxCmd.AddCommand(inboxShowCmd)
	inboxCmd.AddCommand(inboxAckCmd)
	rootCmd.AddCommand(inboxCmd)
}

// openInboxService opens the shared database and returns an
// operatorinbox.Service over it.
func openInboxService(cmd *cobra.Command) (io.Closer, operatorinbox.Service, error) {
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid database path: %w", err)
	}
	dbCtx := libtracker.WithNewRequestID(context.Background())
	db, err := OpenDBAt(dbCtx, dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open database: %w", err)
	}
	return db, operatorinbox.New(db), nil
}

func runInboxList(cmd *cobra.Command, args []string) error {
	ctx := libtracker.WithNewRequestID(context.Background())
	closer, inbox, err := openInboxService(cmd)
	if err != nil {
		return err
	}
	defer closer.Close()

	limit, _ := cmd.Flags().GetInt("limit")
	all, _ := cmd.Flags().GetBool("all")

	var items []*operatorinbox.Item
	if all {
		items, err = inbox.List(ctx, limit)
	} else {
		items, err = inbox.ListUnacked(ctx, limit)
	}
	if err != nil {
		return fmt.Errorf("failed to list inbox: %w", err)
	}
	return renderInboxTable(cmd.OutOrStdout(), items, all, time.Now().UTC())
}

func runInboxShow(cmd *cobra.Command, args []string) error {
	ctx := libtracker.WithNewRequestID(context.Background())
	closer, inbox, err := openInboxService(cmd)
	if err != nil {
		return err
	}
	defer closer.Close()

	item, err := inbox.Get(ctx, args[0])
	if err != nil {
		if errors.Is(err, operatorinbox.ErrNotFound) {
			return fmt.Errorf("no inbox item %q exists — 'contenox inbox list --all' shows every item, acknowledged or not", args[0])
		}
		return fmt.Errorf("failed to show inbox item %q: %w", args[0], err)
	}
	renderInboxItem(cmd.OutOrStdout(), item, time.Now().UTC())
	return nil
}

func runInboxAck(cmd *cobra.Command, args []string) error {
	ctx := libtracker.WithNewRequestID(context.Background())
	closer, inbox, err := openInboxService(cmd)
	if err != nil {
		return err
	}
	defer closer.Close()

	if err := inbox.Ack(ctx, args[0]); err != nil {
		if errors.Is(err, operatorinbox.ErrNotFound) {
			return fmt.Errorf("no inbox item %q exists — 'contenox inbox list --all' shows every item, acknowledged or not", args[0])
		}
		return fmt.Errorf("failed to acknowledge inbox item %q: %w", args[0], err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Inbox item %s acknowledged.\n", args[0])
	return nil
}

// ─── rendering ──────────────────────────────────────────────────────────────

func renderInboxTable(w io.Writer, items []*operatorinbox.Item, all bool, now time.Time) error {
	if len(items) == 0 {
		if all {
			fmt.Fprintln(w, "Operator inbox is empty. A mission's report lands here only when it had no live session to reach — 'contenox mission fire' without a supervising session, or a session that ended before the report arrived.")
			return nil
		}
		fmt.Fprintln(w, "No unacknowledged inbox items. 'contenox inbox list --all' also shows what was already read.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tREASON\tMISSION\tKIND\tSUMMARY\tAGE\tACKED")
	for _, it := range items {
		acked := "no"
		if it.Acked {
			acked = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			it.ID, inboxReasonLabel(it.Reason), it.MissionID, it.Report.Kind, it.Report.Summary,
			formatMissionAge(now, it.CreatedAt), acked)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nFull detail: contenox inbox show <id>. Mark read: contenox inbox ack <id>.")
	return nil
}

func renderInboxItem(w io.Writer, it *operatorinbox.Item, now time.Time) {
	fmt.Fprintf(w, "Inbox item: %s\n", it.ID)
	fmt.Fprintf(w, "Mission:    %s\n", it.MissionID)
	if it.AgentName != "" {
		fmt.Fprintf(w, "Agent:      %s\n", it.AgentName)
	}
	if it.Intent != "" {
		fmt.Fprintf(w, "Intent:     %s\n", it.Intent)
	}
	fmt.Fprintf(w, "Reason:     %s\n", inboxReasonLabel(it.Reason))
	if it.ParentSessionID != "" {
		fmt.Fprintf(w, "Session:    %s (gone by the time this report landed)\n", it.ParentSessionID)
	}
	acked := "no"
	if it.Acked {
		acked = "yes"
	}
	fmt.Fprintf(w, "Acked:      %s\n", acked)
	fmt.Fprintf(w, "Landed:     %s (%s ago)\n", it.CreatedAt.Format(time.RFC3339), formatMissionAge(now, it.CreatedAt))

	fmt.Fprintln(w)
	fmt.Fprintf(w, "[%s] %s\n", it.Report.Kind, it.Report.Summary)
	if d := strings.TrimSpace(it.Report.Detail); d != "" {
		fmt.Fprintf(w, "Detail:\n%s\n", d)
	}
	if len(it.Report.Refs) > 0 {
		fmt.Fprintf(w, "Refs: %s\n", strings.Join(it.Report.Refs, ", "))
	}
	if h := it.Report.Handover; h != nil {
		fmt.Fprintln(w, "Handover:")
		if h.Outcome != "" {
			fmt.Fprintf(w, "  Outcome:  %s\n", h.Outcome)
		}
		if len(h.Artifacts) > 0 {
			fmt.Fprintf(w, "  Artifacts: %s\n", strings.Join(h.Artifacts, ", "))
		}
		if h.HandoverForNext != "" {
			fmt.Fprintf(w, "  For next: %s\n", h.HandoverForNext)
		}
		if h.Caveats != "" {
			fmt.Fprintf(w, "  Caveats:  %s\n", h.Caveats)
		}
	}
	fmt.Fprintf(w, "\nFull mission record: contenox mission show %s\n", it.MissionID)
}

// inboxReasonLabel renders operatorinbox.Reason in plain words.
func inboxReasonLabel(r operatorinbox.Reason) string {
	switch r {
	case operatorinbox.ReasonOperatorFired:
		return "operator-fired (no session)"
	case operatorinbox.ReasonParentGone:
		return "parent session gone"
	default:
		return string(r)
	}
}
