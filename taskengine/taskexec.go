package taskengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dario.cat/mergo"
	"github.com/contenox/vibe/internal/llmrepo"
	libmodelprovider "github.com/contenox/vibe/internal/modelrepo"
	"github.com/contenox/vibe/libtracker"
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

// SimpleExec is a basic implementation of TaskExecutor.
// It supports prompt-to-string, number, score, range, boolean condition evaluation,
// and delegation to registered hooks.
type SimpleExec struct {
	repo         llmrepo.ModelRepo
	hookProvider HookRepo
	tracker      libtracker.ActivityTracker
}

// NewExec creates a new SimpleExec instance
func NewExec(
	_ context.Context,
	repo llmrepo.ModelRepo,
	hookProvider HookRepo,
	tracker libtracker.ActivityTracker,
) (TaskExecutor, error) {
	if hookProvider == nil {
		return nil, fmt.Errorf("hook provider is nil")
	}
	if repo == nil {
		return nil, fmt.Errorf("repo executor is nil")
	}
	return &SimpleExec{
		hookProvider: hookProvider,
		repo:         repo,
		tracker:      tracker,
	}, nil
}

// countTokensAndCheckLimit counts tokens for text and checks against context limit
func (exe *SimpleExec) countTokensAndCheckLimit(ctx context.Context, modelName string, text string, ctxLength int) error {
	if ctxLength <= 0 {
		return nil // No limit enforced
	}

	tokenCount, err := exe.repo.CountTokens(ctx, modelName, text)
	if err != nil {
		return fmt.Errorf("token count failed: %w", err)
	}

	if tokenCount > ctxLength {
		return fmt.Errorf("input token count %d exceeds context length %d", tokenCount, ctxLength)
	}

	return nil
}

// countChatHistoryTokens counts total tokens in chat history
func (exe *SimpleExec) countChatHistoryTokens(ctx context.Context, modelName string, history ChatHistory, ctxLength int) (int, error) {
	if ctxLength <= 0 {
		return 0, nil // No limit to enforce
	}

	// If tokens are already calculated and valid, use them
	if history.InputTokens > 0 && history.OutputTokens > 0 {
		totalTokens := history.InputTokens + history.OutputTokens
		if totalTokens > ctxLength {
			return totalTokens, fmt.Errorf("chat history token count %d exceeds context length %d", totalTokens, ctxLength)
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
		return totalTokens, fmt.Errorf("chat history token count %d exceeds context length %d", totalTokens, ctxLength)
	}

	return totalTokens, nil
}

// getPrimaryModel extracts the primary model name from execution config
func getPrimaryModel(llmCall *LLMExecutionConfig) string {
	if llmCall.Model != "" {
		return llmCall.Model
	}
	if len(llmCall.Models) > 0 {
		return llmCall.Models[0]
	}
	return "default" // Fallback model name for token counting
}

// Prompt resolves a model client using the resolver policy and sends the prompt
// to be executed. Returns the trimmed response string or an error.
func (exe *SimpleExec) Prompt(ctx context.Context, systemInstruction string, llmCall LLMExecutionConfig, prompt string, ctxLength int) (string, error) {
	reportErr, _, end := exe.tracker.Start(ctx, "SimpleExec", "prompt_model",
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
	modelName := getPrimaryModel(&llmCall)
	combinedText := systemInstruction + "\n" + prompt
	if err := exe.countTokensAndCheckLimit(ctx, modelName, combinedText, ctxLength); err != nil {
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
	response, _, err := exe.repo.PromptExecute(ctx, llmrepo.Request{
		ProviderTypes: providerNames,
		ModelNames:    modelNames,
		Tracker:       exe.tracker,
	}, systemInstruction, float32(llmCall.Temperature), prompt)
	if err != nil {
		err = fmt.Errorf("prompt execution failed: %w", err)
		reportErr(err)
		return "", err
	}

	return strings.TrimSpace(response), nil
}

// Prompt resolves a model client and sends the prompt
// to be executed. Returns the trimmed response string or an error.
func (exe *SimpleExec) Embed(ctx context.Context, llmCall LLMExecutionConfig, prompt string, ctxLength int) ([]float64, error) {
	reportErr, _, end := exe.tracker.Start(ctx, "SimpleExec", "Embed",
		"model_name", llmCall.Model,
		"model_names", llmCall.Models,
		"provider_types", llmCall.Providers,
		"provider_type", llmCall.Provider,
	)
	defer end()

	if prompt == "" {
		err := fmt.Errorf("unprocessable empty prompt")
		reportErr(err)
		return nil, err
	}

	// Count tokens and check limits
	modelName := getPrimaryModel(&llmCall)
	if err := exe.countTokensAndCheckLimit(ctx, modelName, prompt, ctxLength); err != nil {
		reportErr(err)
		return nil, err
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
	if len(providerNames) > 1 {
		return nil, fmt.Errorf("multiple providers specified")
	}
	if len(modelNames) > 1 {
		return nil, fmt.Errorf("multiple models specified")
	}
	privider := ""
	modelNameOut := ""
	if len(modelNames) > 0 {
		modelNameOut = modelNames[0]
	}
	if len(providerNames) > 0 {
		privider = providerNames[0]
	}

	response, _, err := exe.repo.Embed(ctx, llmrepo.EmbedRequest{
		ProviderType: privider,
		ModelName:    modelNameOut,
		// Tracker:      exe.tracker,
	}, prompt)
	if err != nil {
		err = fmt.Errorf("prompt execution failed: %w", err)
		reportErr(err)
		return nil, err
	}

	return response, nil
}

// rang executes the prompt and attempts to parse the response as a range string (e.g. "6-8").
// If the response is a single number, it returns a degenerate range like "6-6".
func (exe *SimpleExec) rang(ctx context.Context, systemInstruction string, llmCall LLMExecutionConfig, prompt string, ctxLength int) (string, error) {
	response, err := exe.Prompt(ctx, systemInstruction, llmCall, prompt, ctxLength)
	if err != nil {
		return "", fmt.Errorf("rang: prompt execution failed: %w", err)
	}

	return parseRangeString(response, prompt, response)
}

// parseRangeString parses and validates a string as either a range ("6-8") or a single number.
// Returns the normalized range string (e.g., "6-8" or "6-6") or an error.
func parseRangeString(input, prompt, response string) (string, error) {
	clean := strings.TrimSpace(input)
	clean = strings.ReplaceAll(clean, " ", "")
	clean = strings.ReplaceAll(clean, "\"", "")
	clean = strings.ReplaceAll(clean, "'", "")

	if strings.Contains(clean, "-") {
		parts := strings.Split(clean, "-")
		if len(parts) != 2 {
			return "", fmt.Errorf("invalid range format: %q", input)
		}
		_, err1 := parseNumber(parts[0])
		_, err2 := parseNumber(parts[1])
		if err1 != nil {
			return "", fmt.Errorf("invalid number format: prompt %s answer %s invalid part %q %w", prompt, response, parts[0], err1)
		}
		if err2 != nil {
			return "", fmt.Errorf("invalid number format: prompt %s answer %s invalid part %q %w", prompt, response, parts[1], err2)
		}
		return parts[0] + "-" + parts[1], nil // return normalized (already clean)
	}

	// Try as single number
	if _, err := parseNumber(clean); err != nil {
		return "", fmt.Errorf("invalid number format: prompt %s answer %s %w", prompt, response, err)
	}

	return clean + "-" + clean, nil
}

// number executes the prompt and parses the response as an integer.
func (exe *SimpleExec) number(ctx context.Context, systemInstruction string, llmCall LLMExecutionConfig, prompt string, ctxLength int) (int, error) {
	response, err := exe.Prompt(ctx, systemInstruction, llmCall, prompt, ctxLength)
	if err != nil {
		return 0, fmt.Errorf("number: prompt execution failed: %w", err)
	}

	num, err := parseNumber(response)
	if err != nil {
		return 0, fmt.Errorf("invalid number format: prompt %s answer %s %w", prompt, response, err)
	}

	// Check if the float is actually a whole number
	if num != float64(int(num)) {
		return 0, fmt.Errorf("parsed number is not an integer: %g", num)
	}

	return int(num), nil
}

// score executes the prompt and parses the response as a floating-point score.
func (exe *SimpleExec) score(ctx context.Context, systemInstruction string, llmCall LLMExecutionConfig, prompt string, ctxLength int) (float64, error) {
	response, err := exe.Prompt(ctx, systemInstruction, llmCall, prompt, ctxLength)
	if err != nil {
		return 0, fmt.Errorf("score: prompt execution failed: %w", err)
	}
	f, err := parseNumber(response)
	if err != nil {
		return 0, fmt.Errorf("invalid number format: prompt %s answer %s %w", prompt, response, err)
	}
	return f, nil
}

// TaskExec dispatches task execution based on the task type.
func (exe *SimpleExec) TaskExec(taskCtx context.Context, startingTime time.Time, ctxLength int, chainContext *ChainContext, currentTask *TaskDefinition, input any, dataType DataType) (any, DataType, string, error) {
	var transitionEval string
	var taskErr error
	var output any = input
	var outputType DataType = dataType
	if taskCtx.Err() != nil {
		return nil, DataTypeAny, "request was canceled", fmt.Errorf("task execution failed: %w", taskCtx.Err())
	}
	if currentTask.Handler == HandleNoop {
		return output, outputType, "noop", nil
	}
	if currentTask.Hook == nil {
		currentTask.Hook = &HookCall{}
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
		case DataTypeFloat:
			return fmt.Sprintf("%f", input), nil
		case DataTypeBool:
			return fmt.Sprintf("%t", input), nil
		case DataTypeChatHistory:
			history, ok := input.(ChatHistory)
			if !ok {
				return "", fmt.Errorf("SEVERBUG: input is not a chat history")
			}
			if len(history.Messages) == 0 {
				return "", fmt.Errorf("SEVERBUG: chat history is empty")
			}
			return history.Messages[len(history.Messages)-1].Content, nil
		case DataTypeOpenAIChat:
			request, ok := input.(OpenAIChatRequest)
			if !ok {
				return "", fmt.Errorf("internal error: input is not an OpenAIChatRequest")
			}
			if len(request.Messages) == 0 {
				return "", fmt.Errorf("cannot get prompt from empty OpenAI chat request")
			}
			return request.Messages[len(request.Messages)-1].Content, nil

		default:
			return "", fmt.Errorf("getPrompt unsupported input type for task %v: %v", currentTask.Handler.String(), outputType.String())
		}
	}
	if len(currentTask.Handler) == 0 {
		return output, dataType, transitionEval, fmt.Errorf("%w: task-type is empty", ErrUnsupportedTaskType)
	}
	switch currentTask.Handler {
	case HandlePromptToString,
		HandlePromptToCondition,
		HandlePromptToInt,
		HandlePromptToFloat,
		HandlePromptToRange,
		HandleParseTransition,
		HandleTextToEmbedding,
		HandleRaiseError,
		HandleParseKeyValue,
		HandlePromptToJS:
		prompt, err := getPrompt()
		if err != nil {
			return nil, DataTypeAny, "", err
		}

		if currentTask.ExecuteConfig == nil {
			currentTask.ExecuteConfig = &LLMExecutionConfig{}
		}

		switch currentTask.Handler {
		case HandlePromptToString:
			transitionEval, taskErr = exe.Prompt(taskCtx, currentTask.SystemInstruction, *currentTask.ExecuteConfig, prompt, ctxLength)
			output = transitionEval
			outputType = DataTypeString

		case HandlePromptToCondition:
			var hit bool
			hit, taskErr = exe.condition(taskCtx, currentTask.SystemInstruction, *currentTask.ExecuteConfig, currentTask.ValidConditions, prompt, ctxLength)
			output = hit
			outputType = DataTypeBool
			transitionEval = strconv.FormatBool(hit)

		case HandlePromptToInt:
			var number int
			number, taskErr = exe.number(taskCtx, currentTask.SystemInstruction, *currentTask.ExecuteConfig, prompt, ctxLength)
			output = number
			outputType = DataTypeInt
			transitionEval = strconv.FormatInt(int64(number), 10)

		case HandlePromptToFloat:
			var score float64
			score, taskErr = exe.score(taskCtx, currentTask.SystemInstruction, *currentTask.ExecuteConfig, prompt, ctxLength)
			output = score
			outputType = DataTypeFloat
			transitionEval = strconv.FormatFloat(score, 'f', 2, 64)

		case HandlePromptToRange:
			transitionEval, taskErr = exe.rang(taskCtx, currentTask.SystemInstruction, *currentTask.ExecuteConfig, prompt, ctxLength)
			outputType = DataTypeString
			output = transitionEval

		case HandleParseTransition:
			transitionEval, taskErr = exe.parseTransition(prompt)
			// output / outputType pass-through

		case HandleTextToEmbedding:
			message, err := getPrompt()
			if err != nil {
				return nil, DataTypeAny, "", fmt.Errorf("failed to get prompt: %w", err)
			}
			output, taskErr = exe.Embed(taskCtx, *currentTask.ExecuteConfig, message, ctxLength)
			outputType = DataTypeVector
			transitionEval = "ok"

		case HandleRaiseError:
			message, err := getPrompt()
			if err != nil {
				return nil, DataTypeAny, "", fmt.Errorf("failed to get prompt: %w", err)
			}
			return nil, DataTypeAny, "", errors.New(message)

		case HandleParseKeyValue:
			var message string
			switch outputType {
			case DataTypeJSON:
				// If already JSON, just pass through
				output = input
				outputType = DataTypeJSON
				transitionEval = "already_json"
			default:
				message, err = getPrompt()
				if err != nil {
					return nil, DataTypeAny, "", fmt.Errorf("failed to get prompt: %w", err)
				}
				// Parse key-value pairs
				result, err := parseKeyValueString(message)
				if err != nil {
					return nil, DataTypeAny, "", fmt.Errorf("failed to parse key-value string: %w", err)
				}

				output = result
				outputType = DataTypeJSON
				transitionEval = "parsed"
			}

		case HandlePromptToJS:
			// 1) Ask the model for JS source (or JSON containing JS).
			rawResponse, err := exe.Prompt(taskCtx, currentTask.SystemInstruction, *currentTask.ExecuteConfig, prompt, ctxLength)
			if err != nil {
				taskErr = fmt.Errorf("prompt_to_js: prompt execution failed: %w", err)
				break
			}

			// 2) Normalize: strip code fences and, if it's JSON, extract the `code` field.
			jsCode := normalizeJSResponse(rawResponse)

			output = map[string]any{
				"code": jsCode,
			}
			outputType = DataTypeJSON

			// 3) Simple transition key for branching.
			if strings.TrimSpace(jsCode) == "" {
				transitionEval = "empty_js"
			} else {
				transitionEval = "ok"
			}

		}

	case HandleConvertToOpenAIChatResponse:
		if dataType != DataTypeChatHistory {
			return nil, DataTypeAny, "", fmt.Errorf("handler '%s' requires input of type 'chat_history', used var %s, but got '%s'", currentTask.InputVar, currentTask.Handler, dataType.String())
		}
		chatHistory, ok := input.(ChatHistory)
		if !ok {
			return nil, DataTypeAny, "", fmt.Errorf("input data is not of type ChatHistory")
		}

		id := fmt.Sprintf("chatcmpl-%d-%s", time.Now().UnixNano(), uuid.NewString()[:4])
		openAIResponse := ConvertChatHistoryToOpenAI(id, chatHistory)
		output = openAIResponse
		outputType = DataTypeOpenAIChatResponse
		transitionEval = "converted"
		taskErr = nil

	case HandleChatCompletion:
		if currentTask.ExecuteConfig == nil {
			currentTask.ExecuteConfig = &LLMExecutionConfig{}
		}

		var chatHistory ChatHistory
		var finalExecConfig *LLMExecutionConfig = currentTask.ExecuteConfig

		switch dataType {
		case DataTypeOpenAIChat:
			openAIRequest, ok := input.(OpenAIChatRequest)
			if !ok {
				return nil, DataTypeAny, "", fmt.Errorf("input data for handler %s claimed to be %s but was %T", currentTask.Handler, dataType.String(), input)
			}

			var requestConfig LLMExecutionConfig
			chatHistory, _, _, requestConfig = ConvertOpenAIToChatHistory(openAIRequest)

			finalExecConfig = &requestConfig
			if err := mergo.Merge(finalExecConfig, currentTask.ExecuteConfig, mergo.WithOverride); err != nil {
				return nil, DataTypeAny, "", fmt.Errorf("failed to merge execution configs: %w", err)
			}

		case DataTypeChatHistory:
			var ok bool
			chatHistory, ok = input.(ChatHistory)
			if !ok {
				return nil, DataTypeAny, "", fmt.Errorf("input data for handler %s claimed to be %s but was %T", currentTask.Handler, dataType.String(), input)
			}

		default:
			return nil, DataTypeAny, "", fmt.Errorf("handler '%s' requires input of type 'openai_chat' or 'chat_history', used var: %s but got '%s'", currentTask.InputVar, currentTask.Handler, dataType.String())
		}

		// Count tokens and check limits for chat completion
		modelName := getPrimaryModel(finalExecConfig)
		if _, err := exe.countChatHistoryTokens(taskCtx, modelName, chatHistory, ctxLength); err != nil {
			return nil, DataTypeAny, "", err
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
				messages := []Message{{Role: "system", Content: currentTask.SystemInstruction, Timestamp: time.Now().UTC()}}
				chatHistory.Messages = append(messages, chatHistory.Messages...)
			}
		}

		// Call the final execution function with the prepared data
		output, outputType, transitionEval, taskErr = exe.executeLLM(
			taskCtx,
			chatHistory,
			ctxLength,
			finalExecConfig,
			chainContext.ClientTools,
			chainContext.Tools,
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
			transitionEval = "no_op"
			break
		}

		lastMessage := chatHistory.Messages[len(chatHistory.Messages)-1]
		if len(lastMessage.CallTools) == 0 {
			transitionEval = "no_calls_found"
			break
		}

		executedAny := false

		for _, toolCall := range lastMessage.CallTools {
			// robust resolution: try direct key, then scan by Function.Name / HookName
			resolutionInfo, found := resolveToolWithResolution(chainContext, toolCall.Function.Name)
			if !found {
				// No matching tool wiring for this call; skip it
				continue
			}

			var args map[string]any
			if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
				taskErr = fmt.Errorf("failed to unmarshal tool arguments for %s: %w",
					toolCall.Function.Name, err)
				break
			}

			hookArgs := make(map[string]string)
			if currentTask.Hook != nil && currentTask.Hook.Args != nil {
				hookArgs = currentTask.Hook.Args
			}
			hookCall := &HookCall{
				Name:     resolutionInfo.HookName,
				ToolName: toolCall.Function.Name,
				// NOTE: dynamic args are passed as `input` to Exec; Hook.Args is static/template-level (may be empty for execute_tool_calls)
				Args: hookArgs,
			}

			// `args` are the per-call dynamic tool arguments
			result, resultType, err := exe.hookProvider.Exec(taskCtx, startingTime, args, chainContext.Debug, hookCall)
			if err != nil {
				result = fmt.Sprintf("tool %s execution failed: %s", toolCall.Function.Name, err)
				err = nil
				// Soft error instead! taskErr = fmt.Errorf("tool %s execution failed: %w", toolCall.Function.Name, err)
				// break
			}

			executedAny = true

			// Normalize result to a string for the tool message content
			var content string
			switch resultType {
			case DataTypeNil:
				content = "null"
			case DataTypeAny, DataTypeJSON:
				b, err := json.Marshal(result)
				if err != nil {
					taskErr = fmt.Errorf("failed to marshal tool %s result: %w", toolCall.Function.Name, err)
					break
				}
				content = string(b)
			default:
				content = fmt.Sprintf("%v", result)
			}

			toolResultMessage := Message{
				Role:       "tool",
				Content:    content,
				ToolCallID: toolCall.ID,
				Timestamp:  time.Now().UTC(),
			}
			chatHistory.Messages = append(chatHistory.Messages, toolResultMessage)
		}

		output = chatHistory
		outputType = DataTypeChatHistory

		switch {
		case taskErr != nil:
			transitionEval = "failed"
		case executedAny:
			transitionEval = "tools_executed"
		default:
			// We *had* tool calls, but none resolved to hooks
			transitionEval = "no_calls_found"
		}

	case HandleHook:
		if currentTask.Hook == nil {
			taskErr = fmt.Errorf("hook task missing hook definition")
		} else {
			if currentTask.Hook.Args == nil {
				currentTask.Hook.Args = make(map[string]string)
			}
			output, outputType, transitionEval, taskErr = exe.hookengine(
				taskCtx,
				startingTime,
				output,
				currentTask.Hook,
				chainContext.Debug,
				currentTask.OutputTemplate,
			)
		}

	default:
		taskErr = fmt.Errorf("unknown task type: %w -- %s", ErrUnsupportedTaskType, currentTask.Handler.String())
	}

	return output, outputType, transitionEval, taskErr
}

func (exe *SimpleExec) parseTransition(inputStr string) (string, error) {
	if inputStr == "" {
		return "", nil
	}
	trimmedInput := strings.TrimSpace(inputStr)
	if !strings.HasPrefix(trimmedInput, "/") {
		return "pass", nil
	}

	// Parse command
	parts := strings.SplitN(trimmedInput, " ", 2)
	command := strings.TrimPrefix(parts[0], "/")
	if command == "" {
		return "", fmt.Errorf("empty command")
	}

	return command, nil
}

func (exe *SimpleExec) executeLLM(
	ctx context.Context,
	input ChatHistory,
	ctxLength int,
	llmCall *LLMExecutionConfig,
	clientTools []Tool,
	hookResolution map[string]ToolWithResolution,
) (any, DataType, string, error) {
	reportErr, _, end := exe.tracker.Start(ctx, "SimpleExec", "prompt_model",
		"model_name", llmCall.Model,
		"model_names", llmCall.Models,
		"provider_types", llmCall.Providers,
		"provider_type", llmCall.Provider)
	defer end()
	providerNames := []string{}
	if llmCall.Provider != "" {
		providerNames = append(providerNames, llmCall.Provider)
	}
	if llmCall.Providers != nil {
		providerNames = append(providerNames, llmCall.Providers...)
	}

	// Count tokens if not already counted
	if input.InputTokens <= 0 {
		modelName := getPrimaryModel(llmCall)
		for _, m := range input.Messages {
			InputCount, err := exe.repo.CountTokens(ctx, modelName, m.Content)
			if err != nil {
				reportErr(fmt.Errorf("token count failed: %w", err))
				return nil, DataTypeAny, "", fmt.Errorf("token count failed: %w", err)
			}
			input.InputTokens += InputCount
		}
	}

	if ctxLength > 0 && input.InputTokens > ctxLength {
		reportErr(fmt.Errorf("input token count %d exceeds context length %d", input.InputTokens, ctxLength))
		err := fmt.Errorf("input token count %d exceeds context length %d", input.InputTokens, ctxLength)
		return nil, DataTypeAny, "", err
	}
	modelNames := []string{}
	if llmCall.Model != "" {
		modelNames = append(modelNames, llmCall.Model)
	}
	if llmCall.Models != nil {
		modelNames = append(modelNames, llmCall.Models...)
	}

	messagesC := make([]libmodelprovider.Message, 0, len(input.Messages))
	for _, m := range input.Messages {
		var toolCalls []libmodelprovider.ToolCall
		if len(m.CallTools) > 0 {
			toolCalls = make([]libmodelprovider.ToolCall, len(m.CallTools))
			for i, tc := range m.CallTools {
				toolCalls[i].ID = tc.ID
				toolCalls[i].Type = tc.Type
				toolCalls[i].Function.Name = tc.Function.Name
				toolCalls[i].Function.Arguments = tc.Function.Arguments
			}
		}

		messagesC = append(messagesC, libmodelprovider.Message{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  toolCalls,
			ToolCallID: m.ToolCallID,
		})
	}

	tools := []libmodelprovider.Tool{}
	hiddenTools := map[string]struct{}{}
	for _, toolName := range llmCall.HideTools {
		hiddenTools[toolName] = struct{}{}
	}
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

	if len(llmCall.Hooks) > 0 {
		for _, v := range llmCall.Hooks {
			for _, twr := range hookResolution {
				if v == twr.HookName {
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
	}

	resp, meta, err := exe.repo.Chat(ctx, llmrepo.Request{
		ProviderTypes: providerNames,
		ModelNames:    modelNames,
		ContextLength: input.InputTokens,
		Tracker:       exe.tracker,
	}, messagesC, libmodelprovider.WithTools(tools...))
	if err != nil {
		return nil, DataTypeAny, "", fmt.Errorf("chat failed: %w", err)
	}

	callTools := make([]ToolCall, len(resp.ToolCalls))
	for i, tc := range resp.ToolCalls {
		function := FunctionCall{
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		}
		callTools[i] = ToolCall{
			ID:       tc.ID,
			Function: function,
			Type:     tc.Type,
		}
	}
	respMessage := resp.Message
	input.Messages = append(input.Messages, Message{
		Role:      respMessage.Role,
		Content:   respMessage.Content,
		CallTools: callTools,
		Timestamp: time.Now().UTC(),
	})
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
		return input, DataTypeChatHistory, "tool-call", nil
	}

	return input, DataTypeChatHistory, "executed", nil
}

// hookengine handles the execution of a hook, including output templating.
func (exe *SimpleExec) hookengine(
	ctx context.Context,
	startingTime time.Time,
	input any,
	hook *HookCall,
	debug bool,
	outputTemplate string,
) (any, DataType, string, error) {
	if hook.Args == nil {
		hook.Args = make(map[string]string)
	}

	// Call the provider with the new, simple signature.
	hookOutput, dataType, err := exe.hookProvider.Exec(ctx, startingTime, input, debug, hook)
	if err != nil {
		return nil, dataType, "failed", err
	}

	// On success, process the output.
	finalOutput := hookOutput
	finalOutputType := dataType
	finalTransitionEval := "ok"

	// Apply the output template if it's defined.
	if outputTemplate != "" {
		rendered, tplErr := renderTemplate(outputTemplate, finalOutput)
		if tplErr != nil {
			// Return a descriptive error if templating fails.
			return nil, DataTypeAny, "failed", fmt.Errorf("failed to render hook output template: %w", tplErr)
		}
		finalOutput = rendered
		finalOutputType = DataTypeString
		finalTransitionEval = rendered
	}

	// Return the processed results.
	return finalOutput, finalOutputType, finalTransitionEval, nil
}

// condition executes a prompt and evaluates its result against a provided condition mapping.
// It returns true/false based on the resolved condition value or fallback heuristics.
func (exe *SimpleExec) condition(ctx context.Context, systemInstruction string, llmCall LLMExecutionConfig, validConditions map[string]bool, prompt string, ctxLength int) (bool, error) {
	response, err := exe.Prompt(ctx, systemInstruction, llmCall, prompt, ctxLength)
	if err != nil {
		return false, fmt.Errorf("condition: prompt execution failed: %w", err)
	}
	found := false
	for k := range validConditions {
		if k == response {
			found = true
		}
	}
	if !found {
		return false, fmt.Errorf("failed to parse into valid condition output was: %s prompt was: %s", response, prompt)
	}
	for key, val := range validConditions {
		if strings.EqualFold(response, key) {
			if val {
				return strings.EqualFold(strings.TrimSpace(response), key), nil
			}
			return !strings.EqualFold(strings.TrimSpace(response), key), nil
		}
	}

	return strings.EqualFold(strings.TrimSpace(response), "yes"), nil
}

func parseKeyValueString(input string) (map[string]any, error) {
	result := make(map[string]any)

	// Handle empty input
	if input == "" {
		return result, nil
	}

	// Try to detect delimiter - could be comma, semicolon, or newline
	var pairs []string
	if strings.Contains(input, ";") {
		pairs = strings.Split(input, ";")
	} else if strings.Contains(input, "\n") {
		pairs = strings.Split(input, "\n")
	} else {
		pairs = strings.Split(input, ",")
	}

	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		// Split by equals sign
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			// Try colon as alternative delimiter
			kv = strings.SplitN(pair, ":", 2)
			if len(kv) != 2 {
				return nil, fmt.Errorf("invalid key-value pair: %q", pair)
			}
		}

		key := strings.TrimSpace(kv[0])
		value := strings.TrimSpace(kv[1])

		// Handle quoted values
		if (strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"")) ||
			(strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'")) {
			value = value[1 : len(value)-1]
		}

		// Try to parse value as number or boolean
		if num, err := strconv.ParseFloat(value, 64); err == nil {
			if num == float64(int(num)) {
				result[key] = int(num)
			} else {
				result[key] = num
			}
		} else if value == "true" {
			result[key] = true
		} else if value == "false" {
			result[key] = false
		} else {
			result[key] = value
		}
	}

	return result, nil
}

// resolveToolWithResolution tries to find a ToolWithResolution for a given tool name.
// It first looks up by key, then falls back to scanning by Function.Name / HookName.
// This makes us robust to how chainContext.Tools was keyed.
func resolveToolWithResolution(chainContext *ChainContext, toolName string) (ToolWithResolution, bool) {
	if chainContext == nil {
		return ToolWithResolution{}, false
	}

	// 1) Direct lookup by key
	if twr, ok := chainContext.Tools[toolName]; ok {
		return twr, true
	}

	// 2) Fallback: scan for matching Function.Name or HookName
	for _, twr := range chainContext.Tools {
		if twr.Function.Name == toolName || twr.HookName == toolName {
			return twr, true
		}
	}

	return ToolWithResolution{}, false
}

// stripCodeFences removes leading/trailing Markdown code fences like:
//
// ```
// ```json
// ```javascript
//
// and their trailing ``` at the end of the string.
func stripCodeFences(s string) string {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "```") {
		return trimmed
	}

	// Drop leading ```
	trimmed = trimmed[3:]

	// Optional language tag up to the first newline
	if idx := strings.IndexByte(trimmed, '\n'); idx >= 0 {
		trimmed = trimmed[idx+1:]
	} else {
		// No newline, nothing useful left
		return strings.TrimSpace(trimmed)
	}

	// Remove trailing ``` (last occurrence)
	if idx := strings.LastIndex(trimmed, "```"); idx >= 0 {
		trimmed = trimmed[:idx]
	}

	return strings.TrimSpace(trimmed)
}

// normalizeJSResponse tries to turn the raw LLM response into pure JS source:
//
// 1. Strip Markdown fences.
// 2. If the result looks like JSON and has a "code" field, return that.
// 3. Otherwise, return the stripped text as-is.
func normalizeJSResponse(raw string) string {
	if raw == "" {
		return ""
	}

	// Step 1: strip ``` fences if present
	trimmed := stripCodeFences(raw)
	trimmed = strings.TrimSpace(trimmed)
	if trimmed == "" {
		return ""
	}

	// Step 2: if it's JSON, try to extract { "code": "..." }
	if strings.HasPrefix(trimmed, "{") {
		var obj struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal([]byte(trimmed), &obj); err == nil && obj.Code != "" {
			return obj.Code
		}
	}

	// Step 3: fall back to treating the stripped text as JS
	return trimmed
}
