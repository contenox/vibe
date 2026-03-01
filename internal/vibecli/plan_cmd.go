package vibecli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/contenox/vibe/chatservice"
	"github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/planstore"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/contenox/vibe/taskengine"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Manage execution plans (new, list, show, next, retry, skip, replan, delete, clean).",

	SilenceUsage: true,
}

var planNewCmd = &cobra.Command{
	Use:   "new <goal>",
	Short: "Create a new execution plan.",
	Args:  cobra.ExactArgs(1),
	RunE:  runPlanNew,
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
	Short: "Execute the next pending step.",
	Args:  cobra.NoArgs,
	RunE:  runPlanNext,
}

var planRetryCmd = &cobra.Command{
	Use:   "retry <ordinal>",
	Short: "Reset a failed/skipped step to pending.",
	Args:  cobra.ExactArgs(1),
	RunE:  runPlanRetry,
}

var planSkipCmd = &cobra.Command{
	Use:   "skip <ordinal>",
	Short: "Mark a pending/failed step as skipped.",
	Args:  cobra.ExactArgs(1),
	RunE:  runPlanSkip,
}

var planReplanCmd = &cobra.Command{
	Use:   "replan",
	Short: "Regenerate remaining steps based on current context and failures.",
	Args:  cobra.NoArgs,
	RunE:  runPlanReplan,
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
	planNextCmd.Flags().Bool("auto", false, "Automatically continue to the next step until finished or failed")
}

// openPlanDB is similar to openSessionDB but for plans.
func openPlanDB(cmd *cobra.Command) (context.Context, libdbexec.DBManager, string, func(), error) {
	cfg, configPath, err := loadLocalConfig()
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("failed to load config: %w", err)
	}

	var contenoxDir string
	if configPath != "" {
		contenoxDir = filepath.Dir(configPath)
	} else {
		cwd, _ := os.Getwd()
		contenoxDir = filepath.Join(cwd, ".contenox")
	}

	flags := cmd.Root().Flags()
	effectiveDB, _ := flags.GetString("db")
	if effectiveDB == "" && cfg.DB != "" {
		effectiveDB = cfg.DB
	}
	if effectiveDB == "" {
		effectiveDB = filepath.Join(contenoxDir, "local.db")
	}

	dbPathAbs, err := filepath.Abs(effectiveDB)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("invalid database path: %w", err)
	}

	ctx := libtracker.WithNewRequestID(context.Background())
	db, err := libdbexec.NewSQLiteDBManager(ctx, dbPathAbs, runtimetypes.SchemaSQLite)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("failed to open database: %w", err)
	}
	cleanup := func() { _ = db.Close() }

	return ctx, db, contenoxDir, cleanup, nil
}

func buildPlanOpts(cmd *cobra.Command, input string) runOpts {
	cfg, _, _ := loadLocalConfig()
	flags := cmd.Root().Flags()

	effectiveOllama, _ := flags.GetString("ollama")
	if !flags.Changed("ollama") && cfg.Ollama != "" {
		effectiveOllama = cfg.Ollama
	}

	effectiveModel, _ := flags.GetString("model")
	if !flags.Changed("model") && cfg.Model != "" {
		effectiveModel = cfg.Model
	}

	effectiveContext, _ := flags.GetInt("context")
	if !flags.Changed("context") && cfg.Context != nil {
		effectiveContext = *cfg.Context
	}

	effectiveTracing, _ := flags.GetBool("trace")
	if !flags.Changed("trace") && cfg.Tracing != nil {

		effectiveTracing = *cfg.Tracing
	}

	effectiveEnableLocalExec := false
	if cfg.EnableLocalExec != nil {
		effectiveEnableLocalExec = *cfg.EnableLocalExec
	}
	if v, _ := flags.GetBool("enable-local-exec"); flags.Changed("enable-local-exec") {
		effectiveEnableLocalExec = v
	}

	effectiveLocalExecAllowedDir, _ := flags.GetString("local-exec-allowed-dir")
	if !flags.Changed("local-exec-allowed-dir") && cfg.LocalExecAllowedDir != "" {
		effectiveLocalExecAllowedDir = cfg.LocalExecAllowedDir
	}

	effectiveLocalExecAllowedCommands, _ := flags.GetString("local-exec-allowed-commands")
	if !flags.Changed("local-exec-allowed-commands") && cfg.LocalExecAllowedCommands != "" {
		effectiveLocalExecAllowedCommands = cfg.LocalExecAllowedCommands
	}

	effectiveLocalExecDeniedCommands := cfg.LocalExecDeniedCommands

	resolvedBackends, effectiveDefaultProvider, effectiveDefaultModel := resolveEffectiveBackends(cfg, effectiveOllama, effectiveModel)

	return runOpts{
		Cfg:                               cfg,
		InputFlagPassed:                   true,
		InputValue:                        input,
		EffectiveDefaultModel:             effectiveDefaultModel,
		EffectiveDefaultProvider:          effectiveDefaultProvider,
		EffectiveContext:                  effectiveContext,
		EffectiveEnableLocalExec:          effectiveEnableLocalExec,
		EffectiveLocalExecAllowedDir:      effectiveLocalExecAllowedDir,
		EffectiveLocalExecAllowedCommands: effectiveLocalExecAllowedCommands,
		EffectiveLocalExecDeniedCommands:  effectiveLocalExecDeniedCommands,
		EffectiveTracing:                  effectiveTracing,
		ResolvedBackends:                  resolvedBackends,
	}
}

func runPlanNew(cmd *cobra.Command, args []string) error {
	goal := args[0]
	ctx, db, cDir, cleanup, err := openPlanDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	// 1. Generate name
	name := planNameFromGoal(goal, uuid.New().String()[:8])

	fmt.Printf("Generating plan for: %s...\n", goal)

	// 2. Build the task engine
	o := buildPlanOpts(cmd, goal)
	engine, err := BuildEngine(ctx, db, o)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}
	defer engine.Stop()

	// 3. Ensure and load the planner chain
	plannerPath, _, err := ensurePlanChains(cDir)
	if err != nil {
		return fmt.Errorf("failed to ensure plan chains: %w", err)
	}

	chainData, err := os.ReadFile(plannerPath)
	if err != nil {
		return fmt.Errorf("failed to read planner chain: %w", err)
	}

	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal(chainData, &chain); err != nil {
		return fmt.Errorf("failed to parse planner chain: %w", err)
	}
	if err := validatePlannerChain(&chain, plannerPath); err != nil {
		return err
	}

	// 4. Execute the planner chain
	templateVars := map[string]string{
		"model":    o.EffectiveDefaultModel,
		"provider": o.EffectiveDefaultProvider,
		"chain":    chain.ID,
	}
	execCtx := taskengine.WithTemplateVars(ctx, templateVars)

	userMsg := taskengine.Message{Role: "user", Content: goal, Timestamp: time.Now()}
	chainInput := taskengine.ChatHistory{Messages: []taskengine.Message{userMsg}}

	output, outputType, _, err := engine.TaskService.Execute(execCtx, &chain, chainInput, taskengine.DataTypeChatHistory)
	if err != nil {
		return fmt.Errorf("planner chain execution failed: %w", err)
	}

	// 5. Parse the JSON result
	var planJSON struct {
		Steps []struct {
			Description string `json:"description"`
		} `json:"steps"`
	}

	success := false
	if outputType == taskengine.DataTypeChatHistory {
		if hist, ok := output.(taskengine.ChatHistory); ok && len(hist.Messages) > 0 {
			lastMsg := hist.Messages[len(hist.Messages)-1].Content
			// Strip markdown code block
			lastMsg = strings.TrimPrefix(lastMsg, "```json\n")
			lastMsg = strings.TrimPrefix(lastMsg, "```\n")
			lastMsg = strings.TrimSuffix(lastMsg, "\n```")
			if err := json.Unmarshal([]byte(lastMsg), &planJSON); err == nil {
				success = true
			} else {
				slog.Error("Failed to parse LLM json output", "output", lastMsg, "error", err)
			}
		}
	}
	if !success {
		return fmt.Errorf("failed to get a valid JSON response from the planner")
	}

	// 6. Save to DB
	txExec, commit, release, txErr := db.WithTransaction(ctx)
	if txErr != nil {
		return txErr
	}
	defer release()

	store := planstore.New(txExec)
	planID := uuid.New().String()

	plan := &planstore.Plan{
		ID:   planID,
		Name: name,
		Goal: goal,
	}
	if err := store.CreatePlan(ctx, plan); err != nil {
		return fmt.Errorf("failed to create plan: %w", err)
	}

	for i, st := range planJSON.Steps {
		step := &planstore.PlanStep{
			ID:          uuid.New().String(),
			PlanID:      planID,
			Ordinal:     i + 1,
			Description: st.Description,
		}
		if err := store.CreatePlanSteps(ctx, step); err != nil {
			return fmt.Errorf("failed to save step: %w", err)
		}
	}

	if err := setActivePlanID(ctx, txExec, planID); err != nil {
		return fmt.Errorf("failed to set active plan pointer: %w", err)
	}

	if err := syncPlanMarkdown(ctx, txExec, planID, cDir); err != nil {
		fmt.Printf("Warning: failed to sync markdown: %v\n", err)
	}

	if err := commit(ctx); err != nil {
		return fmt.Errorf("failed to commit: %w", err)
	}

	fmt.Printf("Created plan %q with %d steps. Now active.\n", name, len(planJSON.Steps))
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
		fmt.Println("No plans yet. Run: vibe plan new <goal>")
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

		fmt.Printf("%s%-20s [%d/%d] %s\n", prefix, p.Name, completed, len(steps), p.Status)
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
		return fmt.Errorf("no active plan; run 'vibe plan new' or 'vibe plan switch'")
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

	fmt.Printf("Plan: %s (active) — %d/%d complete\n", plan.Name, completed, len(steps))
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
		fmt.Printf("%d. %s %s\n", s.Ordinal, checkbox, s.Description)
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

	o := buildPlanOpts(cmd, "")
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

	templateVars := map[string]string{
		"model":    o.EffectiveDefaultModel,
		"provider": o.EffectiveDefaultProvider,
		"chain":    chain.ID,
	}
	execCtx := taskengine.WithTemplateVars(ctx, templateVars)

	// execution loop
	for {
		exec := db.WithoutTransaction()
		activeID, err := getActivePlanID(ctx, exec)
		if err != nil || activeID == "" {
			return fmt.Errorf("no active plan")
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

		var nextStep *planstore.PlanStep
		for _, s := range steps {
			if s.Status == planstore.StepStatusPending {
				nextStep = s
				break
			}
		}

		if nextStep == nil {
			_ = planstore.New(exec).UpdatePlanStatus(ctx, activeID, planstore.PlanStatusCompleted)
			_ = syncPlanMarkdown(ctx, exec, activeID, cDir)
			fmt.Println("No pending steps. Plan is complete!")
			return nil
		}

		fmt.Printf("\nExecuting Step %d: %s...\n", nextStep.Ordinal, nextStep.Description)

		sessionID := "plan-" + activeID
		chatMgr := chatservice.NewManager(nil)
		history, _ := chatMgr.ListMessages(ctx, exec, sessionID)

		prompt := fmt.Sprintf("Overall Goal: %s\n\nExecute Step %d: %s\n\nUse your local_shell tools to accomplish this. Once you have fully completed and verified this step, output exactly `===STEP_DONE===` on a new line.", plan.Goal, nextStep.Ordinal, nextStep.Description)
		userMsg := taskengine.Message{Role: "user", Content: prompt, Timestamp: time.Now()}
		chainInput := taskengine.ChatHistory{Messages: append(history, userMsg)}

		output, _, _, err := engine.TaskService.Execute(execCtx, &chain, chainInput, taskengine.DataTypeChatHistory)

		finalStatus := planstore.StepStatusFailed
		finalResult := "Execution failed or stopped prematurely."

		isEmptyContentErr := err != nil && strings.Contains(err.Error(), "empty content from model")

		if err == nil || isEmptyContentErr {
			if updatedHistory, ok := output.(taskengine.ChatHistory); ok {
				// Persist chat diff
				txExec, commit, release, txErr := db.WithTransaction(ctx)
				if txErr == nil {
					_ = chatMgr.PersistDiff(ctx, txExec, sessionID, updatedHistory.Messages)
					_ = commit(ctx)
					release()
				}

				if len(updatedHistory.Messages) > 0 {
					lastMsg := updatedHistory.Messages[len(updatedHistory.Messages)-1].Content
					finalResult = lastMsg
					if strings.Contains(lastMsg, "===STEP_DONE===") {
						finalStatus = planstore.StepStatusCompleted
					}
				}
			}
		}

		if err := store.UpdatePlanStepStatus(ctx, nextStep.ID, finalStatus, finalResult); err != nil {
			return fmt.Errorf("failed to update step status: %w", err)
		}

		_ = syncPlanMarkdown(ctx, exec, activeID, cDir)

		if finalStatus != planstore.StepStatusCompleted {
			fmt.Println("\nStep execution stopped or failed. Please review the output above.")
			break
		}

		fmt.Printf("\nStep %d completed successfully.\n", nextStep.Ordinal)
		if !isAuto {
			break
		}
	}

	return nil
}

func runPlanRetry(cmd *cobra.Command, args []string) error {
	ctx, db, cDir, cleanup, err := openPlanDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	return updateStepStatusByOrdinal(ctx, db, cDir, args[0], planstore.StepStatusPending, "")
}

func runPlanSkip(cmd *cobra.Command, args []string) error {
	ctx, db, cDir, cleanup, err := openPlanDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	return updateStepStatusByOrdinal(ctx, db, cDir, args[0], planstore.StepStatusSkipped, "Skipped by user")
}

func updateStepStatusByOrdinal(ctx context.Context, db libdbexec.DBManager, cDir string, ordinalStr string, newStatus planstore.StepStatus, reason string) error {
	exec := db.WithoutTransaction()
	activeID, err := getActivePlanID(ctx, exec)
	if err != nil || activeID == "" {
		return fmt.Errorf("no active plan")
	}

	var ordinal int
	if _, err := fmt.Sscanf(ordinalStr, "%d", &ordinal); err != nil {
		return fmt.Errorf("invalid ordinal: %s", ordinalStr)
	}

	store := planstore.New(exec)
	steps, err := store.ListPlanSteps(ctx, activeID)
	if err != nil {
		return err
	}

	var targetStep *planstore.PlanStep
	for _, s := range steps {
		if s.Ordinal == ordinal {
			targetStep = s
			break
		}
	}

	if targetStep == nil {
		return fmt.Errorf("step %d not found in active plan", ordinal)
	}

	if err := store.UpdatePlanStepStatus(ctx, targetStep.ID, newStatus, reason); err != nil {
		return fmt.Errorf("failed to update step: %w", err)
	}

	_ = syncPlanMarkdown(ctx, exec, activeID, cDir)
	fmt.Printf("Step %d updated to %s.\n", ordinal, newStatus)
	return nil
}

func runPlanReplan(cmd *cobra.Command, _ []string) error {
	ctx, db, cDir, cleanup, err := openPlanDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	exec := db.WithoutTransaction()
	activeID, err := getActivePlanID(ctx, exec)
	if err != nil || activeID == "" {
		return fmt.Errorf("no active plan")
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

	var completedStr string
	maxOrdinal := 0
	for _, s := range steps {
		if s.Ordinal > maxOrdinal {
			maxOrdinal = s.Ordinal
		}
		if s.Status != planstore.StepStatusPending {
			completedStr += fmt.Sprintf("Step %d: %s (Status: %s, Result: %s)\n", s.Ordinal, s.Description, s.Status, s.ExecutionResult)
		}
	}

	goalPrompt := fmt.Sprintf("Goal: %s\n\nWhat we have done so far:\n%s\nSome steps failed or we need to replan. Please generate a NEW list of logical remaining steps to finish the goal. DO NOT INCLUDE ALREADY COMPLETED OR SKIPPED STEPS.", plan.Goal, completedStr)

	fmt.Println("Generating new plan steps based on current progress...")

	o := buildPlanOpts(cmd, goalPrompt)
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

	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal(chainData, &chain); err != nil {
		return err
	}

	templateVars := map[string]string{
		"model":    o.EffectiveDefaultModel,
		"provider": o.EffectiveDefaultProvider,
		"chain":    chain.ID,
	}
	execCtx := taskengine.WithTemplateVars(ctx, templateVars)

	userMsg := taskengine.Message{Role: "user", Content: goalPrompt, Timestamp: time.Now()}
	chainInput := taskengine.ChatHistory{Messages: []taskengine.Message{userMsg}}

	output, outputType, _, err := engine.TaskService.Execute(execCtx, &chain, chainInput, taskengine.DataTypeChatHistory)
	if err != nil {
		return err
	}

	var planJSON struct {
		Steps []struct {
			Description string `json:"description"`
		} `json:"steps"`
	}

	success := false
	if outputType == taskengine.DataTypeChatHistory {
		if hist, ok := output.(taskengine.ChatHistory); ok && len(hist.Messages) > 0 {
			lastMsg := hist.Messages[len(hist.Messages)-1].Content
			lastMsg = strings.TrimPrefix(lastMsg, "```json\n")
			lastMsg = strings.TrimPrefix(lastMsg, "```\n")
			lastMsg = strings.TrimSuffix(lastMsg, "\n```")
			if err := json.Unmarshal([]byte(lastMsg), &planJSON); err == nil {
				success = true
			} else {
				slog.Error("Failed to parse LLM json output", "error", err, "output", lastMsg)
			}
		}
	}
	if !success {
		return fmt.Errorf("failed to get a valid JSON response from the planner")
	}

	txExec, commit, release, txErr := db.WithTransaction(ctx)
	if txErr != nil {
		return txErr
	}
	defer release()

	txStore := planstore.New(txExec)

	if err := txStore.DeletePendingPlanSteps(ctx, activeID); err != nil {
		return err
	}

	currentOrdinal := maxOrdinal + 1
	for _, st := range planJSON.Steps {
		step := &planstore.PlanStep{
			ID:          uuid.New().String(),
			PlanID:      activeID,
			Ordinal:     currentOrdinal,
			Description: st.Description,
		}
		if err := txStore.CreatePlanSteps(ctx, step); err != nil {
			return err
		}
		currentOrdinal++
	}

	if err := syncPlanMarkdown(ctx, txExec, activeID, cDir); err != nil {
		fmt.Printf("Warning: failed to sync markdown: %v\n", err)
	}

	if err := commit(ctx); err != nil {
		return err
	}

	fmt.Printf("Replanned with %d new steps. Use 'vibe plan show' to see them.\n", len(planJSON.Steps))
	return nil
}

func runPlanDelete(cmd *cobra.Command, args []string) error {
	ctx, db, cDir, cleanup, err := openPlanDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	exec, commit, release, err := db.WithTransaction(ctx)
	if err != nil {
		return err
	}
	defer release()

	store := planstore.New(exec)
	plan, err := store.GetPlanByName(ctx, args[0])
	if err != nil {
		return fmt.Errorf("plan %q not found: %w", args[0], err)
	}

	if err := store.DeletePlan(ctx, plan.ID); err != nil {
		return fmt.Errorf("failed to delete plan: %w", err)
	}

	// Remove the markdown file if it exists.
	mdPath := filepath.Join(cDir, "plans", plan.Name+".md")
	_ = os.Remove(mdPath)

	// If this was the active plan, clear the pointer.
	activeID, _ := getActivePlanID(ctx, exec)
	if activeID == plan.ID {
		_ = setActivePlanID(ctx, exec, "")
	}

	if err := commit(ctx); err != nil {
		return err
	}

	fmt.Printf("Deleted plan %q.\n", plan.Name)
	return nil
}

func runPlanClean(cmd *cobra.Command, args []string) error {
	ctx, db, cDir, cleanup, err := openPlanDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	exec, commit, release, err := db.WithTransaction(ctx)
	if err != nil {
		return err
	}
	defer release()

	store := planstore.New(exec)
	plans, err := store.ListPlans(ctx)
	if err != nil {
		return fmt.Errorf("failed to list plans: %w", err)
	}

	activeID, _ := getActivePlanID(ctx, exec)
	deleted := 0
	for _, p := range plans {
		if p.Status != planstore.PlanStatusCompleted && p.Status != planstore.PlanStatusArchived {
			continue
		}
		if err := store.DeletePlan(ctx, p.ID); err != nil {
			fmt.Printf("Warning: failed to delete plan %q: %v\n", p.Name, err)
			continue
		}
		mdPath := filepath.Join(cDir, "plans", p.Name+".md")
		_ = os.Remove(mdPath)
		if p.ID == activeID {
			_ = setActivePlanID(ctx, exec, "")
		}
		deleted++
	}

	if err := commit(ctx); err != nil {
		return err
	}

	if deleted == 0 {
		fmt.Println("No completed or archived plans to clean up.")
	} else {
		fmt.Printf("Deleted %d plan(s).\n", deleted)
	}
	return nil
}
