package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/contenox/contenox/execservice"
	libdbexec "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/planservice"
	"github.com/contenox/contenox/planstore"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/taskengine"
	"github.com/contenox/contenox/vfsservice"
	"github.com/spf13/cobra"
)

var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Manage execution plans (new, list, show, next, retry, skip, replan, delete, clean).",
	Long: `Create and execute multi-step AI plans that run shell commands on your machine.

Workflow:
  1. contenox plan new "<goal>"    # LLM generates a step-by-step plan and saves it
  2. contenox plan show            # inspect the generated steps
  3. contenox plan next --shell    # execute the next pending step (enable shell tools)
  4. contenox plan next --auto --shell  # run all steps until done or failed

On failure:
  contenox plan retry <N>    # reset step N back to pending and retry
  contenox plan skip  <N>    # mark step N as skipped and continue
  contenox plan replan       # ask the LLM to regenerate remaining steps

Note: plan execution requires a model that supports tool calling.
The active plan is tracked automatically; use 'contenox plan list' to see all plans.`,
	SilenceUsage: true,
}

var planNewCmd = &cobra.Command{
	Use:   "new <goal>",
	Short: "Create a new execution plan from a goal description.",
	Long: `Ask the LLM to break a goal into ordered steps and save the plan to SQLite.

The new plan becomes the active plan automatically.
Input can be provided as a positional argument, piped via stdin, or both:

  contenox plan new "set up a Go project with tests and CI"
  git diff | contenox plan new "write a commit message and update CHANGELOG"
  cat ERROR.log | contenox plan new "diagnose and fix this error"

The planner chain must output a JSON array of step descriptions. If your model
produces malformed output, switch to a stronger model via --model or
'contenox config set default-model'.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runPlanNew,
}

var planListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all plans (* = active).",
	Args:  cobra.NoArgs,
	RunE:  runPlanList,
}

var planShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show the active plan's status.",
	Args:  cobra.NoArgs,
	RunE:  runPlanShow,
}

var planNextCmd = &cobra.Command{
	Use:   "next",
	Short: "Execute the next pending step of the active plan.",
	Long: `Run the next pending step using the LLM step-executor chain.

The step is marked completed if the model outputs ===STEP_DONE=== or failed otherwise.

Flags:
  --auto     Continue executing steps until the plan is done or a step fails
  --shell    Enable the local_shell hook so the model can run commands

Examples:
  contenox plan next
  contenox plan next --shell             # single step with shell access
  contenox plan next --auto --shell      # run everything until done`,
	Args: cobra.NoArgs,
	RunE: runPlanNext,
}

var planRetryCmd = &cobra.Command{
	Use:   "retry <ordinal>",
	Short: "Reset a failed or skipped step back to pending.",
	Long: `Reset a step by its ordinal number (1-based) back to pending status so it
can be re-executed by 'contenox plan next'.

Example:
  contenox plan retry 3`,
	Args: cobra.ExactArgs(1),
	RunE: runPlanRetry,
}

var planSkipCmd = &cobra.Command{
	Use:   "skip <ordinal>",
	Short: "Mark a pending or failed step as skipped.",
	Long: `Mark a step as skipped so 'contenox plan next' moves on to the next one.
Useful when a step is not applicable or was completed manually.

Example:
  contenox plan skip 2`,
	Args: cobra.ExactArgs(1),
	RunE: runPlanSkip,
}

var planReplanCmd = &cobra.Command{
	Use:   "replan",
	Short: "Regenerate remaining steps based on current progress.",
	Long: `Ask the LLM to generate a new set of steps for the active plan, taking into
account steps that have already been completed, failed, or skipped.

Pending steps are deleted and replaced with the newly generated ones.
Completed and skipped steps are preserved.

Example:
  contenox plan replan`,
	Args: cobra.NoArgs,
	RunE: runPlanReplan,
}

var planDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a plan by name.",
	Args:  cobra.ExactArgs(1),
	RunE:  runPlanDelete,
}

var planCleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Delete all completed or archived plans.",
	RunE:  runPlanClean,
}

func init() {
	planCmd.AddCommand(planNewCmd, planListCmd, planShowCmd, planNextCmd, planRetryCmd, planSkipCmd, planReplanCmd, planDeleteCmd, planCleanCmd)
	planNextCmd.Flags().Bool("auto", false, "Continue executing steps automatically until the plan is done or a step fails")
	planNextCmd.Flags().Bool("shell", false, "Enable the local_shell hook for this plan step (required for shell-based tasks)")
}

// openPlanDB is similar to openSessionDB but for plans.
func openPlanDB(cmd *cobra.Command) (context.Context, libdbexec.DBManager, string, func(), error) {
	contenoxDir, err := ResolveContenoxDir()
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}

	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("invalid database path: %w", err)
	}

	ctx := libtracker.WithNewRequestID(context.Background())
	db, err := openDBAt(ctx, dbPath)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("failed to open database: %w", err)
	}
	cleanup := func() { _ = db.Close() }

	return ctx, db, contenoxDir, cleanup, nil
}

// WithTransaction is the exported helper for DB writes used by sub-packages.
func WithTransaction(ctx context.Context, db libdbexec.DBManager, fn func(tx libdbexec.Exec) error) error {
	return withTransaction(ctx, db, fn)
}

// withTransaction is the single source of truth for all plan DB writes.
func withTransaction(ctx context.Context, db libdbexec.DBManager, fn func(tx libdbexec.Exec) error) error {
	txExec, commit, release, err := db.WithTransaction(ctx)
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer release()
	if err := fn(txExec); err != nil {
		return err
	}
	if err := commit(ctx); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	return nil
}

func buildPlanOpts(cmd *cobra.Command, db libdbexec.DBManager, input string) chatOpts {
	flags := cmd.Root().Flags()

	ctx := libtracker.WithNewRequestID(context.Background())
	store := runtimetypes.New(db.WithoutTransaction())

	kvModel, _ := getConfigKV(ctx, store, "default-model")
	kvProvider, _ := getConfigKV(ctx, store, "default-provider")

	effectiveModel, _ := flags.GetString("model")
	if !flags.Changed("model") && (effectiveModel == "" || effectiveModel == defaultModel) {
		if kvModel != "" {
			effectiveModel = kvModel
		} else {
			effectiveModel = defaultModel
		}
	}

	effectiveDefaultProvider := kvProvider
	if flags.Changed("provider") {
		if v, _ := flags.GetString("provider"); v != "" {
			effectiveDefaultProvider = v
		}
	}

	effectiveContext, _ := flags.GetInt("context")
	effectiveTracing, _ := flags.GetBool("trace")
	effectiveEnableLocalExec, _ := flags.GetBool("shell")

	// Also check the subcommand's own local flags (e.g. plan next --shell).
	if localFlags := cmd.Flags(); localFlags != flags {
		if v, _ := localFlags.GetBool("shell"); localFlags.Changed("shell") {
			effectiveEnableLocalExec = v
		}
	}

	effectiveLocalExecAllowedDir, _ := flags.GetString("local-exec-allowed-dir")

	return chatOpts{
		InputFlagPassed:              true,
		InputValue:                   input,
		EffectiveDefaultModel:        effectiveModel,
		EffectiveDefaultProvider:     effectiveDefaultProvider,
		EffectiveContext:             effectiveContext,
		EffectiveEnableLocalExec:     effectiveEnableLocalExec,
		EffectiveLocalExecAllowedDir: effectiveLocalExecAllowedDir,
		EffectiveTracing:             effectiveTracing,
	}
}

// buildPlanService constructs a planservice.Service that persists plan markdown
// files under <cDir>/plans/ via a local VFS.
func buildPlanService(db libdbexec.DBManager, engine *Engine, cDir string) planservice.Service {
	plansDir := filepath.Join(cDir, "plans")
	vfs := vfsservice.NewLocalFS(plansDir)
	var taskSvc execservice.TasksEnvService
	if engine != nil {
		taskSvc = engine.TaskService
	}
	return planservice.New(db, taskSvc, vfs)
}

// execCtxForPlan builds a context with template vars set for plan chain execution.
func execCtxForPlan(ctx context.Context, opts chatOpts, chainID string) context.Context {
	return taskengine.WithTemplateVars(ctx, map[string]string{
		"model":    opts.EffectiveDefaultModel,
		"provider": opts.EffectiveDefaultProvider,
		"chain":    chainID,
	})
}

func runPlanNew(cmd *cobra.Command, args []string) error {
	// Resolve goal from arg or stdin; combine both if they coexist.
	var goal string
	if stat, err := os.Stdin.Stat(); err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
		stdinBytes, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("failed to read from stdin: %w", err)
		}
		stdinStr := strings.TrimSpace(string(stdinBytes))
		if stdinStr != "" {
			if goal != "" {
				goal = goal + "\n\n" + stdinStr
			} else {
				goal = stdinStr
			}
		}
	}
	if len(args) > 0 {
		goal = args[0]
	}
	if goal == "" {
		return fmt.Errorf("goal cannot be empty; provide an argument or pipe via stdin")
	}

	ctx, db, cDir, cleanup, err := openPlanDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	o := buildPlanOpts(cmd, db, goal)
	engine, err := BuildEngine(ctx, db, o)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}
	defer engine.Stop()

	plannerPath, _, err := ensurePlanChains(cDir)
	if err != nil {
		return fmt.Errorf("failed to ensure plan chains: %w", err)
	}

	chainData, err := os.ReadFile(plannerPath)
	if err != nil {
		return fmt.Errorf("failed to read planner chain: %w", err)
	}
	var plannerChain taskengine.TaskChainDefinition
	if err := json.Unmarshal(chainData, &plannerChain); err != nil {
		return fmt.Errorf("failed to parse planner chain: %w", err)
	}
	if err := validatePlannerChain(&plannerChain, plannerPath); err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Generating plan for: %s...\n", goal)

	planSvc := buildPlanService(db, engine, cDir)
	execCtx := execCtxForPlan(ctx, o, plannerChain.ID)

	plan, steps, _, err := planSvc.New(execCtx, goal, &plannerChain)
	if err != nil {
		return fmt.Errorf("plan generation failed: %w", err)
	}

	// Update the KV active pointer so `runPlanList` can mark the active plan.
	if err := withTransaction(ctx, db, func(tx libdbexec.Exec) error {
		return setActivePlanID(ctx, tx, plan.ID)
	}); err != nil {
		slog.Warn("failed to set active plan KV pointer", "error", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Created plan %q with %d steps. Now active.\n", plan.Name, len(steps))
	return nil
}

func runPlanList(cmd *cobra.Command, _ []string) error {
	ctx, db, _, cleanup, err := openPlanDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	exec := db.WithoutTransaction()
	store := planstore.New(exec)
	plans, err := store.ListPlans(ctx)
	if err != nil {
		return err
	}

	if len(plans) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No plans yet. Run: contenox plan new <goal>")
		return nil
	}

	activeID, _ := getActivePlanID(ctx, exec)
	for _, p := range plans {
		prefix := "  "
		if p.ID == activeID {
			prefix = "* "
		}

		steps, _ := store.ListPlanSteps(ctx, p.ID)
		completed := 0
		for _, s := range steps {
			if s.Status == planstore.StepStatusCompleted {
				completed++
			}
		}

		fmt.Fprintf(cmd.OutOrStdout(), "%s%-20s [%d/%d] %s\n", prefix, p.Name, completed, len(steps), p.Status)
	}
	return nil
}

func runPlanShow(cmd *cobra.Command, _ []string) error {
	ctx, db, _, cleanup, err := openPlanDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	exec := db.WithoutTransaction()
	activeID, err := getActivePlanID(ctx, exec)
	if err != nil || activeID == "" {
		return fmt.Errorf("no active plan; run 'contenox plan new' or 'contenox plan switch'")
	}

	store := planstore.New(exec)
	plan, err := store.GetPlanByID(ctx, activeID)
	if err != nil {
		return err
	}

	steps, err := store.ListPlanSteps(ctx, activeID)
	if err != nil {
		return err
	}

	completed := 0
	for _, s := range steps {
		if s.Status == planstore.StepStatusCompleted {
			completed++
		}
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Plan: %s (active) — %d/%d complete\n", plan.Name, completed, len(steps))
	for _, s := range steps {
		var checkbox string
		switch s.Status {
		case planstore.StepStatusCompleted:
			checkbox = "[x]"
		case planstore.StepStatusFailed, planstore.StepStatusSkipped:
			checkbox = "[-]"
		default:
			checkbox = "[ ]"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%d. %s %s\n", s.Ordinal, checkbox, s.Description)
	}
	return nil
}

func runPlanNext(cmd *cobra.Command, _ []string) error {
	ctx, db, cDir, cleanup, err := openPlanDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	isAuto, _ := cmd.Flags().GetBool("auto")

	o := buildPlanOpts(cmd, db, "")
	engine, err := BuildEngine(ctx, db, o)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}
	defer engine.Stop()

	_, executorPath, err := ensurePlanChains(cDir)
	if err != nil {
		return err
	}
	chainData, err := os.ReadFile(executorPath)
	if err != nil {
		return err
	}
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal(chainData, &chain); err != nil {
		return err
	}
	if err := validateExecutorChain(&chain, executorPath); err != nil {
		return err
	}

	planSvc := buildPlanService(db, engine, cDir)
	execCtx := execCtxForPlan(ctx, o, chain.ID)

	for {
		// Peek at the next pending step for display before execution.
		plan, steps, err := planSvc.Active(ctx)
		if err != nil {
			return fmt.Errorf("failed to load active plan: %w", err)
		}
		if plan == nil {
			return fmt.Errorf("no active plan; run 'contenox plan new <goal>'")
		}

		var nextStep *planstore.PlanStep
		for _, s := range steps {
			if s.Status == planstore.StepStatusPending {
				nextStep = s
				break
			}
		}
		if nextStep == nil {
			fmt.Fprintln(cmd.OutOrStdout(), "All steps complete. Plan is done!")
			return nil
		}

		fmt.Fprintf(cmd.OutOrStdout(), "\nExecuting Step %d: %s...\n", nextStep.Ordinal, nextStep.Description)

		// Delegate execution entirely to planservice — it handles DB updates and
		// markdown sync automatically.
		args := planservice.Args{WithShell: o.EffectiveEnableLocalExec, WithAuto: isAuto}
		result, _, execErr := planSvc.Next(execCtx, args, &chain)

		if execErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "\nStep failed: %v\n", execErr)
			if result != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "\nStep output:\n%s\n", result)
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "\nStep did not complete successfully.\n"+
				"  • contenox plan show          → see current status\n"+
				"  • contenox plan retry <N>     → retry this step\n"+
				"  • contenox plan replan        → regenerate remaining steps")
			return nil
		}

		fmt.Fprintf(cmd.OutOrStdout(), "✓ Step %d completed.\n", nextStep.Ordinal)
		if !isAuto {
			return nil
		}
	}
}

func runPlanRetry(cmd *cobra.Command, args []string) error {
	ctx, db, cDir, cleanup, err := openPlanDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	ordinal, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("invalid ordinal %q: must be a number", args[0])
	}

	// Use a nil engine — plan service doesn't need engine for Retry.
	planSvc := buildPlanService(db, nil, cDir)
	msg, err := planSvc.Retry(ctx, ordinal)
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), msg)
	return nil
}

func runPlanSkip(cmd *cobra.Command, args []string) error {
	ctx, db, cDir, cleanup, err := openPlanDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	ordinal, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("invalid ordinal %q: must be a number", args[0])
	}

	planSvc := buildPlanService(db, nil, cDir)
	msg, err := planSvc.Skip(ctx, ordinal)
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), msg)
	return nil
}

func runPlanReplan(cmd *cobra.Command, _ []string) error {
	ctx, db, cDir, cleanup, err := openPlanDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	o := buildPlanOpts(cmd, db, "")
	engine, err := BuildEngine(ctx, db, o)
	if err != nil {
		return err
	}
	defer engine.Stop()

	plannerPath, _, err := ensurePlanChains(cDir)
	if err != nil {
		return err
	}
	chainData, err := os.ReadFile(plannerPath)
	if err != nil {
		return err
	}
	var plannerChain taskengine.TaskChainDefinition
	if err := json.Unmarshal(chainData, &plannerChain); err != nil {
		return err
	}

	fmt.Fprintln(cmd.OutOrStdout(), "Generating new plan steps based on current progress...")

	planSvc := buildPlanService(db, engine, cDir)
	execCtx := execCtxForPlan(ctx, o, plannerChain.ID)

	newSteps, _, err := planSvc.Replan(execCtx, &plannerChain)
	if err != nil {
		return fmt.Errorf("replan failed: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Replanned with %d new steps. Use 'contenox plan show' to see them.\n", len(newSteps))
	return nil
}

func runPlanDelete(cmd *cobra.Command, args []string) error {
	ctx, db, cDir, cleanup, err := openPlanDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	// Look up plan name first so we can clean up the markdown file.
	exec := db.WithoutTransaction()
	store := planstore.New(exec)
	plan, err := store.GetPlanByName(ctx, args[0])
	if err != nil {
		return fmt.Errorf("plan %q not found: %w", args[0], err)
	}

	planSvc := buildPlanService(db, nil, cDir)
	if err := planSvc.Delete(ctx, args[0]); err != nil {
		return err
	}

	// Remove the markdown file if it exists.
	mdPath := filepath.Join(cDir, "plans", filepath.Base(plan.Name)+".md")
	_ = os.Remove(mdPath)

	// If this was the active plan in the KV pointer, clear it.
	activeID, _ := getActivePlanID(ctx, exec)
	if activeID == plan.ID {
		_ = withTransaction(ctx, db, func(tx libdbexec.Exec) error {
			return setActivePlanID(ctx, tx, "")
		})
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Deleted plan %q.\n", plan.Name)
	return nil
}

func runPlanClean(cmd *cobra.Command, _ []string) error {
	ctx, db, cDir, cleanup, err := openPlanDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	// Snapshot plans before deletion so we can remove local markdown files.
	exec := db.WithoutTransaction()
	store := planstore.New(exec)
	plans, err := store.ListPlans(ctx)
	if err != nil {
		return fmt.Errorf("failed to list plans: %w", err)
	}

	planSvc := buildPlanService(db, nil, cDir)
	deleted, err := planSvc.Clean(ctx)
	if err != nil {
		return err
	}

	// Remove local markdown files for plans that were just deleted.
	activeID, _ := getActivePlanID(ctx, exec)
	for _, p := range plans {
		if p.Status != planstore.PlanStatusCompleted && p.Status != planstore.PlanStatusArchived {
			continue
		}
		mdPath := filepath.Join(cDir, "plans", p.Name+".md")
		_ = os.Remove(mdPath)
		if p.ID == activeID {
			_ = withTransaction(ctx, db, func(tx libdbexec.Exec) error {
				return setActivePlanID(ctx, tx, "")
			})
		}
	}

	if deleted == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No completed or archived plans to clean up.")
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Deleted %d plan(s).\n", deleted)
	}
	return nil
}
