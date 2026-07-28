package taskengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/Masterminds/sprig/v3"

	"github.com/contenox/contenox/internal/errdefs"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/getkin/kin-openapi/openapi3"
)

// DataType represents the type of data passed between tasks.
type DataType int

const (
	DataTypeAny DataType = iota
	DataTypeString
	DataTypeInt
	DataTypeJSON
	DataTypeChatHistory
	DataTypeNil
)

// String returns the string representation of the data type.
func (d *DataType) String() string {
	switch *d {
	case DataTypeAny:
		return "any"
	case DataTypeString:
		return "string"
	case DataTypeInt:
		return "int"
	case DataTypeJSON:
		return "json"
	case DataTypeChatHistory:
		return "chat_history"
	case DataTypeNil:
		return "nil"
	default:
		return "unknown"
	}
}

// DataTypeFromString converts a string to DataType.
func DataTypeFromString(s string) (DataType, error) {
	switch strings.ToLower(s) {
	case "any":
		return DataTypeAny, nil
	case "string":
		return DataTypeString, nil
	case "int":
		return DataTypeInt, nil
	case "json":
		return DataTypeJSON, nil
	case "chat_history":
		return DataTypeChatHistory, nil
	case "nil":
		return DataTypeNil, nil
	default:
		return DataTypeAny, fmt.Errorf("unknown data type: %s", s)
	}
}

// EnvExecutor executes complete task chains with input and environment management.
type EnvExecutor interface {
	ExecEnv(ctx context.Context, chain *TaskChainDefinition, input any, dataType DataType) (any, DataType, []CapturedStateUnit, error)
}

// ErrUnsupportedTaskType indicates unrecognized task type
var ErrUnsupportedTaskType = errors.New("executor does not support the task type")

// ErrToolsNotFound is returned when a named tools is not registered in any repo.
var ErrToolsNotFound = errors.New("tools not found")

// ErrToolsToolsUnavailable is returned when a tools is registered but its tool
// list cannot be loaded (e.g. MCP server unreachable or list-tools failed).
// ExecEnv treats this like a missing tools for tool preload: skip tools, continue the chain.
var ErrToolsToolsUnavailable = errors.New("tools tools unavailable")

type toolsToolsUnavailableError struct {
	toolsName string
	cause     error
}

func (e *toolsToolsUnavailableError) Error() string {
	return fmt.Sprintf("%s: tools %q: %v", ErrToolsToolsUnavailable, e.toolsName, e.cause)
}

func (e *toolsToolsUnavailableError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return []error{ErrToolsToolsUnavailable, e.cause}
}

// ToolsToolsUnavailable wraps cause as ErrToolsToolsUnavailable for toolsName (for errors.Is).
func ToolsToolsUnavailable(toolsName string, cause error) error {
	if cause == nil {
		return nil
	}
	return &toolsToolsUnavailableError{
		toolsName: toolsName,
		cause:     cause,
	}
}

// ToolsRepo defines interface for external system integrations and side effects.
type ToolsRepo interface {
	Exec(ctx context.Context, startingTime time.Time, input any, debug bool, args *ToolsCall) (any, DataType, error)
	ToolsRegistry
	ToolsWithSchema
}

type ToolsProvider interface {
	ToolsRegistry
	ToolsWithSchema
}

type ToolsRegistry interface {
	Supports(ctx context.Context) ([]string, error)
}

type ToolsWithSchema interface {
	GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error)
	GetToolsForToolsByName(ctx context.Context, name string) ([]Tool, error)
}

// SimpleEnv is the default implementation of EnvExecutor.
type SimpleEnv struct {
	exec          TaskExecutor
	tracker       libtracker.ActivityTracker
	inspector     Inspector
	toolsProvider ToolsRepo
	eventSink     TaskEventSink
}

// NewEnv creates a new SimpleEnv with the given tracker and task executor.
func NewEnv(
	ctx context.Context,
	tracker libtracker.ActivityTracker,
	exec TaskExecutor,
	inspector Inspector,
	toolsProvider ToolsRepo,
) (EnvExecutor, error) {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &SimpleEnv{
		exec:          exec,
		tracker:       tracker,
		inspector:     inspector,
		toolsProvider: toolsProvider,
		eventSink:     taskEventSinkFromContext(ctx),
	}, nil
}

type ChainContext struct {
	Tools       map[string]ToolWithResolution
	ClientTools []Tool
	Debug       bool
}

type ToolWithResolution struct {
	Tool
	ToolsName string
}

// ExecEnv executes the given chain with the provided input.
func (env SimpleEnv) ExecEnv(ctx context.Context, chain *TaskChainDefinition, input any, dataType DataType) (result any, resultType DataType, history []CapturedStateUnit, retErr error) {
	_, reportChangeChain, endChain := env.tracker.Start(ctx, "chain_exec", chain.ID, "chain_id", chain.ID)
	defer endChain()

	// Address invariant: every event of this run carries at least the chain in
	// its scope. Task attempts below override this with the full task scope.
	ctx = WithTaskEventScope(ctx, TaskEventScope{ChainID: chain.ID})

	stack := env.inspector.Start(ctx)

	defer func() {
		chainEvent := NewTaskEvent(ctx, TaskEventChainCompleted)
		chainEvent.ChainID = chain.ID
		chainEvent.OutputType = resultType.String()
		if retErr != nil {
			var susp *ChainSuspendedError
			if errors.As(retErr, &susp) {
				// A suspension is a typed terminal, not a failure: the
				// segment ends with chain_suspended carrying the
				// interrupt's address and the approval/checkpoint key,
				// published after the checkpoint is persisted so a
				// consumer reacting to it can already resume.
				chainEvent.Kind = TaskEventChainSuspended
				chainEvent.ApprovalID = susp.ApprovalID
				chainEvent.Scope = susp.Scope
				chainEvent.TaskID = susp.Scope.Task
				chainEvent.OutputType = ""
			} else {
				chainEvent.Kind = TaskEventChainFailed
				chainEvent.Error = retErr.Error()
				chainEvent.OutputType = ""
			}
		}
		publishTaskEventBestEffort(ctx, env.tracker, env.eventSink, chainEvent)
	}()
	chainStarted := NewTaskEvent(ctx, TaskEventChainStarted)
	chainStarted.ChainID = chain.ID
	publishTaskEventBestEffort(ctx, env.tracker, env.eventSink, chainStarted)

	vars := map[string]any{
		"input": input,
	}
	varTypes := map[string]DataType{"input": dataType}
	startingTime := time.Now().UTC()
	var err error

	if err := validateChain(chain.Tasks); err != nil {
		return nil, DataTypeAny, stack.GetExecutionHistory(), err
	}

	currentTask, err := findTaskByID(chain.Tasks, chain.Tasks[0].ID)
	if err != nil {
		return nil, DataTypeAny, stack.GetExecutionHistory(), err
	}

	var finalOutput any
	var transitionEval string
	var output any = input
	var outputType DataType = dataType
	var taskErr error
	var inputVar string

	// edgeCounts tracks how many times each edge "fromTaskID->toTaskID" has been
	// traversed during this chain run. Consulted by OpEdgeTraversedAtLeast to
	// bound workflow loops and other cyclic chains. Per-Execute, no DB.
	edgeCounts := map[string]int{}

	// A checkpoint on the context re-enters the chain at the interrupted task
	// with the suspended run's variable state, edge counts, and transcript
	// restored. Everything after this block is the normal execution path.
	resumeFirstAttempt := false
	if cp := resumeCheckpointFromContext(ctx); cp != nil {
		currentTask, err = findTaskByID(chain.Tasks, cp.TaskID)
		if err != nil {
			return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("resume checkpoint %s names task %q which this chain does not contain: %w", cp.ApprovalID, cp.TaskID, err)
		}
		for k, v := range cp.Vars {
			vars[k] = v
		}
		for k, t := range cp.VarTypes {
			varTypes[k] = t
		}
		for k, n := range cp.EdgeCounts {
			edgeCounts[k] = n
		}
		output = cp.History
		outputType = DataTypeChatHistory
		resumeFirstAttempt = true
	}

	chainContext := &ChainContext{
		Tools:       map[string]ToolWithResolution{},
		ClientTools: []Tool{},
		Debug:       chain.Debug,
	}
	filter := map[string]ToolWithResolution{}
	unavailableTools := map[string]struct{}{}
	for _, task := range chain.Tasks {
		if task.ExecuteConfig == nil {
			continue
		}
		toolsNames, err := resolveToolsNames(ctx, task.ExecuteConfig.Tools, env.toolsProvider)
		if err != nil {
			return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: failed to resolve tools: %w", currentTask.ID, err)
		}
		for _, toolsName := range toolsNames {
			if _, unavailable := unavailableTools[toolsName]; unavailable {
				continue
			}
			// WithToolsArgs copies the map, so the stored value is immutable
			// and safe to read concurrently without locks. execute_config's
			// tools_policies is the primary mechanism; task.Tools.Args is
			// the secondary one, for HandleTools tasks.
			toolCtx := ctx
			if task.ExecuteConfig != nil {
				if policy, ok := task.ExecuteConfig.ToolsPolicies[toolsName]; ok && len(policy) > 0 {
					toolCtx = WithToolsArgs(toolCtx, toolsName, policy)
				}
			}
			if task.Tools != nil && task.Tools.Name == toolsName && len(task.Tools.Args) > 0 {
				toolCtx = WithToolsArgs(toolCtx, toolsName, task.Tools.Args)
			}
			toolsTools, err := env.toolsProvider.GetToolsForToolsByName(toolCtx, toolsName)
			if err != nil {
				if errors.Is(err, ErrToolsNotFound) {
					// Tools not registered (e.g. local_shell disabled).
					continue
				}
				if errors.Is(err, ErrToolsToolsUnavailable) {
					unavailableTools[toolsName] = struct{}{}
					reportChangeChain("tools_unavailable", map[string]any{
						"name":  toolsName,
						"error": err.Error(),
					})
					continue
				}
				return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: failed to get tools for tools %s: %w", currentTask.ID, toolsName, err)
			}
			for _, tool := range toolsTools {
				tool.Function.Name = toolsName + "." + tool.Function.Name
				filter[tool.Function.Name] = ToolWithResolution{Tool: tool, ToolsName: toolsName}
			}
		}
	}

	for _, twr := range filter {
		chainContext.Tools[twr.Function.Name] = twr
	}

	for {
		if ctx.Err() != nil {
			return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: %w", currentTask.ID, ctx.Err())
		}

		taskInput := output
		taskInputType := outputType
		inputVar = currentTask.ID
		if resumeFirstAttempt {
			// The resumed task re-enters with the checkpointed transcript
			// verbatim: no input_var redirection, no prompt-template render,
			// no input cap — replacing or truncating it would resume a
			// different run than the one that suspended.
			resumeFirstAttempt = false
		} else {
			if currentTask.InputVar != "" {
				var ok bool
				inputVar = currentTask.InputVar

				taskInput, ok = vars[inputVar]
				if !ok {
					return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: input variable %q not found", currentTask.ID, currentTask.InputVar)
				}
				taskInputType, ok = varTypes[inputVar]
				if !ok {
					return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: input variable %q missing type info", currentTask.ID, currentTask.InputVar)
				}
			}

			if currentTask.PromptTemplate != "" {
				rendered, err := renderTemplate(expandStepMacros(currentTask.PromptTemplate, edgeCounts), vars)
				if err != nil {
					return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: template error: %v", currentTask.ID, err)
				}
				taskInput = rendered
				taskInputType = DataTypeString
			}
			taskInput, taskInputType = capTaskInputForExecution(taskInput, taskInputType, currentTask.InputMaxBytes)
		}
		maxRetries := max(currentTask.RetryOnFailure, 0)

		for retry := 0; retry <= maxRetries; retry++ {
			// Keep task execution attached to the caller so cancellation from
			// Ctrl+C, request shutdown, or parent timeouts stops in-flight work.
			taskCtx := ctx

			var cancel context.CancelFunc
			if currentTask.Timeout != "" {
				timeout, err := time.ParseDuration(currentTask.Timeout)
				if err != nil {
					return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: invalid timeout: %v", currentTask.ID, err)
				}
				taskCtx, cancel = context.WithTimeout(taskCtx, timeout)
			}
			taskCtx = WithTaskEventScope(taskCtx, TaskEventScope{
				ChainID:     chain.ID,
				TaskID:      currentTask.ID,
				TaskHandler: currentTask.Handler.String(),
				Retry:       retry,
			})
			stepStarted := NewTaskEvent(taskCtx, TaskEventStepStarted)
			publishTaskEventBestEffort(taskCtx, env.tracker, env.eventSink, stepStarted)
			reportErrAttempt, reportChangeAttempt, endAttempt := env.tracker.Start(
				taskCtx,
				"task_attempt",
				currentTask.ID,
				"retry", retry,
				"task_type", currentTask.Handler,
			)

			startTime := time.Now().UTC()

			stepTask := *currentTask
			stepTask.SystemInstruction = expandStepMacros(currentTask.SystemInstruction, edgeCounts)
			stepTask.PromptTemplate = expandStepMacros(currentTask.PromptTemplate, edgeCounts)
			stepTask.OutputTemplate = expandStepMacros(currentTask.OutputTemplate, edgeCounts)
			stepTask.Print = expandStepMacros(currentTask.Print, edgeCounts)
			taskCtx = WithEdgeCounts(taskCtx, edgeCounts)

			// chain.TokenLimit is the base budget; a per-request context
			// length attached to taskCtx (e.g. the model's declared
			// ContextLength) is preferred, so usage indicators report the
			// real model window instead of 0 or a chain default.
			tokenLimit := int(chain.TokenLimit)
			if requested := RequestedContextLengthFromContext(taskCtx); requested > 0 {
				if tokenLimit <= 0 || requested < tokenLimit {
					tokenLimit = requested
				}
			}

			output, outputType, transitionEval, taskErr = env.exec.TaskExec(taskCtx, startingTime, tokenLimit, chainContext, &stepTask, taskInput, taskInputType)
			if taskErr != nil {
				taskErr = fmt.Errorf("task %s: %w", currentTask.ID, taskErr)
				reportErrAttempt(taskErr)
			}
			endAttempt()
			if cancel != nil {
				cancel()
			}

			// A parked approval is handled before the retry/on_failure
			// machinery below: it is neither a failure to retry nor one to
			// route around.
			var pendErr *ApprovalPendingError
			if errors.As(taskErr, &pendErr) {
				return env.suspendRun(ctx, stack, chain, currentTask, retry, vars, varTypes, edgeCounts, output, pendErr)
			}
			duration := time.Since(startTime)
			errState := ErrorResponse{
				ErrorInternal: taskErr,
			}
			if taskErr != nil {
				errState.Error = taskErr.Error()
			}
			step := CapturedStateUnit{
				Scope:       EventScope{Chain: chain.ID, Task: currentTask.ID},
				TaskID:      currentTask.ID,
				TaskHandler: currentTask.Handler.String(),
				InputType:   taskInputType,
				OutputType:  outputType,
				InputVar:    inputVar,
				Transition:  transitionEval,
				Duration:    duration,
				Error:       errState,
				Input:       taskInput,
				Output:      output,
				RetryIndex:  retry,
			}
			if taskErr != nil {
				if errors.Is(taskCtx.Err(), context.DeadlineExceeded) {
					step.TimedOut = true
				} else if errors.Is(taskCtx.Err(), context.Canceled) {
					step.Cancelled = true
				}
			}
			if currentTask.ExecuteConfig != nil {
				step.ProviderType = currentTask.ExecuteConfig.Provider
				step.ModelName = GetPrimaryModel(currentTask.ExecuteConfig)
			}
			if currentTask.Handler == HandleExecuteToolCalls {
				if names := extractToolNamesFromOutput(output, outputType); len(names) > 0 {
					step.ToolNames = names
				}
			}
			if hist, ok := output.(ChatHistory); ok {
				if hist.InputTokens > 0 || hist.OutputTokens > 0 {
					step.TokenUsage = &TokenUsage{
						Prompt:     hist.InputTokens,
						Completion: hist.OutputTokens,
						Total:      hist.InputTokens + hist.OutputTokens,
					}
				}
				step.FinishReason = hist.FinishReason
			}
			stack.RecordStep(step)

			stepEvent := NewTaskEvent(taskCtx, TaskEventStepCompleted)
			stepEvent.OutputType = outputType.String()
			stepEvent.Transition = transitionEval
			if taskErr != nil {
				stepEvent.Kind = TaskEventStepFailed
				stepEvent.Error = taskErr.Error()
				stepEvent.OutputType = ""
				publishTaskEventBestEffort(taskCtx, env.tracker, env.eventSink, stepEvent)
				reportErrAttempt(taskErr)
				continue
			}
			publishTaskEventBestEffort(taskCtx, env.tracker, env.eventSink, stepEvent)

			reportChangeAttempt(currentTask.ID, output)
			break
		}

		if taskErr != nil {
			if currentTask.Transition.OnFailure != "" {
				// Prefer the typed input that led to the failure. Only keep
				// the task's output if it is a real, typed value. If the
				// available type is DataTypeAny, infer a concrete type from the
				// failure input so downstream handlers don't receive "any".
				failedOutput := taskInput
				failedOutputType := taskInputType
				if output != nil && outputType != DataTypeAny && outputType != DataTypeNil {
					failedOutput = output
					failedOutputType = outputType
				}
				if failedOutputType == DataTypeAny {
					failedOutputType = InferDataType(failedOutput)
				}
				vars[currentTask.ID] = failedOutput
				varTypes[currentTask.ID] = failedOutputType
				vars["previous_output"] = failedOutput
				varTypes["previous_output"] = failedOutputType
				// Expose the raw error message so failure-handler tasks can
				// reference it via {{.last_error}} or {{.<taskID>_error}}.
				if taskErr != nil {
					vars["last_error"] = taskErr.Error()
					varTypes["last_error"] = DataTypeString
					vars[currentTask.ID+"_error"] = taskErr.Error()
					varTypes[currentTask.ID+"_error"] = DataTypeString
				}
				output = failedOutput
				outputType = failedOutputType

				previousTaskID := currentTask.ID
				edgeCounts[previousTaskID+"->"+currentTask.Transition.OnFailure]++
				currentTask, err = findTaskByID(chain.Tasks, currentTask.Transition.OnFailure)
				if err != nil {
					return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("error transition target not found: %v", err)
				}
				_, reportChangeErrTransition, endErrTransition := env.tracker.Start(
					ctx,
					"next_task",
					previousTaskID,
					"next_task", currentTask.ID,
					"reason", "error",
				)
				reportChangeErrTransition(currentTask.ID, taskErr)
				endErrTransition() // direct call, not defer: defers inside loops leak
				continue
			}
			return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s failed after %d retries: %w", currentTask.ID, maxRetries, taskErr)
		}

		if currentTask.Print != "" {
			printMsg, err := renderTemplate(expandStepMacros(currentTask.Print, edgeCounts), vars)
			if err != nil {
				return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: print template error: %v", currentTask.ID, err)
			}
			printEvent := NewTaskEvent(ctx, TaskEventPrint)
			printEvent.Content = printMsg
			publishTaskEventBestEffort(ctx, env.tracker, env.eventSink, printEvent)
		}

		nextTaskID, _, err := env.evaluateTransitions(ctx, currentTask.ID, currentTask.Transition, transitionEval, edgeCounts)
		if err != nil {
			return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: transition error: %v", currentTask.ID, err)
		}

		vars["previous_output"] = output
		vars[currentTask.ID] = output
		varTypes["previous_output"] = outputType
		varTypes[currentTask.ID] = outputType

		if nextTaskID == "" || nextTaskID == TermEnd {
			finalOutput = output
			_, reportChangeFinal, endFinal := env.tracker.Start(
				ctx,
				"chain_complete",
				"chain")
			reportChangeFinal("chain", finalOutput)
			endFinal() // direct call, not defer
			break
		}

		_, reportChangeTransition, endTransition := env.tracker.Start(
			ctx,
			"next_task",
			currentTask.ID,
			"next_task", nextTaskID,
		)
		reportChangeTransition(nextTaskID, transitionEval)
		endTransition() // direct call, not defer

		// Count this traversal before reassigning currentTask.
		edgeCounts[currentTask.ID+"->"+nextTaskID]++

		currentTask, err = findTaskByID(chain.Tasks, nextTaskID)
		if err != nil {
			return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("next task %s not found: %v", nextTaskID, err)
		}
	}

	normOut, normDT, normErr := NormalizeFinalChainOutput(finalOutput, outputType)
	if normErr != nil {
		return nil, DataTypeAny, stack.GetExecutionHistory(), normErr
	}
	return normOut, normDT, stack.GetExecutionHistory(), nil
}

func renderTemplate(tmplStr string, vars any) (string, error) {
	// missingkey=error is intentionally not set: a referenced-but-absent
	// variable renders "<no value>" rather than erroring, since templates
	// legitimately reference not-yet-populated keys (e.g. a task's Print
	// referencing its own id, set only after Print renders).
	tmpl, err := template.New("prompt").Funcs(sprig.TxtFuncMap()).Parse(tmplStr)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (exe SimpleEnv) evaluateTransitions(_ context.Context, _ string, transition TaskTransition, eval string, edgeCounts map[string]int) (string, *TransitionBranch, error) {
	// A task with no branches is a leaf: end the chain cleanly with its output
	// rather than erroring. (Authoring an explicit {operator: default, goto: ""}
	// branch remains the way to end conditionally.)
	if len(transition.Branches) == 0 {
		return "", nil, nil
	}

	for _, branch := range transition.Branches {
		if branch.Operator == OpDefault {
			continue
		}

		// Edge-state operators read engine state, not task output.
		if branch.Operator == OpEdgeTraversedAtLeast {
			threshold, err := strconv.Atoi(strings.TrimSpace(branch.When))
			if err != nil {
				// Treat as non-match so OpDefault can still fire.
				continue
			}
			if edgeCounts[branch.Edge] >= threshold {
				return branch.Goto, &branch, nil
			}
			continue
		}

		match, err := compare(branch.Operator, eval, branch.When)
		if err != nil {
			// Treat parse errors as non-match so OpDefault can still fire,
			// rather than bypassing the safe fallback branch entirely.
			match = false
		}
		if match {
			return branch.Goto, &branch, nil
		}
	}

	for _, branch := range transition.Branches {
		if branch.Operator == OpDefault {
			return branch.Goto, &branch, nil
		}
	}

	return "", nil, fmt.Errorf("no matching transition found for eval: %s", eval)
}

// compare applies a logical operator to a model response and a target value.
func compare(operator OperatorTerm, response, when string) (bool, error) {
	switch operator {
	case OpEquals:
		return response == when, nil
	case OpContains:
		return strings.Contains(response, when), nil
	case OpStartsWith:
		return strings.HasPrefix(response, when), nil
	case OpEndsWith:
		return strings.HasSuffix(response, when), nil
	default:
		return false, fmt.Errorf("unsupported operator: %s", operator)
	}
}

// findTaskByID returns the task with the given ID from the task list.
func findTaskByID(tasks []TaskDefinition, id string) (*TaskDefinition, error) {
	for i := range tasks {
		if tasks[i].ID == id {
			return &tasks[i], nil
		}
	}
	return nil, fmt.Errorf("task not found: %s", id)
}

func isKnownHandler(h TaskHandler) bool {
	switch h {
	case HandleRaiseError, HandleRoute, HandleChatCompletion, HandleExecuteToolCalls, HandleNoop, HandleTools:
		return true
	}
	return false
}

func validateChain(tasks []TaskDefinition) error {
	if len(tasks) == 0 {
		return fmt.Errorf("chain has no tasks %w", errdefs.ErrBadRequest)
	}
	taskIDs := make(map[string]struct{}, len(tasks))
	for _, ct := range tasks {
		if ct.ID == "" {
			return fmt.Errorf("task ID cannot be empty %w", errdefs.ErrBadRequest)
		}
		if ct.ID == TermEnd {
			return fmt.Errorf("task ID cannot be '%s' %w", TermEnd, errdefs.ErrBadRequest)
		}
		if _, dup := taskIDs[ct.ID]; dup {
			return fmt.Errorf("duplicate task ID %q %w", ct.ID, errdefs.ErrBadRequest)
		}
		taskIDs[ct.ID] = struct{}{}
	}
	for _, ct := range tasks {
		// Handler must be one of the known handlers, else it fails opaquely at
		// runtime on the default switch arm.
		if !isKnownHandler(ct.Handler) {
			return fmt.Errorf("task %q: unknown handler %q %w", ct.ID, ct.Handler, errdefs.ErrBadRequest)
		}
		// A 'tools' task without a tools block silently execs an empty call.
		if ct.Handler == HandleTools && (ct.Tools == nil || ct.Tools.Name == "") {
			return fmt.Errorf("task %q: 'tools' handler requires a tools block with a name %w", ct.ID, errdefs.ErrBadRequest)
		}
		// on_failure must reference a real task ('end' is not resolvable at runtime).
		if ct.Transition.OnFailure != "" {
			if _, ok := taskIDs[ct.Transition.OnFailure]; !ok {
				return fmt.Errorf("task %q: on_failure references unknown task %q %w", ct.ID, ct.Transition.OnFailure, errdefs.ErrBadRequest)
			}
		}
		for _, br := range ct.Transition.Branches {
			// Operator must be a supported term — an empty/unknown operator never
			// matches (compare errors → swallowed), producing a silent dead branch.
			if _, err := ToOperatorTerm(string(br.Operator)); err != nil {
				return fmt.Errorf("task %q: %v %w", ct.ID, err, errdefs.ErrBadRequest)
			}
			// goto must reference a real task; empty or 'end' ends the chain.
			if br.Goto != "" && br.Goto != TermEnd {
				if _, ok := taskIDs[br.Goto]; !ok {
					return fmt.Errorf("task %q: branch goto references unknown task %q %w", ct.ID, br.Goto, errdefs.ErrBadRequest)
				}
			}
			if br.Operator != OpEdgeTraversedAtLeast {
				continue
			}
			if br.Edge == "" {
				return fmt.Errorf("task %q: branch with operator %q requires 'edge' field %w", ct.ID, OpEdgeTraversedAtLeast, errdefs.ErrBadRequest)
			}
			parts := strings.SplitN(br.Edge, "->", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return fmt.Errorf("task %q: branch edge %q must be of the form 'fromTaskID->toTaskID' %w", ct.ID, br.Edge, errdefs.ErrBadRequest)
			}
			if _, ok := taskIDs[parts[0]]; !ok {
				return fmt.Errorf("task %q: branch edge %q references unknown source task %q %w", ct.ID, br.Edge, parts[0], errdefs.ErrBadRequest)
			}
			if _, ok := taskIDs[parts[1]]; !ok && parts[1] != TermEnd {
				return fmt.Errorf("task %q: branch edge %q references unknown target task %q %w", ct.ID, br.Edge, parts[1], errdefs.ErrBadRequest)
			}
			if _, err := strconv.Atoi(strings.TrimSpace(br.When)); err != nil {
				return fmt.Errorf("task %q: branch with operator %q requires integer 'when' threshold, got %q %w", ct.ID, OpEdgeTraversedAtLeast, br.When, errdefs.ErrBadRequest)
			}
		}
	}
	return nil
}

func extractToolNamesFromOutput(output any, outputType DataType) []string {
	if outputType != DataTypeChatHistory {
		return nil
	}
	hist, ok := output.(ChatHistory)
	if !ok {
		return nil
	}
	for i := len(hist.Messages) - 1; i >= 0; i-- {
		m := hist.Messages[i]
		if m.Role != "assistant" || len(m.CallTools) == 0 {
			continue
		}
		seen := make(map[string]struct{}, len(m.CallTools))
		names := make([]string, 0, len(m.CallTools))
		for _, tc := range m.CallTools {
			name := tc.Function.Name
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
		if len(names) > 0 {
			return names
		}
	}
	return nil
}
