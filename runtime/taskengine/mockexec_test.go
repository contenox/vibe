package taskengine_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/stretchr/testify/require"
)

func TestUnit_TaskExec_PromptToString(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockOutput:          "mock-result",
		MockTransitionValue: "mock-response",
		MockError:           nil,
	}

	task := &taskengine.TaskDefinition{
		Handler: taskengine.HandlePromptToString,
	}

	output, _, _, err := mockExec.TaskExec(context.Background(), time.Now(), 100, &taskengine.ChainContext{}, task, "What is 2+2?", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "mock-result", output)
}

// stubEnvExecutor is a minimal EnvExecutor that records the chain it received.
type stubEnvExecutor struct {
	receivedPrompt string
}

func (s *stubEnvExecutor) ExecEnv(ctx context.Context, chain *taskengine.TaskChainDefinition, input any, dataType taskengine.DataType) (any, taskengine.DataType, []taskengine.CapturedStateUnit, error) {
	if len(chain.Tasks) > 0 {
		s.receivedPrompt = chain.Tasks[0].PromptTemplate
	}
	return input, dataType, nil, nil
}

func singleTaskChain(prompt string) *taskengine.TaskChainDefinition {
	return &taskengine.TaskChainDefinition{
		ID: "test-chain",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "t1",
				Handler:        taskengine.HandlePromptToString,
				PromptTemplate: prompt,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}
}

// TestUnit_MacroEnv_Var_HappyPath verifies that a {{var:key}} expands to the
// value injected via WithTemplateVars.
func TestUnit_MacroEnv_Var_HappyPath(t *testing.T) {
	inner := &stubEnvExecutor{}
	env, err := taskengine.NewMacroEnv(inner, nil)
	require.NoError(t, err)

	ctx := taskengine.WithTemplateVars(context.Background(), map[string]string{
		"greeting": "hello",
	})

	_, _, _, err = env.ExecEnv(ctx, singleTaskChain("{{var:greeting}} world"), "in", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "hello world", inner.receivedPrompt)
}

// TestUnit_MacroEnv_Var_MissingKey verifies that a {{var:key}} whose key was
// never added to the vars map returns an error — not a silent empty string.
func TestUnit_MacroEnv_Var_MissingKey(t *testing.T) {
	inner := &stubEnvExecutor{}
	env, err := taskengine.NewMacroEnv(inner, nil)
	require.NoError(t, err)

	ctx := taskengine.WithTemplateVars(context.Background(), map[string]string{
		"other": "value",
	})

	_, _, _, err = env.ExecEnv(ctx, singleTaskChain("prefix {{var:missing}} suffix"), "in", taskengine.DataTypeString)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing")
}

// TestUnit_MacroEnv_Var_NoVarsInContext verifies that using {{var:key}} when
// WithTemplateVars was never called also returns an error.
func TestUnit_MacroEnv_Var_NoVarsInContext(t *testing.T) {
	inner := &stubEnvExecutor{}
	env, err := taskengine.NewMacroEnv(inner, nil)
	require.NoError(t, err)

	// Deliberately use a plain context with no vars attached.
	_, _, _, err = env.ExecEnv(context.Background(), singleTaskChain("{{var:anything}}"), "in", taskengine.DataTypeString)
	require.Error(t, err)
}
