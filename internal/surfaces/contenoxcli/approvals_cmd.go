package contenoxcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/spf13/cobra"
)

var approvalsCmd = &cobra.Command{
	Use:   "approvals",
	Short: "The durable ask inbox: list pending approvals and questions, and answer them",
	Long: `A gated tool call or a mission's question parks for a short window, then
checkpoints the run and releases its process. The ask stays a durable row any
process can answer later — 'approvals respond' records the verdict and resumes
the suspended run right here, even if the process that asked is long gone.`,
}

var approvalsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List pending approvals and attention asks",
	RunE:  runApprovalsList,
}

var approvalsRespondCmd = &cobra.Command{
	Use:   "respond <ask-id>",
	Short: "Answer a pending ask (--approve/--deny a permission, --answer a question) and resume the suspended run",
	Args:  cobra.ExactArgs(1),
	RunE:  runApprovalsRespond,
}

// openApprovalsService opens the shared database and builds the durable-ask
// service over it, using the one constructor the whole codebase uses.
func openApprovalsService(cmd *cobra.Command) (io.Closer, hitlservice.Service, runtimetypes.Store, error) {
	contenoxDir, err := ResolveContenoxDir(cmd)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid database path: %w", err)
	}
	dbCtx := libtracker.WithNewRequestID(context.Background())
	db, err := OpenDBAt(dbCtx, dbPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to open database: %w", err)
	}
	store := runtimetypes.New(db.WithoutTransaction())
	svc := hitlservice.NewWithDefaultPolicy(hitlPolicySource(contenoxDir), runtimetypes.LocalTenantID, store, libtracker.NoopTracker{}, "")
	return db, svc, store, nil
}

func runApprovalsList(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	closer, svc, _, err := openApprovalsService(cmd)
	if err != nil {
		return err
	}
	defer closer.Close()

	// The inbox read is the sweeper's tick: an expired ask must resolve
	// rather than sit pending forever.
	if swept, err := svc.SweepExpired(ctx); err == nil && swept > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Swept %d expired ask(s) to their on-timeout verdict.\n\n", swept)
	}

	limit, _ := cmd.Flags().GetInt("limit")
	pending, err := svc.ListPending(ctx, limit)
	if err != nil {
		return fmt.Errorf("list pending asks: %w", err)
	}
	return renderApprovalsTable(cmd.OutOrStdout(), pending, time.Now())
}

func renderApprovalsTable(w io.Writer, rows []*runtimetypes.HITLApproval, now time.Time) error {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No pending asks. Gated tool calls and mission questions land here when nobody is watching the session.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tTOOL\tSUMMARY\tMISSION\tAGE\tEXPIRES-IN")
	for _, row := range rows {
		kind := "permission"
		if hitlservice.IsAttentionAsk(row) {
			kind = "question"
		}
		mission := ""
		if row.MissionID != nil {
			mission = *row.MissionID
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			row.ID, kind, row.ToolsName+"."+row.ToolName, row.ArgsSummary, mission,
			formatMissionAge(now, row.CreatedAt), formatMissionAge(row.ExpiresAt, now))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, row := range rows {
		if row.Diff != nil && strings.TrimSpace(*row.Diff) != "" {
			fmt.Fprintf(w, "\nDiff for %s:\n%s\n", row.ID, *row.Diff)
		}
	}
	fmt.Fprintln(w, "\nAnswer with 'contenox approvals respond <id> --approve|--deny' or '--answer \"...\"' for a question.")
	return nil
}

func runApprovalsRespond(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	askID := args[0]
	approve, _ := cmd.Flags().GetBool("approve")
	deny, _ := cmd.Flags().GetBool("deny")
	answer, _ := cmd.Flags().GetString("answer")

	verdictCount := 0
	if approve {
		verdictCount++
	}
	if deny {
		verdictCount++
	}
	if answer != "" {
		verdictCount++
	}
	if verdictCount != 1 {
		return fmt.Errorf("exactly one of --approve, --deny, or --answer is required")
	}

	closer, svc, store, err := openApprovalsService(cmd)
	if err != nil {
		return err
	}
	defer closer.Close()

	row, err := store.GetHITLApproval(ctx, askID)
	if err != nil {
		return fmt.Errorf("no ask %q exists — 'contenox approvals list' shows what is pending", askID)
	}
	isQuestion := hitlservice.IsAttentionAsk(row)
	if isQuestion && answer == "" {
		return fmt.Errorf("ask %s is a QUESTION (%s) — answer it with --answer \"your words\", not a yes/no verdict", askID, row.ArgsSummary)
	}
	if !isQuestion && answer != "" {
		return fmt.Errorf("ask %s is a permission ask (%s.%s) — it takes --approve or --deny, not text", askID, row.ToolsName, row.ToolName)
	}

	// With a checkpoint the verdict resumes the run in this process; without
	// one it is only recorded.
	_, checkpointErr := store.GetChainCheckpoint(ctx, askID)
	hasCheckpoint := checkpointErr == nil

	// Degrade honestly when no engine can be built (e.g. no model
	// configured): record the verdict hook-less rather than refuse to answer.
	engineReady := false
	if hasCheckpoint {
		contenoxDir, dirErr := ResolveContenoxDir(cmd)
		if dirErr == nil {
			if db, dbErr := reopenForEngine(cmd); dbErr == nil {
				defer db.Close()
				opts, optsErr := buildRunOpts(cmd, db, contenoxDir)
				if optsErr == nil {
					if engine, engErr := BuildEngine(ctx, db, opts); engErr == nil {
						defer engine.Stop()
						hitlservice.SetResumeHook(svc, agentservice.ResumeHook(agentservice.Deps{
							Engine:      engine,
							DB:          db,
							WorkspaceID: ResolveWorkspaceID(contenoxDir),
						}))
						engineReady = true
					}
				}
			}
		}
		if !engineReady {
			fmt.Fprintln(cmd.OutOrStdout(), "No engine could be built (is a default model configured?); the verdict will be recorded and the run resumes when a capable process next answers or sweeps.")
		}
	}

	switch {
	case isQuestion:
		err = svc.Answer(ctx, askID, answer)
	default:
		err = svc.Respond(ctx, askID, approve)
	}
	if err != nil {
		switch {
		case errors.Is(err, hitlservice.ErrApprovalNotFound):
			return fmt.Errorf("no ask %q exists — 'contenox approvals list' shows what is pending", askID)
		case errors.Is(err, hitlservice.ErrApprovalAlreadyResolved):
			return fmt.Errorf("ask %s was already answered — a verdict is recorded exactly once", askID)
		case errors.Is(err, hitlservice.ErrApprovalExpired):
			return fmt.Errorf("ask %s expired before this answer; its on-timeout verdict (%s) already applied", askID, row.OnTimeout)
		default:
			return fmt.Errorf("respond to ask %s: %w", askID, err)
		}
	}

	out := cmd.OutOrStdout()
	switch {
	case !hasCheckpoint:
		fmt.Fprintf(out, "Verdict recorded for %s; nothing was suspended under it (the asker is parked in its own process or already past its deadline).\n", askID)
	case engineReady:
		fmt.Fprintf(out, "Verdict recorded for %s and the suspended run was resumed to completion in this process.\n", askID)
	default:
		fmt.Fprintf(out, "Verdict recorded for %s.\n", askID)
	}
	return nil
}

// reopenForEngine opens a second handle for the engine build. The engine owns
// backend sync and tool wiring over it; the inbox service keeps its own.
func reopenForEngine(cmd *cobra.Command) (libdbexec.DBManager, error) {
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, err
	}
	return OpenDBAt(libtracker.WithNewRequestID(context.Background()), dbPath)
}

func init() {
	approvalsListCmd.Flags().Int("limit", 50, "Maximum number of asks to list")
	approvalsRespondCmd.Flags().Bool("approve", false, "Approve a pending permission ask")
	approvalsRespondCmd.Flags().Bool("deny", false, "Deny a pending permission ask")
	approvalsRespondCmd.Flags().String("answer", "", "Answer a pending question (attention ask) with your own words")
	approvalsCmd.AddCommand(approvalsListCmd)
	approvalsCmd.AddCommand(approvalsRespondCmd)
}
