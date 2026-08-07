package agentservice_test

// Prompt must wrap a chain failure in "chain execution failed" exactly once,
// leading with the root task's error when its on_failure handler also fails.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/kernel/tools"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/execservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

func TestUnit_Prompt_ChainFailureWrappedOnceRootErrorFirst(t *testing.T) {
	rootErr := errors.New("stream failed (provider=vertex-google, model=gemini-3.6-flash): 404")
	handlerErr := errors.New("summariser also down")
	exec := &taskengine.MockTaskExecutor{
		ErrorSequence: []error{rootErr, handlerErr},
	}
	env, err := taskengine.NewEnv(context.Background(), libtracker.NoopTracker{}, exec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	db, err := libdb.NewSQLiteDBManager(context.Background(), filepath.Join(t.TempDir(), "test.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	ag := agentservice.New(agentservice.Deps{
		Engine: &enginesvc.Engine{
			TaskService: execservice.NewTasksEnv(context.Background(), env, tools.NewMockToolsRegistry()),
			Tracker:     libtracker.NoopTracker{},
		},
		DB: db,
	})

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "main",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					OnFailure: "summarise_failure",
				},
			},
			{
				ID:      "summarise_failure",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	_, err = ag.Prompt(libtracker.WithNewRequestID(context.Background()), agentservice.PromptRequest{
		Chain:      chain,
		InputValue: "input",
		InputType:  taskengine.DataTypeString,
	})

	require.Error(t, err)
	require.Equal(t, 1, strings.Count(err.Error(), "chain execution failed"), "the chain-failure wrap must appear exactly once")
	require.EqualError(t, err, "chain execution failed: task main failed after 0 retries: task main: "+rootErr.Error()+" (on_failure handler also failed: task summarise_failure: "+handlerErr.Error()+")")
	require.ErrorIs(t, err, rootErr, "root cause must stay reachable via errors.Is")
}
