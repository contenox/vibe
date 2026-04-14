// Package planservice manages AI-generated execution plans.
// Each plan is a named, ordered list of steps created from a free-text goal.
//
// Chains are passed per-call so the caller decides which planner / executor
// to use at runtime (selected by the API client).
package planservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/contenox/contenox/execservice"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/plancompile"
	"github.com/contenox/contenox/planstore"
	"github.com/contenox/contenox/taskengine"
	"github.com/contenox/contenox/taskengine/llmretry"
	"github.com/contenox/contenox/vfsservice"
	"github.com/google/uuid"
)

// Service is the contract for managing plans.
type Service interface {
	// New generates a plan from goal using plannerChain, saves it as active.
	New(ctx context.Context, goal string, plannerChain *taskengine.TaskChainDefinition) (*planstore.Plan, []*planstore.PlanStep, string, error)

	// Replan replaces remaining pending steps using plannerChain.
	Replan(ctx context.Context, plannerChain *taskengine.TaskChainDefinition) ([]*planstore.PlanStep, string, error)

	// ReplanScoped is the generalized form of Replan: see [ReplanScope].
	// Setting OnlyOrdinal>0 replaces only that step with substeps (the
	// targeted step is marked skipped for audit; substeps are appended).
	ReplanScoped(ctx context.Context, scope ReplanScope, plannerChain *taskengine.TaskChainDefinition) ([]*planstore.PlanStep, string, error)

	// Next executes the next pending step using executorChain + summarizerChain.
	// The summarizer runs graph-natively as the post-executor subgraph compiled
	// by plancompile.Compile; it produces the typed JSON handover that the next
	// step reads via {{var:previous_output}} / {{var:previous_handover}} / etc.
	Next(ctx context.Context, args Args, executorChain, summarizerChain *taskengine.TaskChainDefinition) (string, string, error)

	// Retry puts a failed/skipped step back to pending (ordinal is 1-based).
	Retry(ctx context.Context, ordinal int) (string, error)

	// Skip marks a step as intentionally bypassed (ordinal is 1-based).
	Skip(ctx context.Context, ordinal int) (string, error)

	// Active returns the current active plan and its steps.
	Active(ctx context.Context) (*planstore.Plan, []*planstore.PlanStep, error)

	// Show returns the active plan rendered as Markdown.
	Show(ctx context.Context) (string, error)

	// List returns all plans oldest-first.
	List(ctx context.Context) ([]*planstore.Plan, error)

	// SetActive makes the named plan active (archives the previous active one).
	SetActive(ctx context.Context, planName string) error

	// Delete permanently removes a plan by name.
	Delete(ctx context.Context, planName string) error

	// Clean removes all completed or archived plans; returns count removed.
	Clean(ctx context.Context) (int, error)

	// Explore runs explorerChain in read-only mode against the workspace and
	// persists a typed [planstore.RepoContext] on the active plan (or on
	// planID when non-empty). The returned RepoContext is also rendered into
	// future step seed prompts via {{var:repo_context}}.
	Explore(ctx context.Context, planID string, explorerChain *taskengine.TaskChainDefinition) (*planstore.RepoContext, error)
}

// Args controls Next execution behaviour.
//
// WithShell is a hard gate enforced inside the task engine: when false, Next
// attaches a runtime hook allowlist ["*", "!local_shell"] to the execution
// context, so resolveHookNames will remove local_shell from whatever the
// executor chain declared. A chain JSON that tries to invoke local_shell when
// the caller disallowed it fails at hook-dispatch time, regardless of what the
// chain author wrote. The flag can only further restrict; when true, the
// chain's own allowlist governs.
//
// WithAuto is a telemetry/audit signal only: the caller claims this step is
// part of an unattended auto-run loop. It is NOT an execution gate — iteration
// is a client-loop concern (see contenoxcli runPlanNext). The flag is forwarded
// to the tracker so plan-run logs/metrics can distinguish interactive vs.
// auto-run steps.
type Args struct {
	WithShell bool
	WithAuto  bool
}

type service struct {
	db     libdb.DBManager
	engine execservice.TasksEnvService
	vfs    vfsservice.Service
}

// New creates a Service. vfs may be nil (plan markdown writing is skipped).
func New(db libdb.DBManager, engine execservice.TasksEnvService, vfs vfsservice.Service) Service {
	return &service{db: db, engine: engine, vfs: vfs}
}

var _ Service = (*service)(nil)

func (s *service) activePlan(ctx context.Context) (*planstore.Plan, []*planstore.PlanStep, error) {
	st := planstore.New(s.db.WithoutTransaction())
	plan, err := st.GetActivePlan(ctx)
	if errors.Is(err, planstore.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	steps, err := st.ListPlanSteps(ctx, plan.ID)
	if err != nil {
		return nil, nil, err
	}
	return plan, steps, nil
}

// Explore runs the read-only explorer chain and persists a typed RepoContext
// on the target plan (active when planID is empty). Implements [Service.Explore].
//
// The chain itself is responsible for restricting tools to read-only via its
// own execute_config.hooks (see chain-plan-explorer.json + validatePlanExplorerChain).
// On success the RepoContext is also returned for the caller to display.
func (s *service) Explore(ctx context.Context, planID string, explorerChain *taskengine.TaskChainDefinition) (*planstore.RepoContext, error) {
	if explorerChain == nil {
		return nil, fmt.Errorf("explorer chain is nil")
	}
	st := planstore.New(s.db.WithoutTransaction())

	var plan *planstore.Plan
	var err error
	if strings.TrimSpace(planID) == "" {
		plan, err = st.GetActivePlan(ctx)
		if errors.Is(err, planstore.ErrNotFound) {
			return nil, fmt.Errorf("no active plan; create one with 'contenox plan new <goal>' before explore")
		}
		if err != nil {
			return nil, err
		}
	} else {
		plan, err = st.GetPlanByID(ctx, planID)
		if err != nil {
			return nil, err
		}
	}

	// Mirror callPlanner: extract the assistant's terminal text from the chain
	// output regardless of typed shape, then parse as RepoContext JSON.
	out, outType, _, execErr := s.engine.Execute(ctx, explorerChain, "", taskengine.DataTypeString)
	if execErr != nil {
		return nil, fmt.Errorf("explorerChain execute: %w", execErr)
	}
	raw := extractTerminalText(out, outType)
	rc, parseErr := parseRepoContextRaw(raw)
	if parseErr != nil {
		return nil, parseErr
	}
	b, mErr := json.Marshal(rc)
	if mErr != nil {
		return nil, fmt.Errorf("marshal repo context: %w", mErr)
	}
	if err := st.UpdatePlanRepoContext(ctx, plan.ID, string(b)); err != nil {
		return nil, fmt.Errorf("persist repo context: %w", err)
	}
	// Compile cache uses the seed prompt template, not its filled values, so
	// repo_context changes propagate via the runtime overlay only — no need to
	// invalidate the cached compiled chain here.
	return rc, nil
}

// extractTerminalText reads the assistant's last message content from a chain
// output regardless of typed shape — same logic as [callPlanner].
func extractTerminalText(out any, outType taskengine.DataType) string {
	switch outType {
	case taskengine.DataTypeString:
		if s, ok := out.(string); ok {
			return s
		}
	case taskengine.DataTypeJSON:
		b, _ := json.Marshal(out)
		return string(b)
	case taskengine.DataTypeChatHistory:
		if hist, ok := out.(taskengine.ChatHistory); ok && len(hist.Messages) > 0 {
			return hist.Messages[len(hist.Messages)-1].Content
		}
		if histPtr, ok := out.(*taskengine.ChatHistory); ok && len(histPtr.Messages) > 0 {
			return histPtr.Messages[len(histPtr.Messages)-1].Content
		}
	}
	return fmt.Sprintf("%v", out)
}

func (s *service) callPlanner(ctx context.Context, goal string, chain *taskengine.TaskChainDefinition) ([]string, error) {
	out, outType, _, err := s.engine.Execute(ctx, chain, goal, taskengine.DataTypeString)
	if err != nil {
		return nil, fmt.Errorf("plannerChain execute: %w", err)
	}
	var raw string
	switch outType {
	case taskengine.DataTypeString:
		raw, _ = out.(string)
	case taskengine.DataTypeJSON:
		b, _ := json.Marshal(out)
		raw = string(b)
	case taskengine.DataTypeChatHistory:
		if hist, ok := out.(taskengine.ChatHistory); ok && len(hist.Messages) > 0 {
			raw = hist.Messages[len(hist.Messages)-1].Content
		} else if histPtr, ok := out.(*taskengine.ChatHistory); ok && len(histPtr.Messages) > 0 {
			raw = histPtr.Messages[len(histPtr.Messages)-1].Content
		} else {
			raw = fmt.Sprintf("%v", out)
		}
	default:
		raw = fmt.Sprintf("%v", out)
	}
	steps, err := parsePlannerJSONRaw(raw)
	if err != nil {
		return nil, err
	}
	return steps, nil
}

// parsePlannerJSONRaw extracts a string-array plan from model output. It accepts:
//   - a JSON array of strings (preferred; see chain-planner.json)
//   - {"steps":["a","b"]}
//   - {"steps":[{"description":"a"},{"description":"b"}]}
func parsePlannerJSONRaw(raw string) ([]string, error) {
	trim := strings.TrimSpace(raw)
	if trim == "" {
		return nil, fmt.Errorf("empty planner output")
	}

	arrStr := taskengine.ExtractJSONArray(trim)
	var steps []string
	if err := json.Unmarshal([]byte(arrStr), &steps); err == nil {
		return normalizeAndValidatePlannerSteps(steps)
	}

	objStr := taskengine.ExtractJSONObject(trim)
	var wrapStrings struct {
		Steps []string `json:"steps"`
	}
	if err := json.Unmarshal([]byte(objStr), &wrapStrings); err == nil && len(wrapStrings.Steps) > 0 {
		return normalizeAndValidatePlannerSteps(wrapStrings.Steps)
	}
	var wrapObjs struct {
		Steps []struct {
			Description string `json:"description"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(objStr), &wrapObjs); err == nil {
		out := make([]string, 0, len(wrapObjs.Steps))
		for _, s := range wrapObjs.Steps {
			if d := strings.TrimSpace(s.Description); d != "" {
				out = append(out, d)
			}
		}
		if len(out) > 0 {
			return normalizeAndValidatePlannerSteps(out)
		}
	}

	arrErr := json.Unmarshal([]byte(arrStr), &steps)
	return nil, fmt.Errorf("plannerChain output is not a JSON string array: %w (raw: %.500s)", arrErr, raw)
}

// compileCacheKey combines the executor + summarizer chain ids with a hash of
// each chain's full JSON so edits to token_limit, tasks, hook_policies, or
// summarizer topology invalidate cached compiled plans. The key is persisted
// in plan.compile_executor_chain_id (column kept for backwards compatibility;
// it now holds the combined cache key, not just the executor id).
func compileCacheKey(executor, summarizer *taskengine.TaskChainDefinition) (string, error) {
	if executor == nil {
		return "", fmt.Errorf("executor chain is nil")
	}
	if summarizer == nil {
		return "", fmt.Errorf("summarizer chain is nil")
	}
	exID := strings.TrimSpace(executor.ID)
	if exID == "" {
		return "", fmt.Errorf("executorChain id is required for compile cache")
	}
	sumID := strings.TrimSpace(summarizer.ID)
	if sumID == "" {
		return "", fmt.Errorf("summarizerChain id is required for compile cache")
	}
	exRaw, err := json.Marshal(executor)
	if err != nil {
		return "", fmt.Errorf("executor chain marshal: %w", err)
	}
	sumRaw, err := json.Marshal(summarizer)
	if err != nil {
		return "", fmt.Errorf("summarizer chain marshal: %w", err)
	}
	exSum := sha256.Sum256(exRaw)
	sumSum := sha256.Sum256(sumRaw)
	return exID + ":" + hex.EncodeToString(exSum[:12]) + "|" + sumID + ":" + hex.EncodeToString(sumSum[:12]), nil
}

func (s *service) getOrCompileChain(ctx context.Context, plan *planstore.Plan, steps []*planstore.PlanStep, executor, summarizer *taskengine.TaskChainDefinition, cacheKey string) (*taskengine.TaskChainDefinition, error) {
	if plan.CompiledChainJSON != "" && plan.CompileExecutorChainID == cacheKey {
		var c taskengine.TaskChainDefinition
		if err := json.Unmarshal([]byte(plan.CompiledChainJSON), &c); err == nil && c.ID != "" && len(c.Tasks) > 0 {
			return &c, nil
		}
	}
	md := renderMarkdown(plan, steps)
	parsed, err := plancompile.ParseMarkdown(md)
	if err != nil {
		return nil, fmt.Errorf("parse plan markdown for compile: %w", err)
	}
	compiledID := plan.ID + "-compiled"
	compiled, err := plancompile.Compile(executor, summarizer, compiledID, parsed)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(compiled)
	if err != nil {
		return nil, err
	}
	st := planstore.New(s.db.WithoutTransaction())
	if err := st.UpdatePlanCompiledChain(ctx, plan.ID, string(b), compiledID, cacheKey); err != nil {
		return nil, err
	}
	return compiled, nil
}

// injectSummarizerModelDefault ensures {{var:summarizer_model}} is set in the
// overlay so the compiled summarizer task's execute_config.model macro never
// errors with "template var not set" when the caller didn't explicitly opt
// into a separate summarizer model. If already present on ctx (caller set it
// explicitly), we respect it; otherwise we copy {{var:model}} as the default.
func injectSummarizerModelDefault(ctx context.Context, overlay map[string]string) {
	if overlay == nil {
		return
	}
	if _, already := overlay["summarizer_model"]; already {
		return
	}
	if existing, err := taskengine.TemplateVarsFromContext(ctx); err == nil && existing != nil {
		if v, ok := existing["summarizer_model"]; ok && v != "" {
			overlay["summarizer_model"] = v
			return
		}
		if v, ok := existing["model"]; ok && v != "" {
			overlay["summarizer_model"] = v
			return
		}
	}
	if v, ok := overlay["model"]; ok && v != "" {
		overlay["summarizer_model"] = v
	}
}

// injectGateModelDefault ensures {{var:gate_model}} is set for optional gated executor
// chains (post-tool gate). Defaults to the main model when unset.
func injectGateModelDefault(ctx context.Context, overlay map[string]string) {
	injectModelVarDefault(ctx, overlay, "gate_model")
}

// injectCompactionModelDefault ensures {{var:compact_model}} is set for executor
// chains that opt into mid-run conversation compaction. Defaults to the main
// model when unset, mirroring [injectSummarizerModelDefault] / [injectGateModelDefault].
func injectCompactionModelDefault(ctx context.Context, overlay map[string]string) {
	injectModelVarDefault(ctx, overlay, "compact_model")
}

// injectModelVarDefault is the shared helper behind the per-role
// inject*ModelDefault functions: if overlay[varName] is unset, it copies the
// existing value from ctx (when present) or falls back to overlay["model"].
func injectModelVarDefault(ctx context.Context, overlay map[string]string, varName string) {
	if overlay == nil {
		return
	}
	if _, already := overlay[varName]; already {
		return
	}
	if existing, err := taskengine.TemplateVarsFromContext(ctx); err == nil && existing != nil {
		if v, ok := existing[varName]; ok && v != "" {
			overlay[varName] = v
			return
		}
		if v, ok := existing["model"]; ok && v != "" {
			overlay[varName] = v
			return
		}
	}
	if v, ok := overlay["model"]; ok && v != "" {
		overlay[varName] = v
	}
}

// classifyStepFailure picks a [planstore.FailureClass] for execErr.
//
// It first consults sink.LastErrorClass — set by [llmretry.Do] when the most
// recent chat round failed for a known transient/capacity/auth class. If the
// sink is empty (failure occurred outside a Chat call, e.g. tool-error,
// summarizer-validation, raise_error), it falls back to substring matching on
// execErr via [llmretry.ClassifyError].
//
// The mapping:
//
//	llmretry.ClassCapacity                                    → FailureClassCapacity
//	llmretry.ClassRateLimit / ClassServerError / ClassTimeout → FailureClassTransient
//	everything else                                           → FailureClassLogic
func classifyStepFailure(execErr error, sink *taskengine.RetryOutcomeSink) planstore.FailureClass {
	class := llmretry.ClassNone
	if sink != nil {
		class = sink.LastErrorClass()
	}
	if class == llmretry.ClassNone {
		class = llmretry.ClassifyError(execErr)
	}
	switch class {
	case llmretry.ClassCapacity:
		return planstore.FailureClassCapacity
	case llmretry.ClassRateLimit, llmretry.ClassServerError, llmretry.ClassTimeout:
		return planstore.FailureClassTransient
	}
	return planstore.FailureClassLogic
}

func (s *service) abortNextWithFailure(ctx context.Context, plan *planstore.Plan, pending *planstore.PlanStep, cause error) (string, string, error) {
	cleanupCtx := context.WithoutCancel(ctx)
	tx, commit, rTx, txErr := s.db.WithTransaction(cleanupCtx)
	if txErr != nil {
		return "", "", txErr
	}
	defer rTx()
	txSt := planstore.New(tx)
	msg := cause.Error()
	if err := txSt.UpdatePlanStepStatus(cleanupCtx, pending.ID, planstore.StepStatusFailed, msg); err != nil {
		return "", "", fmt.Errorf("update step after failure: %w", err)
	}
	allSteps, err := txSt.ListPlanSteps(cleanupCtx, plan.ID)
	if err != nil {
		return "", "", err
	}
	if err := commit(cleanupCtx); err != nil {
		return "", "", err
	}
	md := renderMarkdown(plan, allSteps)
	s.writePlanVFS(ctx, plan, allSteps)
	return "", md, fmt.Errorf("next step: %w", cause)
}

func renderMarkdown(plan *planstore.Plan, steps []*planstore.PlanStep) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Plan: %s\n\n", plan.Name))
	sb.WriteString(fmt.Sprintf("**Goal:** %s\n\n", plan.Goal))
	sb.WriteString(fmt.Sprintf("**Status:** %s\n\n", plan.Status))
	sb.WriteString("## Steps\n\n")
	for _, st := range steps {
		var marker string
		switch st.Status {
		case planstore.StepStatusCompleted:
			marker = "x"
		case planstore.StepStatusFailed:
			marker = "!"
		case planstore.StepStatusSkipped:
			marker = "-"
		default:
			marker = " "
		}
		sb.WriteString(fmt.Sprintf("- [%s] %d. %s\n", marker, st.Ordinal, st.Description))
		if result := strings.TrimSpace(st.ExecutionResult); result != "" {
			for _, line := range strings.Split(result, "\n") {
				sb.WriteString(fmt.Sprintf("  > %s\n", line))
			}
		}
	}
	return sb.String()
}

func (s *service) writePlanVFS(ctx context.Context, plan *planstore.Plan, steps []*planstore.PlanStep) {
	if s.vfs == nil {
		return
	}
	md := renderMarkdown(plan, steps)
	fileName := plan.Name + ".md"
	existing, err := s.vfs.GetFilesByPath(ctx, fileName)
	if err == nil && len(existing) > 0 {
		f := existing[0]
		if _, err := s.vfs.UpdateFile(ctx, &vfsservice.File{
			ID:          f.ID,
			Data:        []byte(md),
			ContentType: "text/markdown",
		}); err != nil {
			log.Printf("planservice: vfs update %s: %v", fileName, err)
		}
		return
	}
	if _, err := s.vfs.CreateFile(ctx, &vfsservice.File{
		Name:        fileName,
		Data:        []byte(md),
		ContentType: "text/markdown",
		ParentID:    "",
	}); err != nil {
		log.Printf("planservice: vfs create %s: %v", fileName, err)
	}
}

func (s *service) New(ctx context.Context, goal string, plannerChain *taskengine.TaskChainDefinition) (*planstore.Plan, []*planstore.PlanStep, string, error) {
	if goal == "" {
		return nil, nil, "", fmt.Errorf("goal is required")
	}
	if plannerChain == nil {
		return nil, nil, "", fmt.Errorf("plannerChain is required")
	}
	stepDescs, err := s.callPlanner(ctx, goal, plannerChain)
	if err != nil {
		return nil, nil, "", err
	}
	if len(stepDescs) == 0 {
		return nil, nil, "", fmt.Errorf("planner returned no steps")
	}

	planID := uuid.NewString()
	now := time.Now().UTC()
	plan := &planstore.Plan{
		ID:        planID,
		Name:      "plan-" + uuid.NewString()[:8],
		Goal:      goal,
		Status:    planstore.PlanStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}

	var stepSlice []*planstore.PlanStep
	for i, desc := range stepDescs {
		stepSlice = append(stepSlice, &planstore.PlanStep{
			ID:          uuid.NewString(),
			PlanID:      planID,
			Ordinal:     i + 1,
			Description: desc,
			Status:      planstore.StepStatusPending,
		})
	}

	tx, commit, rTx, err := s.db.WithTransaction(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	defer rTx()
	st := planstore.New(tx)
	if err := st.CreatePlan(ctx, plan); err != nil {
		return nil, nil, "", fmt.Errorf("create plan: %w", err)
	}
	if err := st.CreatePlanSteps(ctx, stepSlice...); err != nil {
		return nil, nil, "", fmt.Errorf("create steps: %w", err)
	}
	if err := commit(ctx); err != nil {
		return nil, nil, "", err
	}
	md := renderMarkdown(plan, stepSlice)
	s.writePlanVFS(ctx, plan, stepSlice)
	return plan, stepSlice, md, nil
}

func checkAndComplete(ctx context.Context, txSt planstore.Store, plan *planstore.Plan, allSteps []*planstore.PlanStep) error {
	allDone, hasFailed := true, false
	for _, step := range allSteps {
		switch step.Status {
		case planstore.StepStatusPending, planstore.StepStatusRunning:
			allDone = false
		case planstore.StepStatusFailed:
			hasFailed = true
		}
	}
	if allDone && !hasFailed {
		if err := txSt.UpdatePlanStatus(ctx, plan.ID, planstore.PlanStatusCompleted); err != nil {
			return fmt.Errorf("complete plan: %w", err)
		}
		plan.Status = planstore.PlanStatusCompleted
	}
	return nil
}

// maxOrdinalAmongRetainedPlanSteps is the highest ordinal among steps that remain after
// DeletePendingPlanSteps (everything except pending). Replacing only pending rows with new
// steps must start at this value + 1 so ordinals never collide with failed or running steps.
func maxOrdinalAmongRetainedPlanSteps(steps []*planstore.PlanStep) int {
	maxOrdinal := 0
	for _, st := range steps {
		if st.Status == planstore.StepStatusPending {
			continue
		}
		if st.Ordinal > maxOrdinal {
			maxOrdinal = st.Ordinal
		}
	}
	return maxOrdinal
}

// ReplanScope selects what part of the plan to regenerate.
//
//   - OnlyOrdinal == 0: full replan of all remaining (pending) work — current
//     [Service.Replan] semantics.
//   - OnlyOrdinal > 0:  replace only that step with substeps. The targeted step
//     is marked as skipped (audit-preserving) and the substeps are appended at
//     the tail of the plan. The compile cache is invalidated.
//
// Hint, when non-empty, is appended to the planner's user message so the model
// knows why we are replanning (e.g. "split into smaller steps; the previous
// attempt exceeded capacity").
type ReplanScope struct {
	OnlyOrdinal int
	Hint        string
}

// ReplanScoped is the generalized form of [Service.Replan]. ReplanScope{}
// preserves the original whole-tail-replan behavior; setting OnlyOrdinal
// targets a single step.
func (s *service) ReplanScoped(ctx context.Context, scope ReplanScope, plannerChain *taskengine.TaskChainDefinition) ([]*planstore.PlanStep, string, error) {
	if plannerChain == nil {
		return nil, "", fmt.Errorf("plannerChain is required")
	}
	plan, steps, err := s.activePlan(ctx)
	if err != nil {
		return nil, "", err
	}
	if plan == nil {
		return nil, "", fmt.Errorf("no active plan")
	}

	var target *planstore.PlanStep
	if scope.OnlyOrdinal > 0 {
		for _, st := range steps {
			if st.Ordinal == scope.OnlyOrdinal {
				target = st
				break
			}
		}
		if target == nil {
			return nil, "", fmt.Errorf("step %d not found", scope.OnlyOrdinal)
		}
	}

	var sb strings.Builder
	sb.WriteString(plan.Goal)
	sb.WriteString("\n\nProgress so far:\n")
	for _, st := range steps {
		switch st.Status {
		case planstore.StepStatusCompleted:
			sb.WriteString(fmt.Sprintf("- [done] %d. %s\n", st.Ordinal, st.Description))
		case planstore.StepStatusSkipped:
			sb.WriteString(fmt.Sprintf("- [skipped] %d. %s\n", st.Ordinal, st.Description))
		case planstore.StepStatusFailed:
			sb.WriteString(fmt.Sprintf("- [FAILED] %d. %s\n", st.Ordinal, st.Description))
			if st.ExecutionResult != "" {
				sb.WriteString(fmt.Sprintf("  Error: %s\n", strings.TrimSpace(st.ExecutionResult)))
			}
		}
	}
	if target != nil {
		sb.WriteString(fmt.Sprintf("\nReplace step %d (%q) with smaller substeps that accomplish the same thing. ", target.Ordinal, target.Description))
		if strings.TrimSpace(scope.Hint) != "" {
			sb.WriteString(strings.TrimSpace(scope.Hint))
			sb.WriteString(" ")
		}
		sb.WriteString("Output ONLY the substeps for step ")
		sb.WriteString(fmt.Sprintf("%d", target.Ordinal))
		sb.WriteString(" — do not regenerate other steps.")
	} else {
		sb.WriteString("\nGenerate only the remaining steps needed to achieve the goal.")
		if strings.TrimSpace(scope.Hint) != "" {
			sb.WriteString(" ")
			sb.WriteString(strings.TrimSpace(scope.Hint))
		}
	}

	maxOrdinal := maxOrdinalAmongRetainedPlanSteps(steps)

	newDescs, err := s.callPlanner(ctx, sb.String(), plannerChain)
	if err != nil {
		return nil, "", err
	}
	if target != nil {
		// Reject the degenerate "split = the original step verbatim" replan so
		// the auto-loop does not infinitely re-target the same ordinal.
		if len(newDescs) == 1 && strings.TrimSpace(newDescs[0]) == strings.TrimSpace(target.Description) {
			return nil, "", fmt.Errorf("scoped replan returned the original step text unchanged")
		}
	}

	var newSteps []*planstore.PlanStep
	for i, desc := range newDescs {
		newSteps = append(newSteps, &planstore.PlanStep{
			ID:          uuid.NewString(),
			PlanID:      plan.ID,
			Ordinal:     maxOrdinal + i + 1,
			Description: desc,
			Status:      planstore.StepStatusPending,
		})
	}

	tx, commit, rTx, err := s.db.WithTransaction(ctx)
	if err != nil {
		return nil, "", err
	}
	defer rTx()
	st := planstore.New(tx)
	if err := st.UpdatePlanCompiledChain(ctx, plan.ID, "", "", ""); err != nil {
		return nil, "", fmt.Errorf("clear compiled chain: %w", err)
	}
	if target != nil {
		// Audit-preserve the failed step: mark skipped with a brief reason so
		// the timeline reads "step K was unsplittable as written; superseded
		// by appended substeps". Do not delete; do not renumber.
		reason := fmt.Sprintf("superseded by substeps appended after ordinal %d (replan-scoped)", maxOrdinal)
		if err := st.UpdatePlanStepStatus(ctx, target.ID, planstore.StepStatusSkipped, reason); err != nil {
			return nil, "", fmt.Errorf("mark target skipped: %w", err)
		}
		if err := st.SetPlanStepFailureClass(ctx, target.ID, planstore.FailureClassEmpty); err != nil {
			return nil, "", fmt.Errorf("clear target failure class: %w", err)
		}
	} else {
		if err := st.DeletePendingPlanSteps(ctx, plan.ID); err != nil {
			return nil, "", fmt.Errorf("delete pending: %w", err)
		}
	}
	if err := st.CreatePlanSteps(ctx, newSteps...); err != nil {
		return nil, "", fmt.Errorf("create new steps: %w", err)
	}
	if err := commit(ctx); err != nil {
		return nil, "", err
	}

	allSteps, err := planstore.New(s.db.WithoutTransaction()).ListPlanSteps(ctx, plan.ID)
	if err != nil {
		return nil, "", err
	}
	md := renderMarkdown(plan, allSteps)
	s.writePlanVFS(ctx, plan, allSteps)
	return newSteps, md, nil
}

func (s *service) Replan(ctx context.Context, plannerChain *taskengine.TaskChainDefinition) ([]*planstore.PlanStep, string, error) {
	return s.ReplanScoped(ctx, ReplanScope{}, plannerChain)
}

func (s *service) Next(ctx context.Context, args Args, executorChain, summarizerChain *taskengine.TaskChainDefinition) (string, string, error) {
	if executorChain == nil {
		return "", "", fmt.Errorf("executorChain is required")
	}
	if summarizerChain == nil {
		return "", "", fmt.Errorf("summarizerChain is required")
	}
	cacheKey, err := compileCacheKey(executorChain, summarizerChain)
	if err != nil {
		return "", "", err
	}

	st := planstore.New(s.db.WithoutTransaction())
	plan, err := st.GetActivePlan(ctx)
	if errors.Is(err, planstore.ErrNotFound) {
		return "", "", fmt.Errorf("no active plan")
	}
	if err != nil {
		return "", "", err
	}

	pending, err := st.ClaimNextPendingStep(ctx, plan.ID)
	if errors.Is(err, planstore.ErrNotFound) {
		return "", "", fmt.Errorf("no pending steps remaining")
	}
	if err != nil {
		return "", "", err
	}

	plan, err = st.GetPlanByID(ctx, plan.ID)
	if err != nil {
		return "", "", err
	}
	steps, err := st.ListPlanSteps(ctx, plan.ID)
	if err != nil {
		return "", "", err
	}

	compiled, err := s.getOrCompileChain(ctx, plan, steps, executorChain, summarizerChain, cacheKey)
	if err != nil {
		return s.abortNextWithFailure(ctx, plan, pending, err)
	}

	stepChain, err := plancompile.ExtractStepChain(compiled, pending.Ordinal)
	if err != nil {
		return s.abortNextWithFailure(ctx, plan, pending, err)
	}

	overlay := NewPlanStepMacroVars(plan, steps, pending).TemplateVars()
	if reqID, ok := ctx.Value(libtracker.ContextKeyRequestID).(string); ok && reqID != "" {
		overlay["request_id"] = reqID
	}
	// summarizer_model defaults to {{var:model}} when the caller did not set it
	// explicitly, so operators can opt into a cheaper/faster model for summary
	// work without forcing every caller to know about the new var.
	injectSummarizerModelDefault(ctx, overlay)
	injectGateModelDefault(ctx, overlay)
	injectCompactionModelDefault(ctx, overlay)
	execCtx := taskengine.MergeTemplateVars(ctx, overlay)
	// Attach a compaction state registry so persistent circuit-breaker state
	// survives across the agentic-loop's repeated chat invocations.
	execCtx = taskengine.WithCompactionRegistry(execCtx, taskengine.NewCompactionStateRegistry())
	// Retry-outcome sink: read by the failure-classification logic below to
	// distinguish capacity / transient / logic errors and tag the step with a
	// [planstore.FailureClass] so 'plan next --auto' can decide whether to
	// auto-replan instead of giving up. Always attached; cheap when unused.
	retrySink := &taskengine.RetryOutcomeSink{}
	execCtx = taskengine.WithRetryOutcomeSink(execCtx, retrySink)
	// Plan-step identity flows via ctx to the plan_summary hook so it knows
	// which DB row to write (identity chosen at ClaimNextPendingStep time,
	// not at plancompile time; cannot live in compiled chain JSON).
	execCtx = taskengine.WithPlanStepContext(execCtx, plan.ID, pending.ID)
	if !args.WithShell {
		execCtx = taskengine.WithRuntimeHookAllowlist(execCtx, []string{"*", "!local_shell"})
	}

	out, _, _, execErr := s.engine.Execute(execCtx, stepChain, plan.Goal, taskengine.DataTypeString)
	result := formatTaskOutput(out)

	finalStatus := planstore.StepStatusCompleted
	finalResult := result
	failureClass := planstore.FailureClassEmpty
	if execErr != nil {
		finalStatus = planstore.StepStatusFailed
		finalResult = execErr.Error()
		result = ""
		failureClass = classifyStepFailure(execErr, retrySink)
	}

	cleanupCtx := context.WithoutCancel(ctx)
	tx, commit, rTx, txErr := s.db.WithTransaction(cleanupCtx)
	if txErr != nil {
		return "", "", txErr
	}
	defer rTx()
	txSt := planstore.New(tx)
	if err := txSt.UpdatePlanStepStatus(cleanupCtx, pending.ID, finalStatus, finalResult); err != nil {
		return "", "", fmt.Errorf("update step: %w", err)
	}
	if err := txSt.SetPlanStepFailureClass(cleanupCtx, pending.ID, failureClass); err != nil {
		return "", "", fmt.Errorf("update step failure class: %w", err)
	}
	allSteps, err := txSt.ListPlanSteps(cleanupCtx, plan.ID)
	if err != nil {
		return "", "", fmt.Errorf("list steps: %w", err)
	}
	if err := checkAndComplete(cleanupCtx, txSt, plan, allSteps); err != nil {
		return "", "", err
	}
	if err := commit(cleanupCtx); err != nil {
		return "", "", err
	}
	md := renderMarkdown(plan, allSteps)
	s.writePlanVFS(ctx, plan, allSteps)
	if execErr != nil {
		return "", md, execErr
	}
	return result, md, nil
}

func (s *service) Retry(ctx context.Context, ordinal int) (string, error) {
	plan, steps, err := s.activePlan(ctx)
	if err != nil {
		return "", err
	}
	if plan == nil {
		return "", fmt.Errorf("no active plan")
	}
	var target *planstore.PlanStep
	for _, st := range steps {
		if st.Ordinal == ordinal {
			target = st
			break
		}
	}
	if target == nil {
		return "", fmt.Errorf("step %d not found", ordinal)
	}
	tx, commit, rTx, err := s.db.WithTransaction(ctx)
	if err != nil {
		return "", err
	}
	defer rTx()
	txSt := planstore.New(tx)
	// Preserve the failed attempt's summary (or ExecutionResult fallback) as
	// LastFailureSummary so the re-run's summarizer can see why the prior try
	// failed. Matches the repair_js pattern in the enterprise state machine:
	// prior-attempt context feeds the repair attempt.
	if err := txSt.MoveSummaryToLastFailure(ctx, target.ID); err != nil {
		return "", err
	}
	if err := txSt.UpdatePlanStepStatus(ctx, target.ID, planstore.StepStatusPending, ""); err != nil {
		return "", err
	}
	if err := commit(ctx); err != nil {
		return "", err
	}
	allSteps, err := planstore.New(s.db.WithoutTransaction()).ListPlanSteps(ctx, plan.ID)
	if err != nil {
		return "", err
	}
	md := renderMarkdown(plan, allSteps)
	s.writePlanVFS(ctx, plan, allSteps)
	return md, nil
}

func (s *service) Skip(ctx context.Context, ordinal int) (string, error) {
	plan, steps, err := s.activePlan(ctx)
	if err != nil {
		return "", err
	}
	if plan == nil {
		return "", fmt.Errorf("no active plan")
	}
	var target *planstore.PlanStep
	for _, st := range steps {
		if st.Ordinal == ordinal {
			target = st
			break
		}
	}
	if target == nil {
		return "", fmt.Errorf("step %d not found", ordinal)
	}
	tx, commit, rTx, err := s.db.WithTransaction(ctx)
	if err != nil {
		return "", err
	}
	defer rTx()
	txSt := planstore.New(tx)
	if err := txSt.UpdatePlanStepStatus(ctx, target.ID, planstore.StepStatusSkipped, "skipped"); err != nil {
		return "", err
	}
	allSteps, err := txSt.ListPlanSteps(ctx, plan.ID)
	if err != nil {
		return "", err
	}
	if err := checkAndComplete(ctx, txSt, plan, allSteps); err != nil {
		return "", err
	}
	if err := commit(ctx); err != nil {
		return "", err
	}
	md := renderMarkdown(plan, allSteps)
	s.writePlanVFS(ctx, plan, allSteps)
	return md, nil
}

func (s *service) Active(ctx context.Context) (*planstore.Plan, []*planstore.PlanStep, error) {
	return s.activePlan(ctx)
}

func (s *service) Show(ctx context.Context) (string, error) {
	plan, steps, err := s.activePlan(ctx)
	if err != nil {
		return "", err
	}
	if plan == nil {
		return "", fmt.Errorf("no active plan")
	}
	return renderMarkdown(plan, steps), nil
}

func (s *service) List(ctx context.Context) ([]*planstore.Plan, error) {
	tx, commit, rTx, err := s.db.WithTransaction(ctx)
	if err != nil {
		return nil, err
	}
	defer rTx()
	plans, err := planstore.New(tx).ListPlans(ctx)
	if err != nil {
		return nil, err
	}
	if err := commit(ctx); err != nil {
		return nil, err
	}
	return plans, nil
}

func (s *service) SetActive(ctx context.Context, planName string) error {
	tx, commit, rTx, err := s.db.WithTransaction(ctx)
	if err != nil {
		return err
	}
	defer rTx()
	st := planstore.New(tx)
	if err := st.ArchiveActivePlans(ctx); err != nil {
		return err
	}
	target, err := st.GetPlanByName(ctx, planName)
	if err != nil {
		return fmt.Errorf("plan %q not found: %w", planName, err)
	}
	if err := st.UpdatePlanStatus(ctx, target.ID, planstore.PlanStatusActive); err != nil {
		return err
	}
	return commit(ctx)
}

func (s *service) Delete(ctx context.Context, planName string) error {
	tx, commit, rTx, err := s.db.WithTransaction(ctx)
	if err != nil {
		return err
	}
	defer rTx()
	st := planstore.New(tx)
	plan, err := st.GetPlanByName(ctx, planName)
	if err != nil {
		return fmt.Errorf("plan %q not found: %w", planName, err)
	}
	if err := st.DeletePlan(ctx, plan.ID); err != nil {
		return err
	}
	return commit(ctx)
}

func (s *service) Clean(ctx context.Context) (int, error) {
	n, err := planstore.New(s.db.WithoutTransaction()).DeleteFinishedPlans(ctx)
	if err != nil {
		return 0, err
	}
	return n, nil
}
