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

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/libtracker"
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
}

// NewEnv creates a new SimpleEnv with the given tracker and task executor.
func NewEnv(
	_ context.Context,
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
func (env SimpleEnv) ExecEnv(ctx context.Context, chain *TaskChainDefinition, input any, dataType DataType) (any, DataType, []CapturedStateUnit, error) {
	stack := env.inspector.Start(ctx)

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
		if task.ExecuteConfig != nil && len(task.ExecuteConfig.Hooks) > 0 {
			for _, hookName := range task.ExecuteConfig.Hooks {
				hookTools, err := env.hookProvider.GetToolsForHookByName(ctx, hookName)
				if err != nil {
					return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: failed to get tools for hook %s: %v", currentTask.ID, hookName, err)
				}
				for _, tool := range hookTools {
					tool.Function.Name = hookName + "." + tool.Function.Name
					filter[tool.Function.Name] = ToolWithResolution{Tool: tool, HookName: hookName}
				}
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

	retryLoop:
		for retry := 0; retry <= maxRetries; retry++ {
			if stack.HasBreakpoint(currentTask.ID) {
				return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: breakpoint set", currentTask.ID)
			}

			// Track task attempt start
			taskCtx := context.Background()
			taskCtx = libtracker.CopyTrackingValues(ctx, taskCtx)

			var cancel context.CancelFunc
			if currentTask.Timeout != "" {
				timeout, err := time.ParseDuration(currentTask.Timeout)
				if err != nil {
					return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("task %s: invalid timeout: %v", currentTask.ID, err)
				}
				taskCtx, cancel = context.WithTimeout(ctx, timeout)
			}
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

			if taskErr != nil {
				reportErrAttempt(taskErr)
				continue retryLoop
			}

			// Report successful attempt
			reportChangeAttempt(currentTask.ID, output)
			break retryLoop
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
				defer endErrTransition()
				reportChangeErrTransition(currentTask.ID, taskErr)
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
			defer endFinal()
			reportChangeFinal("chain", finalOutput)
			break
		}

		// Track normal transition to next task
		_, reportChangeTransition, endTransition := env.tracker.Start(
			ctx,
			"next_task",
			currentTask.ID,
			"next_task", nextTaskID,
		)
		defer endTransition()
		reportChangeTransition(nextTaskID, transitionEval)

		// Find next task
		currentTask, err = findTaskByID(chain.Tasks, nextTaskID)
		if err != nil {
			return nil, DataTypeAny, stack.GetExecutionHistory(), fmt.Errorf("next task %s not found: %v", nextTaskID, err)
		}
	}

	return finalOutput, outputType, stack.GetExecutionHistory(), nil
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

	result := ChatHistory{
		Messages:     append(chatHist.Messages, newMsg),
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

func (exe SimpleEnv) evaluateTransitions(ctx context.Context, taskID string, transition TaskTransition, eval string) (string, *TransitionBranch, error) {
	// First check explicit matches
	for _, branch := range transition.Branches {
		if branch.Operator == OpDefault {
			continue
		}

		match, err := compare(branch.Operator, eval, branch.When)
		if err != nil {
			return "", nil, err
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
		parts := strings.Split(when, "-")
		if len(parts) != 2 {
			return false, fmt.Errorf("invalid inrange format: %s (expected 'min-max')", when)
		}

		lower, err := parseNumber(strings.TrimSpace(parts[0]))
		if err != nil {
			return false, fmt.Errorf("invalid lower bound in range %q: %w", when, err)
		}

		upper, err := parseNumber(strings.TrimSpace(parts[1]))
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
