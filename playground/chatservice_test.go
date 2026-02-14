package playground_test

import (
	"strings"
	"testing"

	"github.com/contenox/vibe/playground"
	"github.com/contenox/vibe/taskengine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatService_OpenAIChatCompletions(t *testing.T) {
	ctx := t.Context()

	p := playground.New()
	p.WithPostgresTestContainer(ctx)
	p.WithNats(ctx)
	p.WithRuntimeState(ctx, true)
	p.WithMockTokenizer()
	p.WithMockHookRegistry()
	p.WithInternalPromptExecutor(ctx, "smollm2:135m", 2048)
	p.WithInternalChatExecutor(ctx, "smollm2:135m", 2048)
	p.WithOllamaBackend(ctx, "prompt-backend", "latest", false, true)
	p.StartBackgroundRoutines(ctx)
	p.WithLLMRepo()

	require.NoError(t, p.GetError(), "Playground setup failed")
	defer p.CleanUp()

	// Wait for model to be ready
	err := p.WaitUntilModelIsReady(ctx, "prompt-backend", "smollm2:135m")
	require.NoError(t, err, "Model not ready in time")

	// Create chat service
	chatService, err := p.GetChatService(ctx)
	require.NoError(t, err, "Failed to get ChatService")

	// Run individual test cases
	t.Run("ModelExecutionHandler_ProcessesOpenAIChatInput", func(t *testing.T) {
		// Create a task chain definitions
		td := []*taskengine.TaskChainDefinition{
			&taskengine.TaskChainDefinition{
				ID:          "openai-direct-exec-chain-magic",
				Description: "Test chain for OpenAI chat completions with model execution",
				TokenLimit:  2048,
				Tasks: []taskengine.TaskDefinition{
					{
						ID:                "chat_task",
						Description:       "Process OpenAI chat request",
						Handler:           taskengine.HandleChatCompletion,
						SystemInstruction: "You are a task processing engine talking to other machines. Return the direct answer without explanation to the given task.",
						ExecuteConfig: &taskengine.LLMExecutionConfig{
							Model:    "smollm2:135m",
							Provider: "ollama",
						},
						Transition: taskengine.TaskTransition{
							OnFailure: "",
							Branches: []taskengine.TransitionBranch{
								{
									Operator: taskengine.OpDefault,
									Goto:     taskengine.TermEnd,
								},
							},
						},
					},
				},
			},
			&taskengine.TaskChainDefinition{
				ID:          "openai-direct-exec-chain-simple",
				Description: "Test chain for OpenAI chat completions with model execution",
				TokenLimit:  2048,
				Tasks: []taskengine.TaskDefinition{
					{
						ID:                "chat_task",
						Description:       "Process OpenAI chat request",
						Handler:           taskengine.HandleChatCompletion,
						SystemInstruction: "You are a task processing engine talking to other machines. Return the direct answer without explanation to the given task.",
						ExecuteConfig: &taskengine.LLMExecutionConfig{
							Model:    "smollm2:135m",
							Provider: "ollama",
						},
						Transition: taskengine.TaskTransition{
							OnFailure: "",
							Branches: []taskengine.TransitionBranch{
								{
									Operator: taskengine.OpDefault,
									Goto:     "format_output",
								},
							},
						},
					},
					{
						ID:          "format_output",
						Description: "Format response as OpenAI chat completion",
						Handler:     taskengine.HandleConvertToOpenAIChatResponse,
						Transition: taskengine.TaskTransition{
							OnFailure: "",
							Branches: []taskengine.TransitionBranch{
								{
									Operator: taskengine.OpDefault,
									Goto:     taskengine.TermEnd,
								},
							},
						},
					},
				},
			},
		}

		// Save the task chain
		chainService, err := p.GetTaskChainService()
		require.NoError(t, err)
		for _, tcd := range td {
			t.Run("test-"+tcd.ID, func(t *testing.T) {
				err = chainService.Create(ctx, tcd)
				require.NoError(t, err)

				// Create an OpenAI chat request
				req := taskengine.OpenAIChatRequest{
					Model: "smollm2:135m",
					Messages: []taskengine.OpenAIChatRequestMessage{
						{
							Role:    "user",
							Content: "What is the capital of Italy? Respond only with the city name.",
						},
					},
					Temperature: 0.1,
				}

				// Call OpenAIChatCompletions
				response, stackTrace, err := chatService.OpenAIChatCompletions(ctx, tcd.ID, req)
				require.NoError(t, err)
				require.NotNil(t, response)
				require.NotEmpty(t, stackTrace)

				// Verify the response structure
				assert.Equal(t, "chat.completion", response.Object)
				assert.NotEmpty(t, response.ID)
				assert.Len(t, response.Choices, 1)
				assert.Equal(t, "assistant", response.Choices[0].Message.Role)

				// Verify the content contains the expected answer
				if response.Choices[0].Message.Content == nil {
					t.Errorf("response.Choices[0].Message is nil")
				}
				message := response.Choices[0].Message.Content
				assert.Contains(t, strings.ToLower(*message), "rome")
			})
		}
	})
}
