package playground_test

import (
	"strings"
	"testing"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/execservice"
	"github.com/contenox/vibe/playground"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystem_ExecService(t *testing.T) {
	ctx := t.Context()

	p := playground.New()
	p.WithPostgresTestContainer(ctx)
	p.WithNats(ctx)
	p.WithRuntimeState(ctx, true) // Enable groups for model assignment
	p.WithMockTokenizer()
	p.WithInternalPromptExecutor(ctx, "smollm2:135m", 2048)

	p.WithOllamaBackend(ctx, "prompt-backend", "latest", false, true) // assignTasksModel = true
	p.StartBackgroundRoutines(ctx)
	p.WithLLMRepo()
	p.StartBackgroundRoutines(ctx)

	require.NoError(t, p.GetError(), "Playground setup failed")
	defer p.CleanUp()

	err := p.WaitUntilModelIsReady(ctx, "prompt-backend", "smollm2:135m")
	require.NoError(t, err, "Model 'smollm2:135m' did not become ready in time")

	execSvc, err := p.GetExecService(ctx)
	require.NoError(t, err, "Failed to get ExecService")

	t.Run("Successful Execution", func(t *testing.T) {
		// Define a valid request targeting the configured model.
		req := &execservice.TaskRequest{
			Prompt:        "In one word, what is the color of the sky on a clear day?",
			ModelName:     "smollm2:135m",
			ModelProvider: "ollama",
		}

		// Execute the service method.
		resp, err := execSvc.Execute(ctx, req)

		// Assertions for a successful call.
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.NotEmpty(t, resp.ID, "Response should have a generated ID")
		assert.NotEmpty(t, resp.Response, "Response from LLM should not be empty")
		resp.Response = strings.ToLower(resp.Response)
		assert.Contains(t, resp.Response, "blue", "Response should contain the expected answer")
	})

	t.Run("Failure on Nil Request", func(t *testing.T) {
		// Execute with a nil request pointer.
		_, err := execSvc.Execute(ctx, nil)

		// Assert that the specific framework error for empty requests is returned.
		require.Error(t, err)
		assert.ErrorIs(t, err, apiframework.ErrEmptyRequest, "Expected error for nil request")
	})

	t.Run("Failure on Empty Prompt", func(t *testing.T) {
		// Define a request with an empty prompt string.
		req := &execservice.TaskRequest{
			Prompt:        "",
			ModelName:     "smollm2:135m",
			ModelProvider: "ollama",
		}

		// Execute and check for the validation error.
		_, err := execSvc.Execute(ctx, req)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "prompt is empty", "Expected error for empty prompt")
		assert.ErrorIs(t, err, apiframework.ErrEmptyRequestBody, "Expected underlying error for empty body field")
	})
}
