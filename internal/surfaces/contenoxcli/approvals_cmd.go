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
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
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
	Use:   "respond [ask-id]",
	Short: "Answer a pending ask (--approve/--deny a permission, --answer a question) and resume the suspended run",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runApprovalsRespond,
}

// openApprovalsService opens the shared database and builds the durable-ask
// service over it, using the one constructor the whole codebase uses. The
// returned DBManager is both the closer and, for the --as-agent path, the
// handle a missionservice reads the ask's envelope through.
func openApprovalsService(cmd *cobra.Command) (libdbexec.DBManager, hitlservice.Service, runtimetypes.Store, error) {
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
	svc := newHITLService(contenoxDir, store, libtracker.NoopTracker{}, "")
	return db, svc, store, nil
}

func runApprovalsList(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	db, svc, store, err := openApprovalsService(cmd)
	if err != nil {
		return err
	}
	defer db.Close()

	// The inbox read is the sweeper's tick: an expired ask must resolve
	// rather than sit pending forever.
	if swept, err := svc.SweepExpired(ctx); err == nil && swept > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Swept %d expired ask(s) to their on-timeout verdict.\n\n", swept)
	}

	// The same read reconciles stranded resumes: an answered checkpoint with
	// no live claim (a resumer crashed mid-run, or a failed resume whose claim
	// went stale) is carried to completion here. The engine is built only when
	// a strand actually exists.
	if ids, serr := agentservice.StrandedCheckpoints(ctx, store, strandedSweepLimit); serr == nil && len(ids) > 0 {
		deps, cleanup, buildErr := buildResumeDeps(cmd, ctx)
		if buildErr != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "%d answered run(s) are checkpointed and waiting for a capable process; this one cannot build an engine (%v). They resume where a model is configured, on the next 'approvals list' or 'approvals respond'.\n\n", len(ids), buildErr)
		} else {
			resumed, failed, sweepErr := agentservice.SweepStrandedCheckpoints(ctx, deps, strandedSweepLimit)
			cleanup()
			if sweepErr == nil && resumed+failed > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Resumed %d stranded run(s) to completion in this process.\n", resumed)
				if failed > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "%d resume(s) failed; their checkpoints are retained with the failure and retried once the claim goes stale.\n", failed)
				}
				fmt.Fprintln(cmd.OutOrStdout())
			}
		}
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
	approve, _ := cmd.Flags().GetBool("approve")
	deny, _ := cmd.Flags().GetBool("deny")
	answer, _ := cmd.Flags().GetString("answer")
	asAgent, asAgentSet, err := asAgentFlagValue(cmd)
	if err != nil {
		return err
	}

	if len(args) != 1 {
		return fmt.Errorf("respond takes exactly one ask id — 'contenox approvals list' shows what is pending")
	}

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
	if asAgentSet && answer == "" {
		return fmt.Errorf("--as-agent attributes a question's answer; pair it with --answer \"...\"")
	}
	return respondToAsk(cmd, args[0], approve, answer, asAgent, asAgentSet)
}

// strandedSweepLimit bounds checkpoints examined per reconciling read, the
// same idea as hitlservice's sweepBatchLimit; the next read picks up the rest.
const strandedSweepLimit = 200

// askRefusalError wraps a respond outcome that is a bound or durable record
// holding — ask missing, already resolved, expired, or the mission envelope
// refusing an agent answer — as distinct from broken plumbing. Error text is
// the wrapped error's, unchanged.
type askRefusalError struct{ err error }

func (e askRefusalError) Error() string { return e.err.Error() }
func (e askRefusalError) Unwrap() error { return e.err }

// respondToAsk records one verdict for askID — approve/deny for a permission
// ask, answer (optionally agent-attributed) for a question — and resumes the
// suspended run in-process when a checkpoint and an engine exist. Refusals
// that are bounds or records holding come back as askRefusalError.
func respondToAsk(cmd *cobra.Command, askID string, approve bool, answer, asAgent string, asAgentSet bool) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	db, svc, store, err := openApprovalsService(cmd)
	if err != nil {
		return err
	}
	defer db.Close()

	row, err := store.GetHITLApproval(ctx, askID)
	if err != nil {
		return askRefusalError{fmt.Errorf("no ask %q exists — 'contenox approvals list' shows what is pending", askID)}
	}
	isQuestion := hitlservice.IsAttentionAsk(row)
	if isQuestion && answer == "" {
		return fmt.Errorf("ask %s is a QUESTION (%s) — answer it with --answer \"your words\", not a yes/no verdict", askID, row.ArgsSummary)
	}
	if !isQuestion && answer != "" {
		return fmt.Errorf("ask %s is a permission ask (%s.%s) — it takes --approve or --deny, not text", askID, row.ToolsName, row.ToolName)
	}
	if asAgentSet {
		// The envelope always wins: a refusal here is the mission's own bounds
		// holding, checked before any engine is built or verdict recorded. The
		// bound is held for real by the atomic write below; this pre-check only
		// spares an engine build for an answer the envelope was never going to
		// take.
		if err := hitlservice.EnforceAgentAnswerBounds(ctx, missionservice.New(db), svc, row); err != nil {
			return askRefusalError{err}
		}
	}

	// With a checkpoint the verdict resumes the run in this process; without
	// one it is only recorded.
	_, checkpointErr := store.GetChainCheckpoint(ctx, askID)
	hasCheckpoint := checkpointErr == nil

	// Ordering, not degradation: the verdict for a checkpointed run is
	// one-shot, so capability is proven BEFORE anything is recorded. Refusal
	// leaves the ask pending and answerable elsewhere; hitlservice enforces
	// the same rule (ErrVerdictNeedsResumer) for any surface that skips this.
	if hasCheckpoint {
		deps, cleanup, buildErr := buildResumeDeps(cmd, ctx)
		if buildErr != nil {
			return fmt.Errorf("ask %s has a suspended run checkpointed under it, and this process cannot build an engine to resume it: %v\nThe verdict was NOT recorded — the ask is still pending. Fix the configuration here ('contenox setup', or 'contenox config set default-model ...'), or answer from a terminal that can reach your models", askID, buildErr)
		}
		defer cleanup()
		hitlservice.SetResumeHook(svc, agentservice.ResumeHook(deps))
	}

	switch {
	case isQuestion && asAgentSet:
		// Not AnswerAsAgentNamed: the envelope's cap must ride the write, or a
		// second process answering the same mission concurrently overruns it.
		err = hitlservice.AnswerAsAgentWithinBounds(ctx, missionservice.New(db), svc, row, asAgent, answer)
	case isQuestion:
		err = svc.Answer(ctx, askID, answer)
	default:
		err = svc.Respond(ctx, askID, approve)
	}
	if err != nil {
		switch {
		case hitlservice.IsAgentAnswerRefusal(err):
			return askRefusalError{err}
		case errors.Is(err, hitlservice.ErrApprovalNotFound):
			return askRefusalError{fmt.Errorf("no ask %q exists — 'contenox approvals list' shows what is pending", askID)}
		case errors.Is(err, hitlservice.ErrApprovalAlreadyResolved):
			return askRefusalError{fmt.Errorf("ask %s was already answered — a verdict is recorded exactly once", askID)}
		case errors.Is(err, hitlservice.ErrApprovalExpired):
			return askRefusalError{fmt.Errorf("ask %s expired before this answer; its on-timeout verdict (%s) already applied", askID, row.OnTimeout)}
		default:
			return fmt.Errorf("respond to ask %s: %w", askID, err)
		}
	}

	out := cmd.OutOrStdout()
	if asAgentSet {
		fmt.Fprintf(out, "Answered as agent %q — the durable record attributes this answer to it, and it counts against the mission's agent-answer bound.\n", asAgent)
	}
	if hasCheckpoint {
		// "Resumed", not "completed": the hook returns nil for a terminal run
		// AND for a clean re-suspension on the run's next ask.
		fmt.Fprintf(out, "Verdict recorded for %s and the suspended run was resumed in this process. If it paused on a further ask, 'contenox approvals list' shows it.\n", askID)
	} else {
		fmt.Fprintf(out, "Verdict recorded for %s; nothing was suspended under it (the asker is parked in its own process or already past its deadline).\n", askID)
	}
	return nil
}

// buildResumeDeps assembles the engine-bearing Deps this process needs to
// resume a suspended run, with a cleanup closing the engine and its own db
// handle. An error means this process cannot resume (usually: no usable model
// configuration); callers refuse or report rather than degrade silently.
func buildResumeDeps(cmd *cobra.Command, ctx context.Context) (agentservice.Deps, func(), error) {
	contenoxDir, err := ResolveContenoxDir(cmd)
	if err != nil {
		return agentservice.Deps{}, nil, err
	}
	db, err := reopenForEngine(cmd)
	if err != nil {
		return agentservice.Deps{}, nil, err
	}
	opts, err := buildRunOpts(cmd, db, contenoxDir)
	if err != nil {
		db.Close()
		return agentservice.Deps{}, nil, err
	}
	// The resume engine keeps its HITL gate unconditionally: the gate is what
	// gives its mission tools an attention asker, and the run being resumed is
	// one that suspended at that very machinery. Ungated, the resumed unit's
	// next question would have nowhere to go and would self-answer as a blocker.
	opts.EffectiveHITL = true
	engine, err := BuildEngine(ctx, db, opts)
	if err != nil {
		db.Close()
		return agentservice.Deps{}, nil, err
	}
	cleanup := func() {
		engine.Stop()
		db.Close()
	}
	return agentservice.Deps{
		Engine:      engine,
		DB:          db,
		WorkspaceID: ResolveWorkspaceID(contenoxDir),
	}, cleanup, nil
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

// asAgentFlagName is the beta flag attributing a question's answer to an
// agent. Registered only under opt-in-beta (see registerApprovalsRespondFlags):
// a stable invocation neither shows nor parses it.
const asAgentFlagName = "as-agent"

// asAgentFlagValue reads the beta --as-agent flag. set is false when the flag
// is unregistered (beta off) or not given; a given-but-blank name is an error,
// never a silent fall-through to the human path.
func asAgentFlagValue(cmd *cobra.Command) (name string, set bool, err error) {
	f := cmd.Flags().Lookup(asAgentFlagName)
	if f == nil || !f.Changed {
		return "", false, nil
	}
	name = strings.TrimSpace(f.Value.String())
	if name == "" {
		return "", false, fmt.Errorf("--as-agent requires a non-empty agent name")
	}
	return name, true, nil
}

// registerApprovalsRespondFlags installs respond's flag set. --as-agent joins
// only under opt-in-beta — absent from a stable help and refused as an
// unknown flag, matching how Main visibility-gates agentCmd. Idempotent via
// ResetFlags so Main may re-resolve the gate after init's stable default.
func registerApprovalsRespondFlags(beta bool) {
	approvalsRespondCmd.ResetFlags()
	approvalsRespondCmd.Flags().Bool("approve", false, "Approve a pending permission ask")
	approvalsRespondCmd.Flags().Bool("deny", false, "Deny a pending permission ask")
	approvalsRespondCmd.Flags().String("answer", "", "Answer a pending question (attention ask) with your own words")
	if beta {
		approvalsRespondCmd.Flags().String(asAgentFlagName, "", "Record the answer as given by the named agent; allowed only within the mission envelope's attention bounds")
	}
}

func init() {
	approvalsListCmd.Flags().Int("limit", 50, "Maximum number of asks to list")
	registerApprovalsRespondFlags(false)
	approvalsCmd.AddCommand(approvalsListCmd)
	approvalsCmd.AddCommand(approvalsRespondCmd)
}
