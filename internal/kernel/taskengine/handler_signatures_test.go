package taskengine_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/kernel/tools"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/models/llmrepo"
	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

// TestUnit_HandlerSignatures_MatchRealExecutor drives the REAL SimpleExec
// against every (handler × concrete DataType) pair and asserts that acceptance
// and the produced output type match the frozen table in handler_signatures.go.
// This is the behavioral half of the registry invariant: if taskexec.go's
// switch drifts (a handler starts tolerating a new type, or stops), this test
// fails in the package that owns the contract.
func TestUnit_HandlerSignatures_MatchRealExecutor(t *testing.T) {
	repo := &mockModelRepo{
		chatFunc: func(_ context.Context, _ llmrepo.Request, _ []libmodelprovider.Message, _ ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
			return libmodelprovider.ChatResult{
				Message: libmodelprovider.Message{Role: "assistant", Content: "ok"},
			}, llmrepo.Meta{ModelName: "test-model"}, nil
		},
		promptFunc: func(_ context.Context, _ llmrepo.Request, _ string, _ float32, _ string) (string, llmrepo.Meta, error) {
			return "yes", llmrepo.Meta{ModelName: "test-model"}, nil
		},
	}
	toolsRepo := tools.NewMockToolsRegistry().
		WithResponse("echo", tools.ToolsResponse{Output: "echoed"})
	exec, err := taskengine.NewExec(context.Background(), repo, toolsRepo, libtracker.NoopTracker{})
	require.NoError(t, err)

	// One representative runtime value per concrete DataType. DataTypeAny is
	// deliberately absent: Any is "unknown at load", not a value shape.
	inputs := map[taskengine.DataType]any{
		taskengine.DataTypeString: "boom",
		taskengine.DataTypeInt:    7,
		taskengine.DataTypeJSON:   map[string]any{"k": "v"},
		taskengine.DataTypeChatHistory: taskengine.ChatHistory{
			Messages: []taskengine.Message{{Role: "user", Content: "boom", Timestamp: time.Now().UTC()}},
		},
		taskengine.DataTypeNil: nil,
	}

	taskFor := func(h taskengine.TaskHandler) *taskengine.TaskDefinition {
		task := &taskengine.TaskDefinition{
			ID:            "probe",
			Handler:       h,
			ExecuteConfig: &taskengine.LLMExecutionConfig{Model: "test-model"},
		}
		switch h {
		case taskengine.HandleRoute:
			task.Transition = taskengine.TaskTransition{Branches: []taskengine.TransitionBranch{
				{Operator: taskengine.OpEquals, When: "yes", Goto: taskengine.TermEnd},
				{Operator: taskengine.OpEquals, When: "no", Goto: taskengine.TermEnd},
			}}
		case taskengine.HandleTools:
			task.Tools = &taskengine.ToolsCall{Name: "echo", ToolName: "echo"}
		}
		return task
	}

	handlers := []taskengine.TaskHandler{
		taskengine.HandleChatCompletion,
		taskengine.HandleExecuteToolCalls,
		taskengine.HandleRoute,
		taskengine.HandleTools,
		taskengine.HandleNoop,
		taskengine.HandleRaiseError,
	}

	for _, handler := range handlers {
		sig, ok := taskengine.HandlerSignatureFor(handler)
		require.True(t, ok, "handler %q missing from the signature registry", handler)

		for dt, value := range inputs {
			dt, value := dt, value
			dtName := dt
			t.Run(string(handler)+"/"+dtName.String(), func(t *testing.T) {
				out, outType, eval, execErr := exec.TaskExec(
					context.Background(), time.Now().UTC(), 0,
					&taskengine.ChainContext{}, taskFor(handler), value, dt)

				if handler == taskengine.HandleRaiseError {
					// raise_error never succeeds. The registry's accept set
					// distinguishes WHOSE error is raised: an accepted input
					// becomes the author's message, a rejected one surfaces
					// getPrompt's type complaint.
					require.Error(t, execErr)
					if sig.AcceptsInput(dt) {
						require.NotContains(t, execErr.Error(), "unsupported input type")
					} else {
						require.Contains(t, execErr.Error(), "unsupported input type")
					}
					return
				}

				if !sig.AcceptsInput(dt) {
					require.Error(t, execErr,
						"registry says %s rejects %s but the executor accepted it", handler, dtName.String())
					return
				}
				require.NoError(t, execErr,
					"registry says %s accepts %s but the executor rejected it (eval %q)", handler, dtName.String(), eval)

				switch sig.Mode {
				case taskengine.HandlerOutputFixed:
					require.Equal(t, sig.Output, outType,
						"registry says %s produces a fixed type", handler)
				case taskengine.HandlerOutputPassthrough:
					require.Equal(t, dt, outType,
						"registry says %s passes its input type through", handler)
					require.Equal(t, value, out,
						"registry says %s passes its input through unchanged", handler)
				case taskengine.HandlerOutputDynamic:
					// The output type is the tool's to decide; nothing to pin.
				}

				if sig.SuccessEvals != nil {
					require.Contains(t, sig.SuccessEvals, eval,
						"handler %s emitted eval %q outside its declared vocabulary", handler, eval)
				}
			})
		}
	}
}
