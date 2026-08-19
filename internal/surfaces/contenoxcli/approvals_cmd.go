package contenoxcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

var approvalsCmd = &cobra.Command{
	Use:   "approvals",
	Short: "The durable ask inbox: list pending approvals and questions, and answer them",
	Long: `A gated tool call or a mission's question becomes a durable ask the moment it
is raised — the row is written before anything waits. The run that raised it then
waits on that row, so answering it there carries the turn on in place; this inbox
is for the asks whose process is not waiting any more, because it was detached
(a trigger firing), or it shut down, or it is simply somewhere else. The ask is a
row ANY process can answer — 'approvals respond' records the verdict and, when a
run is checkpointed under it, resumes that run right here.

EXPIRES-IN is how long an ask has left, and an expired ask resolves to a denial.
How long that is comes from the envelope that gated the call: a grant written as

  shell = { grant = "approve", timeout = "30m", on_timeout = "deny" }

in [envelopes.<name>] gives its asks thirty minutes, and

  merge = { grant = "approve", timeout = "never" }

gives them no deadline at all — EXPIRES-IN reads "never", nothing sweeps them,
and they are still here after a restart, waiting to be answered. A grant that
names no timeout leaves the ask on this host's approval ceiling ('contenox
config set approval-ceiling <duration|never>', seven days until you set it).
"deny" is the only on_timeout this build can express, and a grant that names
none resolves the same way. Answering an ask whose window already closed is
refused rather than applied twice.`,
}

var approvalsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List pending approvals and attention asks, newest first, with the wait each has left",
	Long: `Print every ask still pending on this host: permission gates gated by an
envelope, and the questions a unit raised for a human's own words.

EXPIRES-IN is the wait the ask has LEFT, and it is the whole of what the
configuration gives it:

  45s, 30m,    the grant named a timeout; at zero the ask resolves to its
  4h, 6d       on_timeout (deny) and the run behind it resumes denied
  never        the grant named timeout = "never" — no deadline at all, nothing
               sweeps it, and it is still listed here after a restart

An ask whose grant named no timeout shows the wait this host's approval ceiling
gave it ('contenox config set approval-ceiling <duration|never>', seven days
until you set it) — not a wait of its own.

Listing is not passive: this command first sweeps asks whose window closed to
their on-timeout verdict (reporting how many), and resumes any answered run
still waiting for a capable process. An ask that vanishes between two listings
expired; it did not go unanswered quietly.`,
	RunE: runApprovalsList,
}

var approvalsRespondCmd = &cobra.Command{
	Use:   "respond [ask-id]",
	Short: "Answer a pending ask (--approve/--deny a permission, --answer a question) and let the run behind it carry on",
	Long: `Record a verdict on one ask, from any terminal, whether or not the process that
raised it is still alive.

  contenox approvals respond <id> --approve
  contenox approvals respond <id> --deny
  contenox approvals respond <id> --answer "use the staging bucket"

Exactly one of --approve, --deny, or --answer. A permission ask takes a verdict;
a question takes words. 'approvals list' says which is which.

What the verdict does depends on where the run behind the ask is. Usually it is
still waiting on it — an ask blocks the call that raised it — and the verdict
lands on the durable row that call is watching, so it stops waiting, the gated
tool runs or is refused, and that turn carries on in place. Nothing resumes,
because nothing suspended. If instead the run's process is gone — you quit it,
the host stopped, a trigger fired it detached — the run was checkpointed beside
the ask, and this command resumes it here, to completion, exactly once.

An answer lands only while the ask is still pending. Answer one whose window
already closed and this refuses rather than applying a second verdict: its
on-timeout verdict was applied when it expired, and a verdict is recorded
exactly once. An ask listed as EXPIRES-IN never has no window to miss — it
waits until this command answers it, however many restarts away that is.

Resuming needs a reachable model, so an ask with a checkpoint under it is
answerable only from a terminal that can build an engine: if this one cannot,
the verdict is NOT recorded and the ask stays pending — fix it here ('contenox
setup') or answer from a terminal that can. An ask whose run is still waiting
elsewhere needs no engine here; the verdict is all it wants.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runApprovalsRespond,
}

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
	svc := newHITLService(dbCtx, contenoxDir, store, libtracker.NoopTracker{}, "")
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

	if swept, err := svc.SweepExpired(ctx); err == nil && swept > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Swept %d expired ask(s) to their on-timeout verdict.\n\n", swept)
	}

	if ids, serr := agentservice.StrandedCheckpoints(ctx, store, strandedSweepLimit); serr == nil && len(ids) > 0 {
		deps, cleanup, buildErr := buildResumeDeps(cmd, ctx, svc)
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
		expires := "never"
		if !row.ExpiresAt.IsZero() {
			expires = formatMissionAge(row.ExpiresAt, now)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			row.ID, kind, row.ToolsName+"."+row.ToolName, row.ArgsSummary, mission,
			formatMissionAge(now, row.CreatedAt), expires)
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

const strandedSweepLimit = 200

type askRefusalError struct{ err error }

func (e askRefusalError) Error() string { return e.err.Error() }
func (e askRefusalError) Unwrap() error { return e.err }

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
		// Pre-check only: the atomic write below enforces the bound.
		if err := hitlservice.EnforceAgentAnswerBounds(ctx, missionservice.New(db), svc, row); err != nil {
			return askRefusalError{err}
		}
	}

	_, checkpointErr := store.GetChainCheckpoint(ctx, askID)
	hasCheckpoint := checkpointErr == nil

	// Capability to resume is proven before anything is recorded.
	if hasCheckpoint {
		// BuildEngine registers the resume hook on svc itself (see engine.go),
		// so the verdict recorded below resumes through the one hook this
		// process has.
		_, cleanup, buildErr := buildResumeDeps(cmd, ctx, svc)
		if buildErr != nil {
			return fmt.Errorf("ask %s has a suspended run checkpointed under it, and this process cannot build an engine to resume it: %v\nThe verdict was NOT recorded — the ask is still pending. Fix the configuration here ('contenox setup', or 'contenox config set default-model ...'), or answer from a terminal that can reach your models", askID, buildErr)
		}
		defer cleanup()
	}

	switch {
	case isQuestion && asAgentSet:
		// Not AnswerAsAgentNamed: the envelope's cap must ride the write.
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
		// "Resumed", not "completed": the hook returns nil for a terminal run and
		// for a clean re-suspension.
		fmt.Fprintf(out, "Verdict recorded for %s and the suspended run was resumed in this process. If it paused on a further ask, 'contenox approvals list' shows it.\n", askID)
	} else {
		fmt.Fprintf(out, "Verdict recorded for %s. Nothing was checkpointed under it, so nothing resumed here — the run that raised it is watching this row in its own process and carries on there, if it is still up.\n", askID)
	}
	return nil
}

// buildResumeDeps builds the engine that resumes a checkpointed run. hitlSvc is
// the already-open service this command answers on, and it is handed to
// BuildEngine so the resumed run gates on THAT service: one instance, one
// resume hook, one card writer. Minting a second one here would leave a resumed
// run's next gated call asking on a service the outer verdict never reaches.
func buildResumeDeps(cmd *cobra.Command, ctx context.Context, hitlSvc hitlservice.Service) (agentservice.Deps, func(), error) {
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
	// HITL gate stays on: without it the resumed unit's next question self-answers
	// as a blocker.
	opts.EffectiveHITL = true
	opts.EffectiveHITLService = hitlSvc
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

func reopenForEngine(cmd *cobra.Command) (libdbexec.DBManager, error) {
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, err
	}
	return OpenDBAt(libtracker.WithNewRequestID(context.Background()), dbPath)
}

const asAgentFlagName = "as-agent"

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
