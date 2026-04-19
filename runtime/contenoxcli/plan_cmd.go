package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/contenox/contenox/runtime/execservice"
	libdbexec "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/planservice"
	"github.com/contenox/contenox/runtime/planstore"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/contenox/contenox/runtime/vfsservice"
	"github.com/spf13/cobra"
)

// planExecDefaultTimeout is used for plan subcommands when the user does not pass
// contenox --timeout (the global default is short for one-shot runs).
const planExecDefaultTimeout = 30 * time.Minute

// planCommandTimeout returns the execution budget for plan CLI operations. If the user
// set --timeout on the root command, that value is used; otherwise planExecDefaultTimeout
// applies instead of the global defaultTimeout (5m).
func planCommandTimeout(cmd *cobra.Command) time.Duration {
	t, _ := cmd.Root().Flags().GetDuration("timeout")
	if cmd.Root().Flags().Changed("timeout") {
		return t
	}
	return planExecDefaultTimeout
}

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

Long runs (monitoring):
  Before: contenox doctor — confirm backend, API keys, and default model/provider.
  During: use contenox --trace plan next … for telemetry on stderr; log the session (e.g. tee plan.log).
  Timeouts: plan subcommands default to 30m per invocation unless you set contenox --timeout (e.g. 2h for huge repos).
  Shell + FS: use plan next --shell and contenox --local-exec-allowed-dir <project-root> so local_shell
  and local_fs policies match your tree.

Chain JSON (under the resolved .contenox directory):
  chain-planner.json              — planner for 'plan new' (must return a JSON array of step strings)
  chain-step-executor.json        — default executor for 'plan next' (chat ↔ tools loop)
  chain-step-executor-gated.json — optional post-tool LLM gate (plan next --gate); uses {{var:gate_model}}
  chain-step-summarizer.json      — per-step summary into planstore
Plan step seeds also receive {{var:execution_context}} (engine boundary) and {{var:gate_model}} when used.
HITL (human-in-the-loop): use plan next --hitl to require terminal approval before write_file, sed, and
local_shell calls. Beam enables HITL by default; set CONTENOX_HITL_ENABLED=false to disable it.
'contenox init' writes these files; any plan subcommand refreshes them from built-in defaults.
To customize them permanently, change the embedded chain definitions in the contenox source tree
and rebuild, or maintain a fork. Set model/provider via --model, --provider, and
'contenox config set default-model' so {{var:model}} / {{var:provider}} resolve.

Note: plan execution requires a model that supports tool calling.
The active plan is tracked automatically; use 'contenox plan list' to see all plans.`,
	SilenceUsage: true,
}

var planNewCmd = &cobra.Command{
	Use:   "new <goal>",
	Short: "Create a new execution plan from a goal description.",
	Long: `Ask the LLM to break a goal into ordered steps and save the plan to SQLite.

The new plan becomes the active plan only after generation succeeds. If the planner fails
(OpenAI quota, network, malformed JSON, etc.), the previous active plan is left unchanged.

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
	Long: `Run the next pending step using the LLM step-executor chain (.contenox/chain-step-executor.json),
or chain-step-executor-gated.json when you pass --gate.

Each step runs one agentic subgraph: chat_completion alternates with execute_tool_calls (same
engine pattern as chain-contenox) until the assistant responds without tool calls, then the subgraph
ends. With --gate, a small model ({{var:gate_model}}, defaulting to the main model) scores each
tool round before the next chat turn; non-zero aborts the step with a clear error.
The step is marked completed when execution succeeds; use ===STEP_DONE=== in the prompt so
the model signals completion.

{{var:model}}, {{var:provider}}, {{var:summarizer_model}}, {{var:gate_model}}, and {{var:execution_context}}
are merged for the compiled seed and chains. See 'contenox plan' help for chain file paths.

Use contenox --trace plan next … to stream step telemetry to stderr. For a long unattended run,
log output: contenox plan next --auto --shell … 2>&1 | tee plan-run.log   (30m default; add --timeout 2h if needed)

Flags:
  --auto     Continue executing steps until the plan is done or a step fails
  --shell    Enable the local_shell hook so the model can run commands
  --gate     Use gated executor (post-tool LLM gate; extra cost/latency)
  --hitl     Pause before write_file, sed, and local_shell calls; require y/n approval in the terminal

Examples:
  contenox plan next
  contenox plan next --shell             # single step with shell access
  contenox plan next --auto --shell      # run everything until done
  contenox plan next --shell --gate      # post-tool LLM gate after each tool round
  contenox plan next --shell --hitl      # human approval before each write/shell tool call`,
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

var planExploreCmd = &cobra.Command{
	Use:   "explore",
	Short: "Run the read-only explorer to populate the active plan's RepoContext seed.",
	Long: `Run chain-plan-explorer.json against the workspace and persist the produced
RepoContext (typed JSON: languages, entry_points, build/test commands, conventions, key files)
on the active plan.

The RepoContext is rendered into every step's seed prompt as {{var:repo_context}},
so steps see concrete file paths and conventions instead of cold-exploring on every run.

The explorer is read-only by contract: only local_fs (and other read-only hooks) are
allowlisted, and contenox plan explore validates this before running the chain.

Example:
  contenox plan explore                # explore for the active plan
  contenox plan new --explore "..."    # explore as part of plan creation`,
	Args: cobra.NoArgs,
	RunE: runPlanExplore,
}

func init() {
	planCmd.AddCommand(planNewCmd, planListCmd, planShowCmd, planNextCmd, planRetryCmd, planSkipCmd, planReplanCmd, planDeleteCmd, planCleanCmd, planExploreCmd)
	planNextCmd.Flags().Bool("auto", false, "Continue executing steps automatically until the plan is done or a step fails")
	planNextCmd.Flags().Bool("shell", false, "Enable the local_shell hook for this plan step (required for shell-based tasks)")
	planNextCmd.Flags().Bool("gate", false, "Use chain-step-executor-gated.json: after each tool round, a small model scores whether to continue (extra latency/cost; aborts bad/corrupt tool output)")
	planNextCmd.Flags().Bool("hitl", false, "Pause before each write/shell tool call and require y/n approval in the terminal (human-in-the-loop)")
	planNewCmd.Flags().Bool("explore", false, "Also run 'plan explore' on the new plan to seed it with a RepoContext")
}

// openPlanDB is similar to openSessionDB but for plans.
func openPlanDB(cmd *cobra.Command) (context.Context, libdbexec.DBManager, string, func(), error) {
	contenoxDir, err := ResolveContenoxDir(cmd)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}

	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("invalid database path: %w", err)
	}

	baseCtx := cmd.Context()
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	reqCtx := libtracker.WithNewRequestID(baseCtx)
	timeoutCtx, cancel := context.WithTimeout(reqCtx, planCommandTimeout(cmd))
	ctx, stop := signal.NotifyContext(timeoutCtx, syscall.SIGINT, syscall.SIGTERM)

	db, err := OpenDBAt(ctx, dbPath)
	if err != nil {
		stop()
		cancel()
		return nil, nil, "", nil, fmt.Errorf("failed to open database: %w", err)
	}
	cleanup := func() {
		stop()
		cancel()
		_ = db.Close()
	}

	return ctx, db, contenoxDir, cleanup, nil
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

	// Also check the subcommand's own local flags (e.g. plan next --shell, --hitl).
	effectiveHITL := false
	if localFlags := cmd.Flags(); localFlags != flags {
		if v, _ := localFlags.GetBool("shell"); localFlags.Changed("shell") {
			effectiveEnableLocalExec = v
		}
		if v, _ := localFlags.GetBool("hitl"); localFlags.Changed("hitl") {
			effectiveHITL = v
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
		EffectiveHITL:                effectiveHITL,
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
	if len(args) > 0 {
		goal = args[0]
	}
	stdinData, ok, err := readStdinIfAvailable(maxCLIStdinBytes)
	if err != nil {
		return err
	}
	if ok {
		stdinStr := strings.TrimSpace(stdinData)
		if stdinStr != "" {
			if goal != "" {
				goal = goal + "\n\n" + stdinStr
			} else {
				goal = stdinStr
			}
		}
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

	if err := PreflightLLMSetup(cmd.ErrOrStderr(), engine.SetupCheck); err != nil {
		return err
	}

	stopTaskEvents := startCLITaskEventStream(ctx, engine, cmd.ErrOrStderr(), cliTaskEventRenderOptions{
		Trace:        o.EffectiveTracing,
		ShowThinking: true,
	})
	defer stopTaskEvents()

	plannerPath, _, _, err := ensurePlanChains(cDir)
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

	if explore, _ := cmd.Flags().GetBool("explore"); explore {
		if err := runExplorerOnPlan(cmd, ctx, db, cDir, engine, o, plan.ID); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "explore failed: %v\n", err)
			return nil
		}
	}
	return nil
}

// runExplorerOnPlan is the shared implementation used by 'plan explore' and by
// 'plan new --explore'. It loads chain-plan-explorer.json from the resolved
// .contenox dir, validates it, and calls planservice.Explore against planID
// (empty = active plan).
func runExplorerOnPlan(
	cmd *cobra.Command,
	ctx context.Context,
	db libdbexec.DBManager,
	cDir string,
	engine *Engine,
	o chatOpts,
	planID string,
) error {
	if _, _, _, err := ensurePlanChains(cDir); err != nil {
		return fmt.Errorf("failed to ensure plan chains: %w", err)
	}
	explorerPath := resolvePlanExplorerPath(cDir)
	chainData, err := os.ReadFile(explorerPath)
	if err != nil {
		return fmt.Errorf("failed to read explorer chain: %w", err)
	}
	var explorerChain taskengine.TaskChainDefinition
	if err := json.Unmarshal(chainData, &explorerChain); err != nil {
		return fmt.Errorf("failed to parse explorer chain: %w", err)
	}
	if err := validatePlanExplorerChain(&explorerChain, explorerPath); err != nil {
		return err
	}

	planSvc := buildPlanService(db, engine, cDir)
	execCtx := execCtxForPlan(ctx, o, explorerChain.ID)

	fmt.Fprintln(cmd.OutOrStdout(), "Exploring workspace (read-only)...")
	rc, err := planSvc.Explore(execCtx, planID, &explorerChain)
	if err != nil {
		return fmt.Errorf("explore failed: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "RepoContext seeded: languages=%v key_files=%d build_commands=%d test_commands=%d\n",
		rc.Languages, len(rc.KeyFiles), len(rc.BuildCommands), len(rc.TestCommands))
	return nil
}

func runPlanExplore(cmd *cobra.Command, _ []string) error {
	ctx, db, cDir, cleanup, err := openPlanDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	o := buildPlanOpts(cmd, db, "")
	engine, err := BuildEngine(ctx, db, o)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}
	defer engine.Stop()

	if err := PreflightLLMSetup(cmd.ErrOrStderr(), engine.SetupCheck); err != nil {
		return err
	}
	stopTaskEvents := startCLITaskEventStream(ctx, engine, cmd.ErrOrStderr(), cliTaskEventRenderOptions{
		Trace:        o.EffectiveTracing,
		ShowThinking: true,
	})
	defer stopTaskEvents()

	return runExplorerOnPlan(cmd, ctx, db, cDir, engine, o, "")
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
		return fmt.Errorf("no active plan; run 'contenox plan new <goal>' (or 'contenox plan list' to see existing plans)")
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

	if err := PreflightLLMSetup(cmd.ErrOrStderr(), engine.SetupCheck); err != nil {
		return err
	}

	stopTaskEvents := startCLITaskEventStream(ctx, engine, cmd.ErrOrStderr(), cliTaskEventRenderOptions{
		Trace:        o.EffectiveTracing,
		ShowThinking: true,
	})
	defer stopTaskEvents()

	plannerPath, defaultExecutorPath, summarizerPath, err := ensurePlanChains(cDir)
	if err != nil {
		return err
	}
	useGate, _ := cmd.Flags().GetBool("gate")
	executorPath := defaultExecutorPath
	if useGate {
		executorPath = filepath.Join(cDir, "chain-step-executor-gated.json")
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
	sumData, err := os.ReadFile(summarizerPath)
	if err != nil {
		return err
	}
	var sumChain taskengine.TaskChainDefinition
	if err := json.Unmarshal(sumData, &sumChain); err != nil {
		return err
	}
	if err := validateSummarizerChain(&sumChain, summarizerPath); err != nil {
		return err
	}
	// Lazily-loaded planner chain for auto-replan-on-capacity. Only parsed
	// when a capacity-class failure actually triggers a replan.
	loadPlannerChain := func() (*taskengine.TaskChainDefinition, error) {
		raw, rerr := os.ReadFile(plannerPath)
		if rerr != nil {
			return nil, rerr
		}
		var pc taskengine.TaskChainDefinition
		if jerr := json.Unmarshal(raw, &pc); jerr != nil {
			return nil, jerr
		}
		if verr := validatePlannerChain(&pc, plannerPath); verr != nil {
			return nil, verr
		}
		return &pc, nil
	}

	planSvc := buildPlanService(db, engine, cDir)
	execCtx := execCtxForPlan(ctx, o, chain.ID)

	// After the last step, planservice marks the plan completed (status != active), so
	// Active() returns nil — same as "never had a plan". Track that we ran at least one
	// successful Next so --auto can exit successfully instead of "no active plan".
	ranStepOK := false
	// autoReplannedOrdinals caps auto-replan-on-capacity to one attempt per
	// step ordinal so a persistently-too-big step cannot loop forever.
	autoReplannedOrdinals := map[int]bool{}

	for {
		// Peek at the next pending step for display before execution.
		plan, steps, err := planSvc.Active(ctx)
		if err != nil {
			return fmt.Errorf("failed to load active plan: %w", err)
		}
		if plan == nil {
			if isAuto && ranStepOK {
				fmt.Fprintln(cmd.OutOrStdout(), "All steps complete. Plan is done!")
				return nil
			}
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
		result, _, execErr := planSvc.Next(execCtx, args, &chain, &sumChain)

		if execErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "\nStep failed: %v\n", execErr)
			if result != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "\nStep output:\n%s\n", result)
			}
			// Auto-replan on capacity failure: when a step blew its context /
			// token budget under --auto, ask the planner to split it into
			// smaller substeps once. Limited to one attempt per ordinal to
			// prevent oscillation. Other failure classes surface to the user
			// because they are not a "step too big" problem.
			if isAuto && !autoReplannedOrdinals[nextStep.Ordinal] {
				_, freshSteps, aerr := planSvc.Active(ctx)
				if aerr == nil {
					var failed *planstore.PlanStep
					for _, s := range freshSteps {
						if s.ID == nextStep.ID {
							failed = s
							break
						}
					}
					if failed != nil && failed.FailureClass == planstore.FailureClassCapacity {
						pc, perr := loadPlannerChain()
						if perr != nil {
							fmt.Fprintf(cmd.ErrOrStderr(), "auto-replan: planner chain unavailable: %v\n", perr)
						} else {
							fmt.Fprintf(cmd.OutOrStdout(), "Auto-replan: step %d failed for capacity reasons; asking planner to split it.\n", nextStep.Ordinal)
							autoReplannedOrdinals[nextStep.Ordinal] = true
							replanCtx := execCtxForPlan(ctx, o, pc.ID)
							_, _, rerr := planSvc.ReplanScoped(replanCtx, planservice.ReplanScope{
								OnlyOrdinal: nextStep.Ordinal,
								Hint:        "Split this step into 2-5 smaller substeps. The previous attempt exceeded the model context window.",
							}, pc)
							if rerr != nil {
								fmt.Fprintf(cmd.ErrOrStderr(), "auto-replan failed: %v\n", rerr)
							} else {
								// New substeps appended; resume the loop so the
								// next iteration picks them up as pending work.
								continue
							}
						}
					}
				}
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "\nStep did not complete successfully.\n"+
				"  • contenox plan show          → see current status\n"+
				"  • contenox plan retry <N>     → retry this step\n"+
				"  • contenox plan replan        → regenerate remaining steps")
			return nil
		}

		ranStepOK = true
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

	if err := PreflightLLMSetup(cmd.ErrOrStderr(), engine.SetupCheck); err != nil {
		return err
	}

	stopTaskEvents := startCLITaskEventStream(ctx, engine, cmd.ErrOrStderr(), cliTaskEventRenderOptions{
		Trace:        o.EffectiveTracing,
		ShowThinking: true,
	})
	defer stopTaskEvents()

	plannerPath, _, _, err := ensurePlanChains(cDir)
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
