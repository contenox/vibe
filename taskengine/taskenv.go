package taskengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/libtracker"
	"github.com/getkin/kin-openapi/openapi3"
)

// DataType represents the type of data passed between tasks.
type DataType int

const (
	DataTypeAny DataType = iota
	DataTypeString
	DataTypeBool
	DataTypeInt
	DataTypeFloat
	DataTypeVector
	DataTypeSearchResults
	DataTypeJSON
	DataTypeChatHistory
	DataTypeOpenAIChat
	DataTypeOpenAIChatResponse
	DataTypeNil
)

// String returns the string representation of the data type.
func (d *DataType) String() string {
	switch *d {
	case DataTypeAny:
		return "any"
	case DataTypeString:
		return "string"
	case DataTypeBool:
		return "bool"
	case DataTypeInt:
		return "int"
	case DataTypeFloat:
		return "float"
	case DataTypeVector:
		return "vector"
	case DataTypeSearchResults:
		return "search_results"
	case DataTypeJSON:
		return "json"
	case DataTypeChatHistory:
		return "chat_history"
	case DataTypeOpenAIChat:
		return "openai_chat"
	case DataTypeOpenAIChatResponse:
		return "openai_chat_response"
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
	case "bool":
		return DataTypeBool, nil
	case "int":
		return DataTypeInt, nil
	case "float":
		return DataTypeFloat, nil
	case "vector":
		return DataTypeVector, nil
	case "search_results":
		return DataTypeSearchResults, nil
	case "json":
		return DataTypeJSON, nil
	case "chat_history":
		return DataTypeChatHistory, nil
	case "openai_chat":
		return DataTypeOpenAIChat, nil
	case "openai_chat_response":
		return DataTypeOpenAIChatResponse, nil
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

// ErrHookNotFound is returned when a named hook is not registered in any repo.
var ErrHookNotFound = errors.New("hook not found")

// ErrHookToolsUnavailable is returned when a hook is registered but its tool
// list cannot be loaded (e.g. MCP server unreachable or list-tools failed).
// ExecEnv treats this like a missing hook for tool preload: skip tools, continue the chain.
var ErrHookToolsUnavailable = errors.New("hook tools unavailable")

type hookToolsUnavailableError struct {
	hookName string
	cause    error
}

func (e *hookToolsUnavailableError) Error() string {
	return fmt.Sprintf("%s: hook %q: %v", ErrHookToolsUnavailable, e.hookName, e.cause)
}

func (e *hookToolsUnavailableError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return []error{ErrHookToolsUnavailable, e.cause}
}

// HookToolsUnavailable wraps cause as ErrHookToolsUnavailable for hookName (for errors.Is).
func HookToolsUnavailable(hookName string, cause error) error {
	if cause == nil {
		return nil
	}
	return &hookToolsUnavailableError{
		hookName: hookName,
		cause:    cause,
	}
}

// HookRepo defines interface for external system integrations and side effects.
type HookRepo interface {
	Exec(ctx context.Context, startingTime time.Time, input any, debug bool, args *HookCall) (any, DataType, error)
	HookRegistry
	HooksWithSchema
}

type HookProvider interface {
	HookRegistry
	HooksWithSchema
}

type HookRegistry interface {
	Supports(ctx context.Context) ([]string, error)
}

type HooksWithSchema interface {
	GetSchemasForSupportedHooks(ctx context.Context) (map[string]*openapi3.T, error)
	GetToolsForHookByName(ctx context.Context, name string) ([]Tool, error)
}

// SimpleEnv is the default implementation of EnvExecutor.
type SimpleEnv struct {
	exec         TaskExecutor
	tracker      libtracker.ActivityTracker
	inspector    Inspector
	hookProvider HookRepo
	eventSink    TaskEventSink
}

// NewEnv creates a new SimpleEnv with the given tracker and task executor.
func NewEnv(
	ctx context.Context,
	tracker libtracker.ActivityTracker,
	exec TaskExecutor,
	inspector Inspector,
	hookProvider HookRepo,
) (EnvExecutor, error) {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &SimpleEnv{
		exec:         exec,
		tracker:      tracker,
		inspector:    inspector,
		hookProvider: hookProvider,
		eventSink:    taskEventSinkFromContext(ctx),
	}, nil
}

type ChainContext struct {
	Tools       map[string]ToolWithResolution
	ClientTools []Tool
	Debug       bool
}

type ToolWithResolution struct {
	Tool
	HookName string
}

// ExecEnv executes the given chain with the provided input.
func (env SimpleEnv) ExecEnv(ctx context.Context, chain *TaskChainDefinition, input any, dataType DataType) (result any, resultType DataType, history []CapturedStateUnit, retErr error) {
	reportErrChain, _, endChain := env.tracker.Start(ctx, "chain_exec", chain.ID, "chain_id", chain.ID)
	defer endChain()

	stack := env.inspector.Start(ctx)
	defer func() {
		history = stack.GetExecutionHistory()
		chainEvent := NewTaskEvent(ctx, TaskEventChainCompleted)
		chainEvent.ChainID = chain.ID
		chainEvent.OutputType = resultType.String()
		if retErr != nil {
			chainEvent.Kind = TaskEventChainFailed
			chainEvent.Error = retErr.Error()
			chainEvent.OutputType = ""
		}
		publishTaskEventBestEffort(ctx, env.eventSink, chainEvent)
	}()
	chainStarted := NewTaskEvent(ctx, TaskEventChainStarted)
	chainStarted.ChainID = chain.ID
	publishTaskEventBestEffort(ctx, env.eventSink, chainStarted)

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

	clientTools := []Tool{}
	if dataType == DataTypeOpenAIChat {
		req, ok := input.(OpenAIChatRequest)
		if !ok {
			return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: invalid input type", currentTask.ID)
		}
		clientTools = req.Tools
	}

	chainContext := &ChainContext{
		Tools:       map[string]ToolWithResolution{},
		ClientTools: clientTools,
		Debug:       chain.Debug,
	}
	filter := map[string]ToolWithResolution{}
	for _, task := range chain.Tasks {
		if task.ExecuteConfig == nil {
			continue
		}
		hookNames, err := resolveHookNames(ctx, task.ExecuteConfig.Hooks, env.hookProvider)
		if err != nil {
			return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: failed to resolve hooks: %w", currentTask.ID, err)
		}
		for _, hookName := range hookNames {
			// Build a task-scoped context carrying any chain-level policy args for
			// this hook. WithHookArgs copies the map, so the stored value is
			// immutable and safe to read concurrently without locks.
			toolCtx := ctx
			// 1. execute_config.hook_policies is the primary mechanism — chain authors
			//    set per-hook policy here without touching the Hook field.
			if task.ExecuteConfig != nil {
				if policy, ok := task.ExecuteConfig.HookPolicies[hookName]; ok && len(policy) > 0 {
					toolCtx = WithHookArgs(toolCtx, hookName, policy)
				}
			}
			// 2. task.Hook.Args is the secondary mechanism for HandleHook tasks.
			if task.Hook != nil && task.Hook.Name == hookName && len(task.Hook.Args) > 0 {
				toolCtx = WithHookArgs(toolCtx, hookName, task.Hook.Args)
			}
			hookTools, err := env.hookProvider.GetToolsForHookByName(toolCtx, hookName)
			if err != nil {
				if errors.Is(err, ErrHookNotFound) {
					// Hook not registered (e.g. local_shell disabled via --enable-local-exec=false).
					// The model simply won't see this tool.
					continue
				}
				if errors.Is(err, ErrHookToolsUnavailable) {
					reportErrChain(err)
					continue
				}
				return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: failed to get tools for hook %s: %w", currentTask.ID, hookName, err)
			}
			for _, tool := range hookTools {
				tool.Function.Name = hookName + "." + tool.Function.Name
				filter[tool.Function.Name] = ToolWithResolution{Tool: tool, HookName: hookName}
			}
		}
	}

	for _, twr := range filter {
		chainContext.Tools[twr.Function.Name] = twr
	}
	chainContext.ClientTools = clientTools

	for {
		if ctx.Err() != nil {
			return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: context canceled", currentTask.ID)
		}

		// Determine task input
		taskInput := output
		taskInputType := outputType
		inputVar = currentTask.ID
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

		// Render prompt template if exists
		if currentTask.PromptTemplate != "" {
			rendered, err := renderTemplate(currentTask.PromptTemplate, vars)
			if err != nil {
				return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: template error: %v", currentTask.ID, err)
			}
			taskInput = rendered
			taskInputType = DataTypeString
		}
		maxRetries := max(currentTask.RetryOnFailure, 0)

		for retry := 0; retry <= maxRetries; retry++ {
			if stack.HasBreakpoint(currentTask.ID) {
				return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: breakpoint set", currentTask.ID)
			}

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
			publishTaskEventBestEffort(taskCtx, env.eventSink, stepStarted)
			reportErrAttempt, reportChangeAttempt, endAttempt := env.tracker.Start(
				taskCtx,
				"task_attempt",
				currentTask.ID,
				"retry", retry,
				"task_type", currentTask.Handler,
			)

			startTime := time.Now().UTC()

			output, outputType, transitionEval, taskErr = env.exec.TaskExec(taskCtx, startingTime, int(chain.TokenLimit), chainContext, currentTask, taskInput, taskInputType)
			if taskErr != nil {
				taskErr = fmt.Errorf("task %s: %w", currentTask.ID, taskErr)
				reportErrAttempt(taskErr)
			}
			endAttempt()
			if cancel != nil {
				cancel()
			}
			duration := time.Since(startTime)
			errState := ErrorResponse{
				ErrorInternal: taskErr,
			}
			if taskErr != nil {
				errState.Error = taskErr.Error()
			}
			// Record execution step
			step := CapturedStateUnit{
				TaskID:      currentTask.ID,
				TaskHandler: currentTask.Handler.String(),
				InputType:   taskInputType,
				OutputType:  outputType,
				InputVar:    inputVar,
				Transition:  transitionEval,
				Duration:    duration,
				Error:       errState,
			}
			if chain.Debug {
				step.Input = fmt.Sprintf("%v", taskInput)
				outputBytes, err := json.Marshal(output)
				if err == nil {
					step.Output = string(outputBytes)
				} else {
					step.Output = fmt.Sprintf("%v", output)
				}
			}
			stack.RecordStep(step)
			stepEvent := NewTaskEvent(taskCtx, TaskEventStepCompleted)
			stepEvent.OutputType = outputType.String()
			stepEvent.Transition = transitionEval
			// Drain any UI hints emitted by hooks during this step (Phase 5
			// of the canvas-vision plan). Hints go out exactly once per
			// publish — Drain() also clears them so the next step starts
			// clean. Failed steps still publish hints because a hook may
			// have produced a useful widget before the step's terminal
			// error (e.g. a partial file_view before a downstream parse fail).
			if hints := drainWidgetHints(taskCtx); len(hints) > 0 {
				stepEvent.Attachments = hints
			}

			if taskErr != nil {
				stepEvent.Kind = TaskEventStepFailed
				stepEvent.Error = taskErr.Error()
				stepEvent.OutputType = ""
				publishTaskEventBestEffort(taskCtx, env.eventSink, stepEvent)
				reportErrAttempt(taskErr)
				continue
			}
			publishTaskEventBestEffort(taskCtx, env.eventSink, stepEvent)

			// Report successful attempt
			reportChangeAttempt(currentTask.ID, output)
			break
		}

		if taskErr != nil {
			if currentTask.Transition.OnFailure != "" {
				previousTaskID := currentTask.ID
				currentTask, err = findTaskByID(chain.Tasks, currentTask.Transition.OnFailure)
				if err != nil {
					return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("error transition target not found: %v", err)
				}
				// Track error-based transition
				_, reportChangeErrTransition, endErrTransition := env.tracker.Start(
					ctx,
					"next_task",
					previousTaskID,
					"next_task", currentTask.ID,
					"reason", "error",
				)
				reportChangeErrTransition(currentTask.ID, taskErr)
				endErrTransition() // Fix 2: direct call, not defer — defers inside loops leak
				continue
			}
			return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s failed after %d retries: %v", currentTask.ID, maxRetries, taskErr)
		}

		// Handle print statement
		if currentTask.Print != "" {
			printMsg, err := renderTemplate(currentTask.Print, vars)
			if err != nil {
				return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: print template error: %v", currentTask.ID, err)
			}
			fmt.Println(printMsg)
		}

		// Evaluate transitions and get chosen branch
		nextTaskID, chosenBranch, err := env.evaluateTransitions(ctx, currentTask.ID, currentTask.Transition, transitionEval)
		if err != nil {
			return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: transition error: %v", currentTask.ID, err)
		}

		// Handle branch-specific compose
		if chosenBranch.Compose != nil {
			compose := chosenBranch.Compose

			// Validate compose variables exist
			rightVal, exists := vars[compose.WithVar]
			if !exists {
				return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("compose right_var %q not found", compose.WithVar)
			}
			rightType, exists := varTypes[compose.WithVar]
			if !exists {
				return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("compose right_var %q missing type info", compose.WithVar)
			}

			// Determine strategy with fallback
			strategy := compose.Strategy
			if strategy == "" {
				strategy = env.determineDefaultComposeStrategy(outputType, rightType)
			}

			// Execute compose operation
			composedOutput, composedType, err := env.executeCompose(strategy, output, outputType, rightVal, rightType)
			if err != nil {
				return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("compose failed: %w", err)
			}

			// Update output for next task
			output = composedOutput
			outputType = composedType

			// Store composed result in a branch-specific variable
			branchVarName := fmt.Sprintf("%s_%s_composed", currentTask.ID, sanitizeBranchName(chosenBranch.When))
			vars[branchVarName] = output
			varTypes[branchVarName] = outputType
		}
		// Update execution variables with raw task output
		vars["previous_output"] = output
		vars[currentTask.ID] = output
		varTypes["previous_output"] = outputType
		varTypes[currentTask.ID] = outputType

		if nextTaskID == "" || nextTaskID == TermEnd {
			finalOutput = output
			// Track final output
			_, reportChangeFinal, endFinal := env.tracker.Start(
				ctx,
				"chain_complete",
				"chain")
			reportChangeFinal("chain", finalOutput)
			endFinal() // Fix 2: direct call, not defer
			break
		}

		// Track normal transition to next task
		_, reportChangeTransition, endTransition := env.tracker.Start(
			ctx,
			"next_task",
			currentTask.ID,
			"next_task", nextTaskID,
		)
		reportChangeTransition(nextTaskID, transitionEval)
		endTransition() // Fix 2: direct call, not defer

		// Find next task
		currentTask, err = findTaskByID(chain.Tasks, nextTaskID)
		if err != nil {
			return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("next task %s not found: %v", nextTaskID, err)
		}
	}

	normOut, normDT, normErr := NormalizeFinalChainOutput(finalOutput, outputType)
	if normErr != nil {
		return nil, DataTypeAny, nil, normErr
	}
	return normOut, normDT, nil, nil
}

// Helper methods for compose operations
func (env SimpleEnv) determineDefaultComposeStrategy(leftType, rightType DataType) string {
	if leftType == DataTypeChatHistory && rightType == DataTypeChatHistory {
		return "merge_chat_histories"
	}
	if (leftType == DataTypeString && rightType == DataTypeChatHistory) ||
		(leftType == DataTypeChatHistory && rightType == DataTypeString) {
		return "append_string_to_chat_history"
	}
	return "override"
}

func (env SimpleEnv) executeCompose(strategy string, leftVal any, leftType DataType, rightVal any, rightType DataType) (any, DataType, error) {
	switch strategy {
	case "override":
		return env.composeOverride(leftVal, leftType, rightVal, rightType)
	case "append_string_to_chat_history":
		return env.composeAppendStringToChatHistory(leftVal, leftType, rightVal, rightType)
	case "merge_chat_histories":
		return env.composeMergeChatHistories(leftVal, leftType, rightVal, rightType)
	default:
		return nil, DataTypeAny, fmt.Errorf("unsupported compose strategy: %q", strategy)
	}
}

func (env SimpleEnv) composeOverride(leftVal any, leftType DataType, rightVal any, rightType DataType) (any, DataType, error) {
	// If both are maps, merge them with left overriding right
	leftMap, okLeft := leftVal.(map[string]any)
	rightMap, okRight := rightVal.(map[string]any)

	if okLeft && okRight {
		// Start with the "right" (older) and override with "left" (newer)
		result := make(map[string]any, len(rightMap)+len(leftMap))
		for k, v := range rightMap {
			result[k] = v
		}
		for k, v := range leftMap {
			result[k] = v
		}
		return result, DataTypeJSON, nil
	}

	// If only one is a map, prefer the map over non-map
	if okLeft {
		return leftVal, leftType, nil
	}
	if okRight {
		return rightVal, rightType, nil
	}

	// Neither is a map, return the left value (current output)
	return leftVal, leftType, nil
}

func (env SimpleEnv) composeAppendStringToChatHistory(leftVal any, leftType DataType, rightVal any, rightType DataType) (any, DataType, error) {
	var strVal string
	var chatHist ChatHistory

	if leftType == DataTypeString && rightType == DataTypeChatHistory {
		// left = new assistant text, right = existing history
		strVal = leftVal.(string)
		chatHist = rightVal.(ChatHistory)
	} else if leftType == DataTypeChatHistory && rightType == DataTypeString {
		// left = existing history, right = new assistant text
		strVal = rightVal.(string)
		chatHist = leftVal.(ChatHistory)
	} else {
		return nil, DataTypeAny, fmt.Errorf("invalid types for append_string_to_chat_history %s - %s", leftType.String(), rightType.String())
	}

	// Append assistant message to the END of the history
	newMsg := Message{
		Content:   strVal,
		Role:      "assistant",
		Timestamp: time.Now().UTC(),
	}

	// Fix 6: force reallocation so we never mutate the backing array of the
	// original slice, which could corrupt shared state on branching/retries.
	newMessages := append([]Message(nil), chatHist.Messages...)
	result := ChatHistory{
		Messages:     append(newMessages, newMsg),
		Model:        chatHist.Model,
		OutputTokens: chatHist.OutputTokens,
		InputTokens:  chatHist.InputTokens,
	}

	return result, DataTypeChatHistory, nil
}

func (env SimpleEnv) composeMergeChatHistories(leftVal any, leftType DataType, rightVal any, rightType DataType) (any, DataType, error) {
	if leftType != DataTypeChatHistory || rightType != DataTypeChatHistory {
		return nil, DataTypeAny, fmt.Errorf("both values must be ChatHistory for merge")
	}

	leftHist := leftVal.(ChatHistory)   // current task output (task2)
	rightHist := rightVal.(ChatHistory) // WithVar (task1)

	// Right-first, then left (as tests expect)
	merged := ChatHistory{
		Messages:     append(append([]Message{}, rightHist.Messages...), leftHist.Messages...),
		InputTokens:  rightHist.InputTokens + leftHist.InputTokens,
		OutputTokens: rightHist.OutputTokens + leftHist.OutputTokens,
	}

	// Only keep model if identical; otherwise empty
	if rightHist.Model == leftHist.Model {
		merged.Model = leftHist.Model
	} else {
		merged.Model = ""
	}

	return merged, DataTypeChatHistory, nil
}

func sanitizeBranchName(branchName string) string {
	safe := strings.ReplaceAll(branchName, " ", "_")
	safe = strings.ReplaceAll(safe, "-", "_")
	safe = strings.ToLower(safe)
	return safe
}

func renderTemplate(tmplStr string, vars any) (string, error) {
	tmpl, err := template.New("prompt").Parse(tmplStr)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (exe SimpleEnv) evaluateTransitions(_ context.Context, _ string, transition TaskTransition, eval string) (string, *TransitionBranch, error) {
	// First check explicit matches
	for _, branch := range transition.Branches {
		if branch.Operator == OpDefault {
			continue
		}

		match, err := compare(branch.Operator, eval, branch.When)
		if err != nil {
			// Fix 8: treat parse errors as non-match so OpDefault can still fire.
			// Returning an error here would bypass the safe fallback branch entirely.
			match = false
		}
		if match {
			return branch.Goto, &branch, nil
		}
	}

	// Then check for default
	for _, branch := range transition.Branches {
		if branch.Operator == OpDefault {
			return branch.Goto, &branch, nil
		}
	}

	return "", nil, fmt.Errorf("no matching transition found for eval: %s", eval)
}

// parseNumber attempts to parse a string as either an integer or float.
func parseNumber(s string) (float64, error) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\"", "")
	s = strings.ReplaceAll(s, "'", "")
	s = strings.ReplaceAll(s, " ", "")

	if s == "" {
		return 0, fmt.Errorf("cannot parse number from empty string")
	}

	if num, err := strconv.ParseFloat(s, 64); err == nil {
		return num, nil
	}

	re := regexp.MustCompile(`[-+]?\d*\.?\d+`)
	match := re.FindString(s)
	if match == "" {
		return 0, fmt.Errorf("no valid number found in %q", s)
	}

	num, err := strconv.ParseFloat(match, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse number from %q (extracted %q): %w", s, match, err)
	}
	return num, nil
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
	case OpGreaterThan, OpGt:
		resNum, err := parseNumber(response)
		if err != nil {
			return false, err
		}
		targetNum, err := parseNumber(when)
		if err != nil {
			return false, err
		}
		return resNum > targetNum, nil
	case OpLessThan, OpLt:
		resNum, err := parseNumber(response)
		if err != nil {
			return false, err
		}
		targetNum, err := parseNumber(when)
		if err != nil {
			return false, err
		}
		return resNum < targetNum, nil
	case OpInRange:
		// Fix 11: use regex so negative bounds like "-10--2" or "-5-5" parse correctly.
		// strings.Split(when, "-") breaks on any leading '-' in a negative number.
		rangeRe := regexp.MustCompile(`^(-?\d+(?:\.\d+)?)-(-?\d+(?:\.\d+)?)$`)
		m := rangeRe.FindStringSubmatch(strings.TrimSpace(when))
		if m == nil {
			return false, fmt.Errorf("invalid inrange format: %q (expected 'min-max')", when)
		}
		lower, err := parseNumber(m[1])
		if err != nil {
			return false, fmt.Errorf("invalid lower bound in range %q: %w", when, err)
		}
		upper, err := parseNumber(m[2])
		if err != nil {
			return false, fmt.Errorf("invalid upper bound in range %q: %w", when, err)
		}
		if lower > upper {
			return false, fmt.Errorf("invalid range: lower bound %f > upper bound %f", lower, upper)
		}
		resNum, err := parseNumber(response)
		if err != nil {
			return false, fmt.Errorf("failed to parse response as number: %q: %w", response, err)
		}
		return resNum >= lower && resNum <= upper, nil
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

func validateChain(tasks []TaskDefinition) error {
	if len(tasks) == 0 {
		return fmt.Errorf("chain has no tasks %w", apiframework.ErrBadRequest)
	}
	for _, ct := range tasks {
		if ct.ID == "" || ct.ID == TermEnd {
			if ct.ID == "" {
				return fmt.Errorf("task ID cannot be empty %w", apiframework.ErrBadRequest)
			}
			if ct.ID == TermEnd {
				return fmt.Errorf("task ID cannot be '%s' %w", TermEnd, apiframework.ErrBadRequest)
			}
		}
	}
	return nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
