package taskengine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine/llmretry"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/llmrepo"
	libmodelprovider "github.com/contenox/beam/internal/models/modelrepo"
	"github.com/google/uuid"
)

// TaskExecutor executes individual tasks within a workflow.
// Implementations should handle all task types and return appropriate outputs.
type TaskExecutor interface {
	// TaskExec executes a single task with the given input and data type.
	// Returns:
	// - output: The processed task result
	// - outputType: The data type of the output
	// - transitionEval: String used for transition evaluation
	// - error: Any execution error encountered
	//
	// Parameters:
	// - ctx: Context for cancellation and timeouts
	// - startingTime: Chain start time for consistent timing
	// - ctxLength: Token context length limit for LLM operations
	// - chainContext: Immutable context of the chain
	// - currentTask: The task definition to execute
	// - input: Task input data
	// - dataType: Type of the input data
	TaskExec(ctx context.Context, startingTime time.Time, ctxLength int, chainContext *ChainContext, currentTask *TaskDefinition, input any, dataType DataType) (any, DataType, string, error)
}

// ensureUniqueToolCallID ensures a tool call ID is unique by appending
// a short UUID suffix. This prevents duplicate IDs that can confuse
// ACP clients when the model reuses IDs across turns.
func ensureUniqueToolCallID(id string) string {
	if id == "" {
		return uuid.NewString()
	}
	// Append 4-char UUID suffix to ensure uniqueness
	return id + "-" + uuid.NewString()[:4]
}

// SimpleExec is a basic implementation of TaskExecutor.
// It executes chat completion, tools, route, raise_error, and noop tasks.
type SimpleExec struct {
	repo          llmrepo.ModelRepo
	toolsProvider ToolsRepo
	tracker       libtracker.ActivityTracker
	eventSink     TaskEventSink
}

// NewExec creates a new SimpleExec instance
func NewExec(
	ctx context.Context,
	repo llmrepo.ModelRepo,
	toolsProvider ToolsRepo,
	tracker libtracker.ActivityTracker,
) (TaskExecutor, error) {
	if toolsProvider == nil {
		return nil, fmt.Errorf("tools provider is nil")
	}
	if repo == nil {
		return nil, fmt.Errorf("repo executor is nil")
	}
	return &SimpleExec{
		toolsProvider: toolsProvider,
		repo:          repo,
		tracker:       tracker,
		eventSink:     taskEventSinkFromContext(ctx),
	}, nil
}

func (exe *SimpleExec) publishStepChunk(ctx context.Context, meta llmrepo.Meta, content, thinking string) {
	if content == "" && thinking == "" {
		return
	}
	if exe.eventSink == nil || !exe.eventSink.Wants(TaskEventStepChunk) {
		return
	}
	_, _, end := exe.tracker.Start(ctx, "publish_step_chunk", "task_event",
		"content_len", len(content),
		"thinking_len", len(thinking),
		"model", meta.ModelName,
	)
	defer end()
	event := NewTaskEvent(ctx, TaskEventStepChunk)
	event.ModelName = meta.ModelName
	event.ProviderType = meta.ProviderType
	event.BackendID = meta.BackendID
	event.Content = content
	event.Thinking = thinking
	publishTaskEventBestEffort(ctx, exe.tracker, exe.eventSink, event)
}

// countTokensAndCheckLimit counts tokens for text and checks against context limit.
// If ctxLength <= 0 (chain token_limit not set and no per-request/session override),
// no limit is enforced at this layer. The prompt proceeds; the underlying model/provider
// is responsible for any limits. Usage indicators will use model ContextLength as fallback size.
func (exe *SimpleExec) countTokensAndCheckLimit(ctx context.Context, modelName string, text string, ctxLength int) (int, error) {
	if ctxLength <= 0 {
		return 0, nil // No limit enforced
	}

	tokenCount, err := exe.repo.CountTokens(ctx, modelName, text)
	if err != nil {
		return 0, fmt.Errorf("token count failed: %w", err)
	}

	if tokenCount > ctxLength {
		return tokenCount, fmt.Errorf("%w: input token count %d > %d", ErrContextLengthExceeded, tokenCount, ctxLength)
	}

	return tokenCount, nil
}

// countChatHistoryTokens counts total tokens in chat history.
// If ctxLength <= 0 (no effective session token budget set on the chain or override),
// we do not enforce or slide here. See main check in executeLLM for Shift behavior when a
// positive budget is present. Indicators fall back to model-reported context window.
func (exe *SimpleExec) countChatHistoryTokens(ctx context.Context, modelName string, history ChatHistory, ctxLength int) (int, error) {
	if ctxLength <= 0 {
		return 0, nil // No limit to enforce
	}

	// If tokens are already calculated and valid, use them
	if history.InputTokens > 0 && history.OutputTokens > 0 {
		totalTokens := history.InputTokens + history.OutputTokens
		if totalTokens > ctxLength {
			return totalTokens, fmt.Errorf("%w: chat history token count %d > %d", ErrContextLengthExceeded, totalTokens, ctxLength)
		}
		return totalTokens, nil
	}

	// Count tokens for each message
	totalTokens := 0
	for _, msg := range history.Messages {
		tokenCount, err := exe.repo.CountTokens(ctx, modelName, msg.Content)
		if err != nil {
			return 0, fmt.Errorf("token count failed for message: %w", err)
		}
		totalTokens += tokenCount
	}

	if totalTokens > ctxLength {
		return totalTokens, fmt.Errorf("%w: chat history token count %d > %d", ErrContextLengthExceeded, totalTokens, ctxLength)
	}

	return totalTokens, nil
}

func reserveOutputTokens(llmCall *LLMExecutionConfig, ctxLength int) int {
	if llmCall.MaxTokens != nil && *llmCall.MaxTokens > 0 {
		return *llmCall.MaxTokens
	}
	if ctxLength >= 8 {
		return ctxLength / 8
	}
	return 0
}

func requestedContextRequirement(ctx context.Context, actualTokens int) int {
	if requested := RequestedContextLengthFromContext(ctx); requested > actualTokens {
		return requested
	}
	return actualTokens
}

func (exe *SimpleExec) shiftMessagesToFit(ctx context.Context, modelName string, msgs []Message, budget int) ([]Message, int, error) {
	toks := make([]int, len(msgs))
	for i, m := range msgs {
		n, err := exe.repo.CountTokens(ctx, modelName, m.Content)
		if err != nil {
			return nil, 0, fmt.Errorf("token count failed: %w", err)
		}
		toks[i] = n
	}

	type unit struct {
		idx    []int
		tokens int
		system bool
	}
	var units []unit
	for i := 0; i < len(msgs); i++ {
		switch {
		case msgs[i].Role == "system":
			units = append(units, unit{idx: []int{i}, tokens: toks[i], system: true})
		case msgs[i].Role == "tool":
		case msgs[i].Role == "assistant" && len(msgs[i].CallTools) > 0:
			u := unit{idx: []int{i}, tokens: toks[i]}
			for j := i + 1; j < len(msgs) && msgs[j].Role == "tool"; j++ {
				u.idx = append(u.idx, j)
				u.tokens += toks[j]
				i = j
			}
			units = append(units, u)
		default:
			units = append(units, unit{idx: []int{i}, tokens: toks[i]})
		}
	}

	systemTokens := 0
	for _, u := range units {
		if u.system {
			systemTokens += u.tokens
		}
	}
	if systemTokens > budget {
		return nil, 0, ErrContextLengthExceeded
	}

	keepUnit := make([]bool, len(units))
	used := systemTokens
	for k := range units {
		if units[k].system {
			keepUnit[k] = true
		}
	}
	for k := len(units) - 1; k >= 0; k-- {
		if units[k].system {
			continue
		}
		if used+units[k].tokens > budget {
			break
		}
		keepUnit[k] = true
		used += units[k].tokens
	}

	keepIdx := make([]bool, len(msgs))
	for k, u := range units {
		if keepUnit[k] {
			for _, ix := range u.idx {
				keepIdx[ix] = true
			}
		}
	}
	out := make([]Message, 0, len(msgs))
	for i := range msgs {
		if keepIdx[i] {
			out = append(out, msgs[i])
		}
	}

	out = repairToolCallPairing(out)
	if len(out) == 0 {
		return nil, 0, ErrContextLengthExceeded
	}

	total := 0
	for _, m := range out {
		n, err := exe.repo.CountTokens(ctx, modelName, m.Content)
		if err != nil {
			return nil, 0, fmt.Errorf("token count failed: %w", err)
		}
		total += n
	}
	return out, total, nil
}

// getPrimaryModel extracts the primary model name from execution config
// temperatureValue unwraps an optional temperature. ok is false when the field
// is nil (unset), in which case callers must NOT send a temperature so the
// provider default applies. See LLMExecutionConfig.Temperature.
func temperatureValue(t *float32) (float32, bool) {
	if t == nil {
		return 0, false
	}
	return *t, true
}

func GetPrimaryModel(llmCall *LLMExecutionConfig) string {
	if llmCall.Model != "" {
		return llmCall.Model
	}
	if len(llmCall.Models) > 0 {
		return llmCall.Models[0]
	}
	return "default" // Fallback model name for token counting
}

func routeHistoryPrompt(history ChatHistory) string {
	lines := make([]string, 0, len(history.Messages))
	for _, msg := range history.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			role = "message"
		}
		lines = append(lines, role+": "+strings.TrimSpace(msg.Content))
	}
	return strings.Join(lines, "\n")
}

// Prompt resolves a model client using the resolver policy and sends the prompt
// to be executed. Returns the trimmed response string or an error.
func (exe *SimpleExec) Prompt(ctx context.Context, systemInstruction string, llmCall LLMExecutionConfig, prompt string, ctxLength int) (string, error) {
	reportErr, reportChange, end := exe.tracker.Start(ctx, "SimpleExec", "prompt_model",
		"model_name", llmCall.Model,
		"model_names", llmCall.Models,
		"provider_types", llmCall.Providers,
		"provider_type", llmCall.Provider,
	)
	defer end()

	if prompt == "" {
		err := fmt.Errorf("unprocessable empty prompt")
		reportErr(err)
		return "", err
	}

	// Count tokens and check limits
	modelName := GetPrimaryModel(&llmCall)
	combinedText := systemInstruction + "\n" + prompt
	promptTokens, err := exe.countTokensAndCheckLimit(ctx, modelName, combinedText, ctxLength)
	if err != nil {
		reportErr(err)
		return "", err
	}

	providerNames := []string{}
	if llmCall.Provider != "" {
		providerNames = append(providerNames, llmCall.Provider)
	}
	if llmCall.Providers != nil {
		providerNames = append(providerNames, llmCall.Providers...)
	}
	modelNames := []string{}
	if llmCall.Model != "" {
		modelNames = append(modelNames, llmCall.Model)
	}
	if llmCall.Models != nil {
		modelNames = append(modelNames, llmCall.Models...)
	}
	req := llmrepo.Request{
		ProviderTypes: providerNames,
		ModelNames:    modelNames,
		ContextLength: requestedContextRequirement(ctx, promptTokens),
		Tracker:       exe.tracker,
	}

	// Keep prompt/route temperature behavior stable: unset is sent as 0.
	// Preserve that — route handlers depend on temp-0 determinism to emit exactly
	// one label. Only the chat path treats nil as "use the provider default".
	promptTemp, _ := temperatureValue(llmCall.Temperature)
	streamArgs := []libmodelprovider.ChatArgument{
		libmodelprovider.WithTemperature(float64(promptTemp)),
	}
	if llmCall.Think != "" {
		streamArgs = append(streamArgs, libmodelprovider.WithThink(llmCall.Think))
	}
	if llmCall.Shift {
		streamArgs = append(streamArgs, libmodelprovider.WithShift{})
	}

	// ONE execution path (mirroring executeLLM): prompts stream and are
	// assembled engine-side; the sink only observes. PromptExecute survives as
	// the fallback when stream setup fails, taken identically with or without
	// a sink.
	messages := []libmodelprovider.Message{}
	if systemInstruction != "" {
		messages = append(messages, libmodelprovider.Message{Role: "system", Content: systemInstruction})
	}
	messages = append(messages, libmodelprovider.Message{Role: "user", Content: prompt})

	stream, meta, err := exe.repo.Stream(ctx, req, messages, streamArgs...)
	if err == nil {
		asm := libmodelprovider.NewStreamAssembler(meta.ProviderType, meta.ModelName)
		for parcel := range stream {
			if err := asm.Consume(parcel); err != nil {
				err = fmt.Errorf("prompt stream failed: %w", err)
				reportErr(err)
				return "", err
			}
			exe.publishStepChunk(ctx, meta, parcel.Data, parcel.Thinking)
		}
		res, err := asm.Result()
		if err != nil {
			err = fmt.Errorf("prompt stream failed: %w", err)
			reportErr(err)
			return "", err
		}
		return strings.TrimSpace(res.Content), nil
	}

	response, _, err := exe.promptWithRetry(ctx, reportChange, &llmCall, req, systemInstruction, prompt)
	if err != nil {
		err = fmt.Errorf("prompt execution failed: %w", err)
		reportErr(err)
		return "", err
	}

	return strings.TrimSpace(response), nil
}

// promptWithRetry wraps repo.PromptExecute with [llmretry.Do] when the task's
// LLMExecutionConfig declares a RetryPolicy. Used by the route handler's
// single-shot classification call.
// The streaming branch in [Prompt] is intentionally not retried because
// parcels may already have been published to the user; only the
// non-streaming fallback path is wrapped.
//
// Mirrors [chatWithRetry] except it calls PromptExecute, which has no tool
// dispatch.
func (exe *SimpleExec) promptWithRetry(
	ctx context.Context,
	reportChange func(id string, data any),
	llmCall *LLMExecutionConfig,
	req llmrepo.Request,
	systemInstruction, prompt string,
) (string, llmrepo.Meta, error) {
	policy := llmretry.RetryPolicy{}
	if llmCall != nil && llmCall.RetryPolicy != nil {
		policy = *llmCall.RetryPolicy
	}
	primary := GetPrimaryModel(llmCall)
	type promptResult struct {
		response string
		meta     llmrepo.Meta
	}
	attempt := 0
	var prevErr error
	result, outcome, err := llmretry.Do(ctx, policy, primary, func(modelID string) (any, error) {
		attempt++
		if attempt > 1 && reportChange != nil {
			reportChange("retry_attempt", map[string]any{
				"attempt":          attempt,
				"model":            modelID,
				"prev_error_class": string(llmretry.ClassifyError(prevErr)),
				"prev_error":       prevErr.Error(),
			})
		}
		callReq := req
		if modelID != "" && modelID != primary {
			callReq.ModelNames = []string{modelID}
		}
		promptTemp, _ := temperatureValue(llmCall.Temperature)
		r, m, e := exe.repo.PromptExecute(ctx, callReq, systemInstruction, promptTemp, prompt)
		prevErr = e
		if e != nil {
			return nil, e
		}
		return promptResult{response: r, meta: m}, nil
	})
	appendRetryOutcome(ctx, outcome)
	if reportChange != nil {
		reportChange("retry_outcome", map[string]any{
			"attempts":         outcome.Attempts,
			"used_fallback":    outcome.UsedFallback,
			"last_error_class": string(outcome.LastErrorClass),
			"elapsed":          outcome.Elapsed.String(),
		})
	}
	if err != nil {
		return "", llmrepo.Meta{}, err
	}
	pr := result.(promptResult)
	return pr.response, pr.meta, nil
}

// TaskExec dispatches task execution based on the task type.
func declaredRoutes(branches []TransitionBranch) []string {
	routes := make([]string, 0, len(branches))
	for _, b := range branches {
		if b.Operator == OpEquals && strings.TrimSpace(b.When) != "" {
			routes = append(routes, b.When)
		}
	}
	return routes
}

func selectRoute(answer string, routes []string) string {
	chosen := strings.TrimSpace(answer)
	for _, r := range routes {
		if strings.EqualFold(chosen, r) {
			return r
		}
	}
	for _, r := range routes {
		if strings.Contains(strings.ToLower(chosen), strings.ToLower(r)) {
			return r
		}
	}
	return chosen
}

func (exe *SimpleExec) TaskExec(taskCtx context.Context, startingTime time.Time, ctxLength int, chainContext *ChainContext, currentTask *TaskDefinition, input any, dataType DataType) (any, DataType, string, error) {
	var transitionEval string
	var taskErr error
	var output any = input
	var outputType DataType = dataType
	if taskCtx.Err() != nil {
		return nil, DataTypeAny, "request was canceled", fmt.Errorf("task execution failed: %w", taskCtx.Err())
	}
	if currentTask.Handler == HandleNoop {
		return output, outputType, TransitionNoop, nil
	}
	if currentTask.Tools == nil {
		currentTask.Tools = &ToolsCall{}
	}
	// Unified prompt extraction function
	getPrompt := func() (string, error) {
		switch outputType {
		case DataTypeString:
			prompt, ok := input.(string)
			if !ok {
				return "", fmt.Errorf("SEVERBUG: input is not a string")
			}
			return prompt, nil
		case DataTypeInt:
			return fmt.Sprintf("%d", input), nil
		case DataTypeChatHistory:
			history, ok := input.(ChatHistory)
			if !ok {
				return "", fmt.Errorf("SEVERBUG: input is not a chat history")
			}
			if len(history.Messages) == 0 {
				return "", fmt.Errorf("SEVERBUG: chat history is empty")
			}
			return history.Messages[len(history.Messages)-1].Content, nil
		default:
			return "", fmt.Errorf("getPrompt unsupported input type for task %v: %v", currentTask.Handler.String(), outputType.String())
		}
	}
	if len(currentTask.Handler) == 0 {
		return output, dataType, transitionEval, fmt.Errorf("%w: task-type is empty", ErrUnsupportedTaskType)
	}
	switch currentTask.Handler {
	case HandleRaiseError:
		message, err := getPrompt()
		if err != nil {
			return nil, DataTypeAny, "", fmt.Errorf("failed to get prompt: %w", err)
		}
		return nil, DataTypeAny, "", errors.New(message)

	case HandleRoute:
		if currentTask.ExecuteConfig == nil {
			currentTask.ExecuteConfig = &LLMExecutionConfig{}
		}
		routes := declaredRoutes(currentTask.Transition.Branches)
		if len(routes) == 0 {
			return nil, DataTypeAny, "", fmt.Errorf("route task %s has no equals branches to route between", currentTask.ID)
		}
		sys := currentTask.SystemInstruction
		if sys != "" {
			sys += "\n\n"
		}
		sys += "Respond with exactly one of the following labels and nothing else: " + strings.Join(routes, ", ")

		// Slide chat history for route tasks when Shift is enabled, so long
		// histories can still classify using a token-safe suffix.
		if dataType == DataTypeChatHistory && currentTask.ExecuteConfig.Shift && ctxLength > 0 {
			history, ok := input.(ChatHistory)
			if !ok {
				return nil, DataTypeAny, "", fmt.Errorf("route task %s: input type mismatch for shift: %v", currentTask.ID, dataType)
			}
			if len(history.Messages) > 0 {
				modelName := GetPrimaryModel(currentTask.ExecuteConfig)
				sysTokens, countErr := exe.repo.CountTokens(taskCtx, modelName, sys)
				if countErr != nil {
					return nil, DataTypeAny, "", fmt.Errorf("route task %s: token count failed: %w", currentTask.ID, countErr)
				}
				routeOutputReserve := reserveOutputTokens(currentTask.ExecuteConfig, ctxLength)
				budget := ctxLength - sysTokens - routeOutputReserve
				if budget > 0 {
					slid, _, slideErr := exe.shiftMessagesToFit(taskCtx, modelName, history.Messages, budget)
					if slideErr != nil {
						return nil, DataTypeAny, "", fmt.Errorf("route task %s: %w", currentTask.ID, slideErr)
					}
					history.Messages = slid
					input = history
					// If the slide itself fails (e.g. even system messages overflow)
					// we fall through and let Prompt()'s own limit check surface the
					// real error rather than swallowing it here.
				}
			}
		}

		// Extract the prompt from the configured input format.
		var (
			prompt string
			err    error
		)
		switch dataType {
		case DataTypeString, DataTypeInt:
			outputType = dataType
			prompt, err = getPrompt()
		case DataTypeChatHistory:
			history, ok := input.(ChatHistory)
			if !ok {
				return nil, DataTypeAny, "", fmt.Errorf("failed to get prompt: route task %s: input is not a chat history", currentTask.ID)
			}
			prompt = routeHistoryPrompt(history)
		default:
			return nil, DataTypeAny, "", fmt.Errorf("getPrompt unsupported input type for task %v: %v", currentTask.Handler.String(), dataType.String())
		}
		if err != nil {
			return nil, DataTypeAny, "", fmt.Errorf("failed to get prompt: %w", err)
		}

		answer, err := exe.Prompt(taskCtx, sys, *currentTask.ExecuteConfig, prompt, ctxLength)
		if err != nil {
			return nil, DataTypeAny, "", fmt.Errorf("route task %s: %w", currentTask.ID, err)
		}
		return input, dataType, selectRoute(answer, routes), nil

	case HandleChatCompletion:
		if currentTask.ExecuteConfig == nil {
			currentTask.ExecuteConfig = &LLMExecutionConfig{}
		}

		var chatHistory ChatHistory
		var finalExecConfig *LLMExecutionConfig = currentTask.ExecuteConfig
		if input == nil {
			return nil, DataTypeAny, "", fmt.Errorf("input is nil for task %s", currentTask.ID)
		}

		switch dataType {
		case DataTypeChatHistory:
			var ok bool
			chatHistory, ok = input.(ChatHistory)
			if !ok {
				return nil, DataTypeAny, "", fmt.Errorf("input data for handler %s claimed to be %s but was %T", currentTask.Handler, dataType.String(), input)
			}

			// GUARD: If the last message is an unanswered tool call (e.g., from a state-machine
			// budget handoff), flush those tools inline so we don't hand the provider a dangling
			// tool_call. After flushing, fall through to the normal chat_completion path so this
			// task's system_instruction is still applied and the LLM gets a turn with the fresh
			// tool results in context.
			if len(chatHistory.Messages) > 0 {
				last := chatHistory.Messages[len(chatHistory.Messages)-1]
				if last.Role == "assistant" && len(last.CallTools) > 0 {
					savedHandler := currentTask.Handler
					currentTask.Handler = HandleExecuteToolCalls
					flushOut, _, _, flushErr := exe.TaskExec(taskCtx, startingTime, ctxLength, chainContext, currentTask, chatHistory, dataType)
					currentTask.Handler = savedHandler
					if flushErr != nil {
						return nil, DataTypeAny, "", fmt.Errorf("guard: failed to flush pending tool calls: %w", flushErr)
					}
					if h, ok := flushOut.(ChatHistory); ok {
						chatHistory = h
						chatHistory.InputTokens = 0
					}
				}
			}

		case DataTypeString:
			// Automatically coerce simple string input into a chat-compatible format
			strInput, ok := input.(string)
			if !ok {
				return nil, DataTypeAny, "", fmt.Errorf("input data for handler %s claimed to be string but was %T", currentTask.Handler, input)
			}
			chatHistory = ChatHistory{
				Messages: []Message{
					{Role: "user", Content: strInput, Timestamp: time.Now().UTC()},
				},
			}

		default:
			return nil, DataTypeAny, "", fmt.Errorf("handler '%s' requires input of type 'chat_history' or 'string', used var: %s but got '%s'", currentTask.Handler, currentTask.InputVar, dataType.String())
		}

		// Count tokens and check limits for chat completion
		modelName := GetPrimaryModel(finalExecConfig)
		if !finalExecConfig.Shift {
			if _, err := exe.countChatHistoryTokens(taskCtx, modelName, chatHistory, ctxLength); err != nil {
				return nil, DataTypeAny, "", err
			}
		}

		if currentTask.SystemInstruction != "" {
			alreadyPresent := false
			for _, msg := range chatHistory.Messages {
				if msg.Role == "system" && msg.Content == currentTask.SystemInstruction {
					alreadyPresent = true
					break
				}
			}
			if !alreadyPresent {
				messages := []Message{{ID: uuid.NewString(), Role: "system", Content: currentTask.SystemInstruction, Timestamp: time.Now().UTC()}}
				chatHistory.Messages = append(messages, chatHistory.Messages...)
				// Fix 9: force recount — the system instruction tokens are not in
				// the old InputTokens value, so executeLLM would skip counting.
				chatHistory.InputTokens = 0
			}
		}

		output, outputType, transitionEval, taskErr = exe.executeLLM(
			taskCtx,
			chatHistory,
			ctxLength,
			finalExecConfig,
			chainContext.ClientTools,
			chainContext.Tools,
			nil,
		)

	case HandleExecuteToolCalls:
		if dataType != DataTypeChatHistory {
			taskErr = fmt.Errorf("handler '%s' requires 'chat_history' input, but got '%s'",
				currentTask.Handler, dataType.String())
			break
		}

		chatHistory, ok := input.(ChatHistory)
		if !ok {
			taskErr = fmt.Errorf("input data is not of type ChatHistory")
			break
		}

		if len(chatHistory.Messages) == 0 {
			transitionEval = TransitionNoop
			break
		}

		lastMessage := chatHistory.Messages[len(chatHistory.Messages)-1]
		if len(lastMessage.CallTools) == 0 {
			transitionEval = TransitionNoCallsFound
			break
		}

		allowedTools, explicitToolsScope, err := exe.executionToolsScope(taskCtx, currentTask)
		if err != nil {
			taskErr = fmt.Errorf("failed to resolve execution tools scope: %w", err)
			break
		}
		hiddenTools := executionHiddenTools(currentTask)
		executedAny := false
		priorToolMessages := append([]Message(nil), chatHistory.Messages[:len(chatHistory.Messages)-1]...)
		batchRepeatCounts := make(map[string]int)
		// Tracks which calls in this batch received a result message. Any call
		// left unanswered (early break on error/cancellation) gets a stub result
		// after the loop — strict providers reject a transcript in which an
		// assistant tool call has no result.
		answeredBatch := make(map[string]bool)

		for _, toolCall := range lastMessage.CallTools {
			argsStr := normalizedToolArguments(toolCall.Function.Arguments)
			repeatIndex := nextToolCallRepeatIndex(priorToolMessages, batchRepeatCounts, toolCall.Function.Name, argsStr)

			// robust resolution: try direct key, then scan by Function.Name / ToolsName
			resolutionInfo, found := resolveToolWithResolution(chainContext, toolCall.Function.Name)
			if !found {
				errStr := fmt.Sprintf("tool %s not found", toolCall.Function.Name)
				exe.reportInvalidToolCall(taskCtx, toolCall, "not_found", errStr, repeatIndex)
				exe.appendToolErrorResult(taskCtx, &chatHistory, toolCall, errStr, nil)
				answeredBatch[toolCall.ID] = true
				continue
			}
			if resolutionInfo.Function.Name != "" && resolutionInfo.Function.Name != toolCall.Function.Name {
				toolCall.Function.Name = resolutionInfo.Function.Name
			}

			if isExecutionToolHidden(hiddenTools, toolCall.Function.Name, resolutionInfo) {
				errStr := fmt.Sprintf("tool %s is hidden for task %s", toolCall.Function.Name, currentTask.ID)
				exe.reportInvalidToolCall(taskCtx, toolCall, "hidden", errStr, repeatIndex)
				exe.appendToolErrorResult(taskCtx, &chatHistory, toolCall, errStr, nil)
				answeredBatch[toolCall.ID] = true
				continue
			}
			if explicitToolsScope {
				if _, ok := allowedTools[resolutionInfo.ToolsName]; !ok {
					errStr := fmt.Sprintf("tool %s from tools %q is not allowed for task %s", toolCall.Function.Name, resolutionInfo.ToolsName, currentTask.ID)
					exe.reportInvalidToolCall(taskCtx, toolCall, "not_allowed", errStr, repeatIndex, "tools_name", resolutionInfo.ToolsName)
					exe.appendToolErrorResult(taskCtx, &chatHistory, toolCall, errStr, nil)
					answeredBatch[toolCall.ID] = true
					continue
				}
			}

			var args map[string]any
			if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
				taskErr = fmt.Errorf("failed to unmarshal tool arguments for %s: %w",
					toolCall.Function.Name, err)

				errStr := taskErr.Error()
				exe.reportInvalidToolCall(taskCtx, toolCall, "invalid_arguments", errStr, repeatIndex, "tools_name", resolutionInfo.ToolsName)
				exe.appendToolErrorResult(taskCtx, &chatHistory, toolCall, errStr, args)
				answeredBatch[toolCall.ID] = true
				break
			}

			toolsArgs := make(map[string]string)
			if currentTask.Tools != nil && currentTask.Tools.Args != nil {
				toolsArgs = currentTask.Tools.Args
			}
			toolsCall := &ToolsCall{
				Name: resolutionInfo.ToolsName,
				// Strip the "toolsName." prefix: tools are advertised to the model as
				// "toolsName.toolName" for namespacing, but Exec() only needs the leaf name.
				ToolName: strings.TrimPrefix(toolCall.Function.Name, resolutionInfo.ToolsName+"."),
				// NOTE: dynamic args are passed as `input` to Exec; Tools.Args is static/template-level (may be empty for execute_tool_calls)
				Args: toolsArgs,
			}

			// Apply tools policies if defined in the current task
			callCtx := taskCtx
			if currentTask.ExecuteConfig != nil {
				if policy, ok := currentTask.ExecuteConfig.ToolsPolicies[resolutionInfo.ToolsName]; ok && len(policy) > 0 {
					callCtx = WithToolsArgs(callCtx, resolutionInfo.ToolsName, policy)
				}
			}

			// Calculate a safe byte limit for the tool output to prevent context overflow
			currentTokens := chatHistory.InputTokens + chatHistory.OutputTokens
			if currentTokens == 0 && currentTask.ExecuteConfig != nil {
				modelName := GetPrimaryModel(currentTask.ExecuteConfig)
				var err error
				currentTokens, err = exe.countChatHistoryTokens(taskCtx, modelName, chatHistory, ctxLength)
				if err != nil {
					return nil, DataTypeAny, "", fmt.Errorf("failed to count chat history tokens: %w", err)
				}
			}
			remainingTokens := max(ctxLength-currentTokens, 0)
			budgetBytes := max(int64(remainingTokens-500)*3, 0)
			callCtx = context.WithValue(callCtx, ContextKeyOutputByteLimit, budgetBytes)
			callCtx = context.WithValue(callCtx, ContextKeyToolCallID, toolCall.ID)

			toolReportErr, toolReportChange, toolEnd := exe.tracker.Start(
				callCtx, "tool_call", toolCall.Function.Name,
				"tools_name", resolutionInfo.ToolsName,
				"call_id", toolCall.ID,
				"repeat_index", repeatIndex,
			)

			// Emit a "pending" event so ACP clients can show the tool card
			// before execution starts (spec: pending → in_progress → completed).
			if exe.eventSink.Wants(TaskEventToolCallPending) {
				pendingEvent := NewTaskEvent(callCtx, TaskEventToolCallPending)
				pendingEvent.ToolName = toolCall.Function.Name
				pendingEvent.ApprovalID = toolCall.ID
				pendingEvent.ApprovalArgs = args
				publishTaskEventBestEffort(callCtx, exe.tracker, exe.eventSink, pendingEvent)
			}

			// `args` are the per-call dynamic tool arguments
			result, resultType, err := exe.toolsProvider.Exec(callCtx, startingTime, args, chainContext.Debug, toolsCall)

			toolExecErr := err
			switch {
			case err != nil && (errors.Is(err, context.Canceled) || callCtx.Err() != nil):
				toolReportErr(err)
				taskErr = err
			case err != nil:
				toolReportErr(fmt.Errorf("tool %s execution failed: %w", toolCall.Function.Name, err))
				result = fmt.Sprintf("tool %s execution failed: %s", toolCall.Function.Name, err)
				err = nil
			}

			executedAny = true

			content, marshalErr := serializeToolResultContent(result, resultType)
			if marshalErr != nil {
				taskErr = fmt.Errorf("failed to marshal tool %s result: %w", toolCall.Function.Name, marshalErr)
				toolExecErr = taskErr
				content = toolErrorContent(taskErr.Error())
			}
			content, capInfo := capToolResultContentFromContext(callCtx, toolCall.Function.Name, content)
			if taskErr == nil {
				reportToolResultContent(toolReportChange, resultType, capInfo)
				reportToolCallRepeat(toolReportChange, repeatIndex)
			}
			toolEnd()

			toolResultMessage := Message{
				ID:         uuid.NewString(),
				Role:       "tool",
				Content:    content,
				ToolCallID: toolCall.ID,
				Timestamp:  time.Now().UTC(),
			}
			chatHistory.Messages = append(chatHistory.Messages, toolResultMessage)
			answeredBatch[toolCall.ID] = true

			if exe.eventSink.Wants(TaskEventToolCall) {
				toolEvent := NewTaskEvent(callCtx, TaskEventToolCall)
				toolEvent.ToolName = toolCall.Function.Name
				toolEvent.ApprovalID = toolCall.ID
				toolEvent.ApprovalArgs = args
				toolEvent.Content = content
				if toolExecErr != nil {
					toolEvent.Error = toolExecErr.Error()
				}
				if diff, ok := toolDiffFromResult(result); ok {
					toolEvent.ToolDiffPath = diff.Path
					toolEvent.ToolDiffOldText = diff.OldText
					toolEvent.ToolDiffNewText = diff.NewText
				}
				publishTaskEventBestEffort(callCtx, exe.tracker, exe.eventSink, toolEvent)
			}

			if taskErr != nil {
				break
			}
		}

		for _, tc := range lastMessage.CallTools {
			if tc.ID == "" || answeredBatch[tc.ID] {
				continue
			}
			answeredBatch[tc.ID] = true
			exe.appendToolErrorResult(taskCtx, &chatHistory, tc, interruptedToolCallResult, nil)
		}

		output = chatHistory
		outputType = DataTypeChatHistory

		switch {
		case taskErr != nil:
			transitionEval = TransitionFailed
		case executedAny:
			transitionEval = TransitionToolsExecuted
		default:
			// We *had* tool calls, but none resolved to tools
			transitionEval = TransitionNoCallsFound
		}

	case HandleTools:
		if currentTask.Tools == nil {
			taskErr = fmt.Errorf("tools task missing tools definition")
		} else {
			if currentTask.Tools.Args == nil {
				currentTask.Tools.Args = make(map[string]string)
			}
			toolsCtx := context.WithValue(taskCtx, ContextKeyToolCallID, ensureUniqueToolCallID(currentTask.ID))
			if currentTask.ExecuteConfig != nil {
				if policy, ok := currentTask.ExecuteConfig.ToolsPolicies[currentTask.Tools.Name]; ok && len(policy) > 0 {
					toolsCtx = WithToolsArgs(toolsCtx, currentTask.Tools.Name, policy)
				}
			}

			toolReportErr, toolReportChange, toolEnd := exe.tracker.Start(
				toolsCtx, "tool_call", currentTask.Tools.ToolName,
				"tools_name", currentTask.Tools.Name,
			)

			output, outputType, transitionEval, taskErr = exe.toolsengine(
				toolsCtx,
				startingTime,
				output,
				currentTask.Tools,
				chainContext.Debug,
				currentTask.OutputTemplate,
			)

			toolExecErr := taskErr
			if taskErr != nil {
				toolReportErr(fmt.Errorf("tools task execution failed: %w", taskErr))
			} else {
				toolReportChange("result_type", outputType.String())
			}
			toolEnd()

			if exe.eventSink.Wants(TaskEventToolCall) {
				toolEvent := NewTaskEvent(toolsCtx, TaskEventToolCall)

				toolName := currentTask.Tools.Name
				if currentTask.Tools.ToolName != "" {
					toolName += "." + currentTask.Tools.ToolName
				}
				toolEvent.ToolName = toolName

				if m, ok := input.(map[string]any); ok {
					toolEvent.ApprovalArgs = m
				} else if s, ok := input.(string); ok {
					toolEvent.ApprovalArgs = map[string]any{"input": s}
				}

				content, marshalErr := serializeToolResultContent(output, outputType)
				if marshalErr != nil {
					content = fmt.Sprintf("error: failed to marshal output: %v", marshalErr)
					if toolExecErr == nil {
						toolExecErr = marshalErr
					}
				}

				toolEvent.Content = content
				if toolExecErr != nil {
					toolEvent.Error = toolExecErr.Error()
				}
				if diff, ok := toolDiffFromResult(output); ok {
					toolEvent.ToolDiffPath = diff.Path
					toolEvent.ToolDiffOldText = diff.OldText
					toolEvent.ToolDiffNewText = diff.NewText
				}

				publishTaskEventBestEffort(toolsCtx, exe.tracker, exe.eventSink, toolEvent)
			}
		}

	default:
		taskErr = fmt.Errorf("unknown task type: %w -- %s", ErrUnsupportedTaskType, currentTask.Handler.String())
	}

	return output, outputType, transitionEval, taskErr
}

const noResolvedToolsInstruction = "No tools are available in this turn. Do not claim to have inspected files, run commands, opened URLs, or used tools. Answer only from the provided conversation and context; if tool access or external inspection is needed, say so explicitly."

func llmCallRequestedTools(llmCall *LLMExecutionConfig) bool {
	return llmCall != nil && (llmCall.PassClientsTools || len(llmCall.Tools) > 0)
}

func (exe *SimpleExec) executeLLM(
	ctx context.Context,
	input ChatHistory,
	ctxLength int,
	llmCall *LLMExecutionConfig,
	clientTools []Tool,
	toolsResolution map[string]ToolWithResolution,
	prelude []Message,
) (any, DataType, string, error) {
	reportErr, reportChange, end := exe.tracker.Start(ctx, "SimpleExec", "prompt_model",
		"model_name", llmCall.Model,
		"model_names", llmCall.Models,
		"provider_types", llmCall.Providers,
		"provider_type", llmCall.Provider)
	defer end()

	// Chains legitimately route histories out of failure states (recovery and
	// summarise tasks receive an earlier task's output via input_var), so the
	// incoming history may end mid tool-call protocol. Reconcile before the
	// provider sees it.
	input.Messages = repairToolCallPairing(input.Messages)

	// Build the full list of tools
	tools := []libmodelprovider.Tool{}
	hiddenTools := map[string]struct{}{}
	for _, toolName := range llmCall.HideTools {
		hiddenTools[toolName] = struct{}{}
	}

	// 1. Client tools (if allowed)
	if llmCall.PassClientsTools {
		for _, t := range clientTools {
			if _, ok := hiddenTools[t.Function.Name]; ok {
				continue
			}
			tools = append(tools, libmodelprovider.Tool{
				Type: t.Type,
				Function: &libmodelprovider.FunctionTool{
					Name:        t.Function.Name,
					Description: t.Function.Description,
					Parameters:  t.Function.Parameters,
				},
			})
		}
	}

	// 2. Tools tools (if any tools are allowed)
	if len(llmCall.Tools) > 0 {
		resolvedNames, err := resolveToolsNames(ctx, llmCall.Tools, exe.toolsProvider)
		if err != nil {
			return nil, DataTypeAny, "", fmt.Errorf("failed to resolve tools for LLM call: %w", err)
		}
		included := make(map[string]struct{}, len(resolvedNames))
		for _, name := range resolvedNames {
			included[name] = struct{}{}
		}
		for _, twr := range toolsResolution {
			if _, ok := hiddenTools[twr.Function.Name]; ok {
				continue
			}
			if _, ok := included[twr.ToolsName]; ok {
				tools = append(tools, libmodelprovider.Tool{
					Type: twr.Type,
					Function: &libmodelprovider.FunctionTool{
						Name:        twr.Function.Name,
						Description: twr.Function.Description,
						Parameters:  twr.Function.Parameters,
					},
				})
			}
		}
	}
	if len(tools) == 0 && llmCallRequestedTools(llmCall) {
		prelude = append(prelude, Message{
			Role:    "system",
			Content: noResolvedToolsInstruction,
		})
		reportChange("no_tools_prompt_guard", map[string]any{
			"requested_registry_tools": len(llmCall.Tools) > 0,
			"requested_client_tools":   llmCall.PassClientsTools,
		})
	}

	// Token counting
	modelName := GetPrimaryModel(llmCall)

	var messagesTokens int

	// Count messages tokens if not already set
	if input.InputTokens > 0 {
		messagesTokens = input.InputTokens
	} else {
		var total int
		for _, m := range input.Messages {
			cnt, err := exe.repo.CountTokens(ctx, modelName, m.Content)
			if err != nil {
				reportErr(fmt.Errorf("token count failed: %w", err))
				return nil, DataTypeAny, "", fmt.Errorf("token count failed: %w", err)
			}
			total += cnt
		}
		messagesTokens = total
	}

	preludeTokens := 0
	for _, m := range prelude {
		cnt, err := exe.repo.CountTokens(ctx, modelName, m.Content)
		if err != nil {
			reportErr(fmt.Errorf("token count failed: %w", err))
			return nil, DataTypeAny, "", fmt.Errorf("token count failed: %w", err)
		}
		preludeTokens += cnt
	}
	messagesTokens += preludeTokens

	// Count tool tokens
	toolTokens, err := exe.countToolTokens(ctx, modelName, tools)
	if err != nil {
		reportErr(err)
		return nil, DataTypeAny, "", fmt.Errorf("failed to count tool tokens: %w", err)
	}

	totalTokens := messagesTokens + toolTokens

	// Log token usage
	reportChange("token_usage", map[string]any{
		"messages_tokens": messagesTokens,
		"tool_tokens":     toolTokens,
		"total_tokens":    totalTokens,
		"limit":           ctxLength,
	})
	// Also publish as TaskEvent for ACP etc. to pick up (used ~ total input, size = ctx)
	if exe.eventSink.Wants(TaskEventTokenUsage) {
		ev := NewTaskEvent(ctx, TaskEventTokenUsage)
		ev.ModelName = modelName
		ev.TokenUsed = totalTokens
		ev.TokenSize = ctxLength
		publishTaskEventBestEffort(ctx, nil, exe.eventSink, ev)
	}

	// Check limit and decide on sliding.
	// - If ctxLength (the effective session token budget from chain.TokenLimit or per-session override)
	//   is 0 or unset: no limit is enforced here. We pass the (potentially large) prompt to the provider.
	//   The model/provider may still reject it. Indicators should fall back to model ContextLength if known.
	// - If ctxLength > 0 but totalTokens > ctxLength:
	//     - If Shift == false (the default for LLMExecutionConfig, or explicitly set on the task):
	//       We return ErrContextLengthExceeded. No history is dropped. The turn fails.
	//       A pre-check token_usage event has already been emitted, so usage indicators can still
	//       show the state that hit the hard limit.
	//     - If Shift == true (common on chat_completion tasks in default chains):
	//       We slide (drop oldest non-system units) and continue. A post_shift event is emitted.
	if ctxLength > 0 && totalTokens > ctxLength {
		if !llmCall.Shift {
			err := fmt.Errorf("%w: total token count %d (messages: %d, tools: %d) > %d", ErrContextLengthExceeded,
				totalTokens, messagesTokens, toolTokens, ctxLength)
			reportErr(err)
			return nil, DataTypeAny, "", err
		}
		reserve := reserveOutputTokens(llmCall, ctxLength)
		budget := ctxLength - toolTokens - preludeTokens - reserve
		slid, slidTokens, err := exe.shiftMessagesToFit(ctx, modelName, input.Messages, budget)
		if err != nil {
			wrapped := fmt.Errorf("%w: irreducible context after shift (tools: %d, prelude: %d, reserve: %d, limit: %d)",
				ErrContextLengthExceeded, toolTokens, preludeTokens, reserve, ctxLength)
			reportErr(wrapped)
			return nil, DataTypeAny, "", wrapped
		}
		input.Messages = slid
		messagesTokens = slidTokens + preludeTokens
		totalTokens = messagesTokens + toolTokens
		reportChange("token_usage_post_shift", map[string]any{
			"messages_tokens": messagesTokens,
			"tool_tokens":     toolTokens,
			"total_tokens":    totalTokens,
			"limit":           ctxLength,
			"kept_messages":   len(slid),
		})
		if exe.eventSink.Wants(TaskEventTokenUsage) {
			ev := NewTaskEvent(ctx, TaskEventTokenUsage)
			ev.ModelName = modelName
			ev.TokenUsed = totalTokens
			ev.TokenSize = ctxLength
			publishTaskEventBestEffort(ctx, nil, exe.eventSink, ev)
		}
	}

	messagesC := providerMessagesFromEngine(prelude, input.Messages)

	// Prepare chat arguments
	chatArgs := chatArgsForLLMCall(llmCall, tools)
	toolSchemaBytes := 0
	if len(tools) > 0 {
		if b, err := json.Marshal(tools); err == nil {
			toolSchemaBytes = len(b)
		}
	}
	reportChange("tools_prepared", map[string]any{
		"count":        len(tools),
		"model":        llmCall.Model,
		"schema_bytes": toolSchemaBytes,
	})
	if len(prelude) > 0 {
		preludeContents := make([]string, 0, len(prelude))
		for _, m := range prelude {
			preludeContents = append(preludeContents, m.Content)
		}
		reportChange("prelude_injected", map[string]any{
			"count":    len(prelude),
			"messages": preludeContents,
		})
	}
	providerNames := []string{}
	if llmCall.Provider != "" {
		providerNames = append(providerNames, llmCall.Provider)
	}
	if llmCall.Providers != nil {
		providerNames = append(providerNames, llmCall.Providers...)
	}
	modelNames := []string{}
	if llmCall.Model != "" {
		modelNames = append(modelNames, llmCall.Model)
	}
	if llmCall.Models != nil {
		modelNames = append(modelNames, llmCall.Models...)
	}
	req := llmrepo.Request{
		ProviderTypes: providerNames,
		ModelNames:    modelNames,
		ContextLength: requestedContextRequirement(ctx, totalTokens),
		Tracker:       exe.tracker,
	}

	// ONE execution path: every chat streams and is assembled engine-side by
	// modelrepo.StreamAssembler; the event sink only observes (publishStepChunk
	// gates itself per-kind via Wants). Whether a sink listens can never select
	// a different code path — that fork is what let streamed tool calls diverge
	// from the non-streaming path. The non-streaming Chat call survives only as
	// the fallback when stream setup fails (e.g. the resolved model cannot
	// stream), and that fallback is taken identically with or without a sink.
	streamed, resp, meta, err := exe.streamChatOnce(ctx, reportChange, req, messagesC, chatArgs)
	if !streamed && err != nil {
		resp, meta, err = exe.chatWithRetry(ctx, reportChange, llmCall, req, messagesC, chatArgs)
		if err != nil {
			if len(tools) > 0 && isRecoverableToolSurfaceError(err) {
				reportChange("tools_disabled_after_tool_surface_error", map[string]any{
					"tool_count": len(tools),
					"error":      err.Error(),
				})
				noToolReq := req
				noToolReq.ContextLength = requestedContextRequirement(ctx, totalTokens-toolTokens)
				noToolMessages := stripToolProtocolMessages(messagesC)
				noToolArgs := chatArgsForLLMCall(llmCall, nil)
				resp, meta, err = exe.chatWithRetry(ctx, reportChange, llmCall, noToolReq, noToolMessages, noToolArgs)
				if err == nil {
					reportChange("tools_disabled_chat_succeeded", map[string]any{
						"model":         meta.ModelName,
						"provider_type": meta.ProviderType,
					})
				}
			}
		}
		if err == nil {
			// The fallback response was never streamed, so publish it as one
			// chunk (streamed replies were already published incrementally).
			exe.publishStepChunk(ctx, meta, resp.Message.Content, resp.Message.Thinking)
		}
	}
	if err != nil {
		return nil, DataTypeAny, "", fmt.Errorf("chat failed: %w", err)
	}

	// Process response
	callTools := make([]ToolCall, len(resp.ToolCalls))
	for i, tc := range resp.ToolCalls {
		function := FunctionCall{
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		}
		callTools[i] = ToolCall{
			ID:           ensureUniqueToolCallID(tc.ID),
			Function:     function,
			Type:         tc.Type,
			ProviderMeta: tc.ProviderMeta,
		}
	}
	respMessage := resp.Message
	input.Messages = append(input.Messages, Message{
		ID:        uuid.NewString(),
		Role:      respMessage.Role,
		Content:   respMessage.Content,
		Thinking:  respMessage.Thinking,
		CallTools: callTools,
		Timestamp: time.Now().UTC(),
	})

	// Count output tokens (only for the response content, not tool calls)
	var outputTokensCount int
	if message := resp.Message; len(message.Content) != 0 {
		outputTokensCount, err = exe.repo.CountTokens(ctx, meta.ModelName, message.Content)
		if err != nil {
			err = fmt.Errorf("tokenizer failed: %w", err)
			reportErr(err)
			return nil, DataTypeAny, "", err
		}
	}
	input.OutputTokens = outputTokensCount

	if len(callTools) > 0 {
		return input, DataTypeChatHistory, TransitionToolCall, nil
	}
	return input, DataTypeChatHistory, TransitionExecuted, nil
}

// streamChatOnce runs one chat over the streaming path and assembles the
// result with modelrepo.StreamAssembler — the single place streamed deltas
// become tool calls. streamed=false with an error means stream SETUP failed
// (resolution/connect) and the caller may fall back to non-streaming Chat;
// streamed=true means parcels were (possibly) already observed, so mid-stream
// failures are final — re-running the model after publishing chunks would
// show the user two divergent replies.
func (exe *SimpleExec) streamChatOnce(
	ctx context.Context,
	reportChange func(id string, data any),
	req llmrepo.Request,
	messages []libmodelprovider.Message,
	chatArgs []libmodelprovider.ChatArgument,
) (streamed bool, _ libmodelprovider.ChatResult, _ llmrepo.Meta, _ error) {
	stream, meta, err := exe.repo.Stream(ctx, req, messages, chatArgs...)
	if err != nil {
		return false, libmodelprovider.ChatResult{}, llmrepo.Meta{}, err
	}

	asm := libmodelprovider.NewStreamAssembler(meta.ProviderType, meta.ModelName)
	for parcel := range stream {
		if err := asm.Consume(parcel); err != nil {
			return true, libmodelprovider.ChatResult{}, meta, fmt.Errorf("chat stream failed: %w", err)
		}
		// Observation only: visible content/thinking deltas fan out to the
		// sink; tool-call fragments, usage, and terminal parcels never leak
		// into the transcript.
		exe.publishStepChunk(ctx, meta, parcel.Data, parcel.Thinking)
	}
	res, err := asm.Result()
	if err != nil {
		return true, libmodelprovider.ChatResult{}, meta, fmt.Errorf("chat stream failed: %w", err)
	}

	// The terminal parcel's finish reason and usage are part of the response
	// truth: length/content_filter endings and provider token accounting are
	// recorded instead of ignored.
	finish := map[string]any{
		"finish_reason": res.FinishReason,
		"model":         meta.ModelName,
		"provider_type": meta.ProviderType,
	}
	if res.Usage != nil {
		finish["prompt_tokens"] = res.Usage.PromptTokens
		finish["completion_tokens"] = res.Usage.CompletionTokens
		finish["total_tokens"] = res.Usage.TotalTokens
	}
	reportChange("stream_finished", finish)

	out := libmodelprovider.ChatResult{
		Message: libmodelprovider.Message{
			Role:     "assistant",
			Content:  res.Content,
			Thinking: res.Thinking,
		},
		ToolCalls: res.ToolCalls,
	}
	return true, out, meta, nil
}

func chatArgsForLLMCall(llmCall *LLMExecutionConfig, tools []libmodelprovider.Tool) []libmodelprovider.ChatArgument {
	chatArgs := make([]libmodelprovider.ChatArgument, 0, 5)
	if len(tools) > 0 {
		chatArgs = append(chatArgs, libmodelprovider.WithTools(tools...))
	}
	if v, ok := temperatureValue(llmCall.Temperature); ok {
		chatArgs = append(chatArgs, libmodelprovider.WithTemperature(float64(v)))
	}
	if llmCall.Think != "" {
		chatArgs = append(chatArgs, libmodelprovider.WithThink(llmCall.Think))
	}
	// Only forward an explicit max_tokens. Falling back to ctxLength conflates
	// the input+output context window with the per-response output cap and trips
	// per-model output limits (e.g. Vertex Gemini 2.5 Pro caps maxOutputTokens
	// at 65536, well below typical 131072 ctxLength settings).
	if llmCall.MaxTokens != nil && *llmCall.MaxTokens > 0 {
		chatArgs = append(chatArgs, libmodelprovider.WithMaxTokens(*llmCall.MaxTokens))
	}
	if llmCall.Shift {
		chatArgs = append(chatArgs, libmodelprovider.WithShift{})
	}
	return chatArgs
}

func isRecoverableToolSurfaceError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "unsupported feature") && strings.Contains(s, "tool call") {
		return true
	}
	if strings.Contains(s, "json schema conversion failed") || strings.Contains(s, "unrecognized schema") {
		return true
	}
	if strings.Contains(s, "chat template") && strings.Contains(s, "tool") {
		return true
	}
	return strings.Contains(s, "context overflow") || strings.Contains(s, "exceeded the session context window")
}

// providerMessagesFromEngine converts engine messages (prelude followed by the
// conversation) into provider messages, carrying text, tool calls, and image
// attachments. Prelude messages are engine-injected instructions and never
// carry tool calls or images.
func providerMessagesFromEngine(prelude, messages []Message) []libmodelprovider.Message {
	out := make([]libmodelprovider.Message, 0, len(prelude)+len(messages))
	for _, m := range prelude {
		out = append(out, libmodelprovider.Message{
			Role:    m.Role,
			Content: m.Content,
		})
	}
	for _, m := range messages {
		var toolCalls []libmodelprovider.ToolCall
		if len(m.CallTools) > 0 {
			toolCalls = make([]libmodelprovider.ToolCall, len(m.CallTools))
			for i, tc := range m.CallTools {
				toolCalls[i].ID = tc.ID
				toolCalls[i].Type = tc.Type
				toolCalls[i].Function.Name = tc.Function.Name
				toolCalls[i].Function.Arguments = tc.Function.Arguments
				toolCalls[i].ProviderMeta = tc.ProviderMeta
			}
		}
		var images []libmodelprovider.ImagePart
		if len(m.Images) > 0 {
			images = make([]libmodelprovider.ImagePart, len(m.Images))
			for i, img := range m.Images {
				images[i] = libmodelprovider.ImagePart{Data: img.Data, MimeType: img.MimeType}
			}
		}
		out = append(out, libmodelprovider.Message{
			Role:       m.Role,
			Content:    m.Content,
			Images:     images,
			ToolCalls:  toolCalls,
			ToolCallID: m.ToolCallID,
		})
	}
	return out
}

func stripToolProtocolMessages(messages []libmodelprovider.Message) []libmodelprovider.Message {
	out := make([]libmodelprovider.Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == "tool" {
			continue
		}
		hadToolCalls := len(msg.ToolCalls) > 0
		msg.ToolCalls = nil
		msg.ToolCallID = ""
		if msg.Role == "assistant" && hadToolCalls && strings.TrimSpace(msg.Content) == "" {
			continue
		}
		out = append(out, msg)
	}
	return out
}

// toolsengine handles the execution of a tools, including output templating.
func (exe *SimpleExec) toolsengine(
	ctx context.Context,
	startingTime time.Time,
	input any,
	tools *ToolsCall,
	debug bool,
	outputTemplate string,
) (any, DataType, string, error) {
	if tools.Args == nil {
		tools.Args = make(map[string]string)
	}

	// Call the provider with the new, simple signature.
	toolsOutput, dataType, err := exe.toolsProvider.Exec(ctx, startingTime, input, debug, tools)
	if err != nil {
		return nil, dataType, TransitionFailed, err
	}

	toolsOutput, dataType, normErr := NormalizeDataType(toolsOutput, dataType)
	if normErr != nil {
		return nil, DataTypeAny, TransitionFailed, normErr
	}

	// On success, process the output. Default eval matches execute_tool_calls'
	// success token; an OutputTemplate (below) overrides it with its rendered text.
	finalOutput := toolsOutput
	finalOutputType := dataType
	finalTransitionEval := TransitionToolsExecuted

	// Apply the output template if it's defined.
	if outputTemplate != "" {
		rendered, tplErr := renderTemplate(outputTemplate, finalOutput)
		if tplErr != nil {
			// Return a descriptive error if templating fails.
			return nil, DataTypeAny, TransitionFailed, fmt.Errorf("failed to render tools output template: %w", tplErr)
		}
		finalOutput = rendered
		finalOutputType = DataTypeString
		finalTransitionEval = rendered
	}

	// Return the processed results.
	return finalOutput, finalOutputType, finalTransitionEval, nil
}

// resolveToolWithResolution tries to find a ToolWithResolution for a given tool name.
// It first looks up by key, then falls back to scanning by Function.Name / ToolsName.
// This makes us robust to how chainContext.Tools was keyed.
func resolveToolWithResolution(chainContext *ChainContext, toolName string) (ToolWithResolution, bool) {
	if chainContext == nil {
		return ToolWithResolution{}, false
	}

	// 1) Direct lookup by key
	if twr, ok := chainContext.Tools[toolName]; ok {
		return twr, true
	}

	// 2) Fallback: scan for matching Function.Name or ToolsName
	for _, twr := range chainContext.Tools {
		if twr.Function.Name == toolName || twr.ToolsName == toolName {
			return twr, true
		}
	}

	if !strings.Contains(toolName, ".") {
		var match ToolWithResolution
		matches := 0
		for _, twr := range chainContext.Tools {
			leaf := strings.TrimPrefix(twr.Function.Name, twr.ToolsName+".")
			if leaf == toolName {
				match = twr
				matches++
			}
		}
		if matches == 1 {
			return match, true
		}
	}

	return ToolWithResolution{}, false
}

func (exe *SimpleExec) executionToolsScope(ctx context.Context, currentTask *TaskDefinition) (map[string]struct{}, bool, error) {
	if currentTask == nil || currentTask.ExecuteConfig == nil || currentTask.ExecuteConfig.Tools == nil {
		return nil, false, nil
	}
	names, err := resolveToolsNames(ctx, currentTask.ExecuteConfig.Tools, exe.toolsProvider)
	if err != nil {
		return nil, true, err
	}
	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		allowed[name] = struct{}{}
	}
	return allowed, true, nil
}

func executionHiddenTools(currentTask *TaskDefinition) map[string]struct{} {
	if currentTask == nil || currentTask.ExecuteConfig == nil || len(currentTask.ExecuteConfig.HideTools) == 0 {
		return nil
	}
	hidden := make(map[string]struct{}, len(currentTask.ExecuteConfig.HideTools))
	for _, name := range currentTask.ExecuteConfig.HideTools {
		name = strings.TrimSpace(name)
		if name != "" {
			hidden[name] = struct{}{}
		}
	}
	return hidden
}

func isExecutionToolHidden(hidden map[string]struct{}, toolName string, resolution ToolWithResolution) bool {
	if len(hidden) == 0 {
		return false
	}
	if _, ok := hidden[toolName]; ok {
		return true
	}
	if _, ok := hidden[resolution.Function.Name]; ok {
		return true
	}
	leaf := strings.TrimPrefix(toolName, resolution.ToolsName+".")
	if _, ok := hidden[leaf]; ok {
		return true
	}
	namespaced := resolution.ToolsName + "." + leaf
	if _, ok := hidden[namespaced]; ok {
		return true
	}
	return false
}

func toolErrorContent(errStr string) string {
	payload, err := json.Marshal(map[string]string{"error": errStr})
	if err != nil {
		return fmt.Sprintf(`{"error": "%s"}`, errStr)
	}
	return string(payload)
}

type toolResultCapInfo struct {
	OriginalBytes int
	FinalBytes    int
	MaxBytes      int64
	CapActive     bool
	Truncated     bool
}

type toolDiff struct {
	Path    string
	OldText string
	NewText string
}

type toolDiffProvider interface {
	ToolDiff() (path string, oldText string, newText string, ok bool)
}

func serializeToolResultContent(result any, resultType DataType) (string, error) {
	switch resultType {
	case DataTypeNil:
		return "null", nil
	case DataTypeAny, DataTypeJSON:
		b, err := json.Marshal(result)
		if err != nil {
			return "", err
		}
		return string(b), nil
	default:
		return fmt.Sprintf("%v", result), nil
	}
}

func toolDiffFromResult(result any) (toolDiff, bool) {
	provider, ok := result.(toolDiffProvider)
	if !ok {
		return toolDiff{}, false
	}
	path, oldText, newText, ok := provider.ToolDiff()
	if !ok || path == "" || oldText == newText {
		return toolDiff{}, false
	}
	return toolDiff{Path: path, OldText: oldText, NewText: newText}, true
}

func normalizedToolArguments(arguments string) string {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return "{}"
	}
	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return trimmed
	}
	normalized, err := json.Marshal(decoded)
	if err != nil {
		return trimmed
	}
	return string(normalized)
}

func nextToolCallRepeatIndex(priorMessages []Message, batchCounts map[string]int, toolName string, arguments string) int {
	key := toolCallRepeatKey(toolName, arguments)
	prior := 0
	for _, msg := range priorMessages {
		for _, call := range msg.CallTools {
			if toolCallRepeatKey(call.Function.Name, normalizedToolArguments(call.Function.Arguments)) == key {
				prior++
			}
		}
	}
	batchCounts[key]++
	return prior + batchCounts[key]
}

func toolCallRepeatKey(toolName string, arguments string) string {
	return toolName + "\x00" + arguments
}

func capToolResultContentFromContext(ctx context.Context, toolName string, content string) (string, toolResultCapInfo) {
	maxBytes, ok := ctx.Value(ContextKeyOutputByteLimit).(int64)
	if !ok {
		return content, toolResultCapInfo{
			OriginalBytes: len(content),
			FinalBytes:    len(content),
		}
	}
	return capToolResultContent(toolName, content, maxBytes)
}

func capToolResultContent(toolName string, content string, maxBytes int64) (string, toolResultCapInfo) {
	info := toolResultCapInfo{
		OriginalBytes: len(content),
		FinalBytes:    len(content),
		MaxBytes:      maxBytes,
		CapActive:     true,
	}
	if maxBytes < 0 || int64(len(content)) <= maxBytes {
		return content, info
	}

	sum := sha256.Sum256([]byte(content))
	payload := map[string]any{
		"error":          "tool_result_too_large",
		"tool":           toolName,
		"original_bytes": info.OriginalBytes,
		"max_bytes":      maxBytes,
		"truncated":      true,
		"sha256":         fmt.Sprintf("%x", sum),
	}
	if preview := toolResultPreview(content, maxBytes); preview != "" {
		payload["preview"] = preview
		payload["preview_kind"] = "head"
	}

	b, err := json.Marshal(payload)
	if err != nil {
		fallback := fmt.Sprintf(`{"error":"tool_result_too_large","tool":%q,"original_bytes":%d,"max_bytes":%d,"truncated":true}`,
			toolName, info.OriginalBytes, maxBytes)
		info.FinalBytes = len(fallback)
		info.Truncated = true
		return fallback, info
	}

	capped := string(b)
	info.FinalBytes = len(capped)
	info.Truncated = true
	return capped, info
}

func toolResultPreview(content string, maxBytes int64) string {
	if maxBytes <= 512 {
		return ""
	}
	previewBytes := int(min(int64(4096), maxBytes/2))
	if previewBytes <= 0 {
		return ""
	}
	if len(content) <= previewBytes {
		return content
	}
	return content[:previewBytes]
}

func reportToolResultContent(reportChange func(string, any), resultType DataType, info toolResultCapInfo) {
	if reportChange == nil {
		return
	}
	reportChange("result_type", resultType.String())
	reportChange("result_bytes", info.FinalBytes)
	reportChange("result_original_bytes", info.OriginalBytes)
	reportChange("result_truncated", info.Truncated)
	if info.CapActive {
		reportChange("result_max_bytes", info.MaxBytes)
	}
}

func reportToolCallRepeat(reportChange func(string, any), repeatIndex int) {
	if reportChange == nil {
		return
	}
	reportChange("repeat_index", repeatIndex)
	if repeatIndex > 1 {
		reportChange("repeated_call", true)
	}
}

func (exe *SimpleExec) reportInvalidToolCall(ctx context.Context, toolCall ToolCall, class string, errStr string, repeatIndex int, kvArgs ...any) {
	callCtx := context.WithValue(ctx, ContextKeyToolCallID, toolCall.ID)
	startArgs := []any{
		"call_id", toolCall.ID,
		"invalid_call", true,
		"invalid_call_class", class,
		"repeat_index", repeatIndex,
	}
	startArgs = append(startArgs, kvArgs...)
	reportErr, reportChange, end := exe.tracker.Start(callCtx, "tool_call", toolCall.Function.Name, startArgs...)
	reportChange("invalid_call", true)
	reportChange("invalid_call_class", class)
	reportToolCallRepeat(reportChange, repeatIndex)
	content := toolErrorContent(errStr)
	reportToolResultContent(reportChange, DataTypeString, toolResultCapInfo{
		OriginalBytes: len(content),
		FinalBytes:    len(content),
	})
	reportErr(errors.New(errStr))
	end()
}

func (exe *SimpleExec) appendToolErrorResult(ctx context.Context, chatHistory *ChatHistory, toolCall ToolCall, errStr string, args map[string]any) {
	if chatHistory == nil {
		return
	}
	toolResultMessage := Message{
		ID:         uuid.NewString(),
		Role:       "tool",
		Content:    toolErrorContent(errStr),
		ToolCallID: toolCall.ID,
		Timestamp:  time.Now().UTC(),
	}
	chatHistory.Messages = append(chatHistory.Messages, toolResultMessage)

	if exe.eventSink.Wants(TaskEventToolCall) {
		toolEvent := NewTaskEvent(ctx, TaskEventToolCall)
		toolEvent.ToolName = toolCall.Function.Name
		toolEvent.ApprovalID = toolCall.ID
		toolEvent.ApprovalArgs = args
		toolEvent.Error = errStr
		publishTaskEventBestEffort(ctx, exe.tracker, exe.eventSink, toolEvent)
	}
}

// chatWithRetry wraps repo.Chat with [llmretry.Do] when llmCall.RetryPolicy is
// set; otherwise it issues a single call (preserving today's behavior). On
// fallback, the request's ModelNames slice is replaced with the fallback id so
// the underlying resolver targets that model directly.
//
// Every invocation appends an [llmretry.Outcome] to the context-bound sink
// (see [WithRetryOutcomeSink]) so callers can inspect what happened.
func (exe *SimpleExec) chatWithRetry(
	ctx context.Context,
	reportChange func(id string, data any),
	llmCall *LLMExecutionConfig,
	req llmrepo.Request,
	messages []libmodelprovider.Message,
	chatArgs []libmodelprovider.ChatArgument,
) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
	policy := llmretry.RetryPolicy{}
	if llmCall != nil && llmCall.RetryPolicy != nil {
		policy = *llmCall.RetryPolicy
	}
	primary := GetPrimaryModel(llmCall)
	type chatResult struct {
		resp libmodelprovider.ChatResult
		meta llmrepo.Meta
	}
	attempt := 0
	var prevErr error
	result, outcome, err := llmretry.Do(ctx, policy, primary, func(modelID string) (any, error) {
		attempt++
		if attempt > 1 && reportChange != nil {
			data := map[string]any{
				"attempt":          attempt,
				"model":            modelID,
				"prev_error_class": string(llmretry.ClassifyError(prevErr)),
				"prev_error":       prevErr.Error(),
			}
			reportChange("retry_attempt", data)
		}
		callReq := req
		if modelID != "" && modelID != primary {
			callReq.ModelNames = []string{modelID}
		}
		r, m, e := exe.repo.Chat(ctx, callReq, messages, chatArgs...)
		prevErr = e
		if e != nil {
			return nil, e
		}
		return chatResult{resp: r, meta: m}, nil
	})
	appendRetryOutcome(ctx, outcome)
	if reportChange != nil {
		reportChange("retry_outcome", map[string]any{
			"attempts":         outcome.Attempts,
			"used_fallback":    outcome.UsedFallback,
			"last_error_class": string(outcome.LastErrorClass),
			"elapsed":          outcome.Elapsed.String(),
		})
	}
	if err != nil {
		return libmodelprovider.ChatResult{}, llmrepo.Meta{}, err
	}
	cr := result.(chatResult)
	return cr.resp, cr.meta, nil
}

// countToolTokens serializes the tools to JSON and counts tokens using the model's tokenizer.
func (exe *SimpleExec) countToolTokens(ctx context.Context, modelName string, tools []libmodelprovider.Tool) (int, error) {
	if len(tools) == 0 {
		return 0, nil
	}
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal tools: %w", err)
	}
	tokenCount, err := exe.repo.CountTokens(ctx, modelName, string(toolsJSON))
	if err != nil {
		return 0, fmt.Errorf("failed to count tool tokens: %w", err)
	}
	return tokenCount, nil
}
