package contenoxcli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/contenox/contenox/taskengine"
)

func TestValidateExecutorChain_embeddedStepExecutor(t *testing.T) {
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal([]byte(chainStepExecutor), &chain); err != nil {
		t.Fatal(err)
	}
	if err := validateExecutorChain(&chain, "chain-step-executor.json"); err != nil {
		t.Fatal(err)
	}
}

func TestValidateExecutorChain_agenticLoopMustClose(t *testing.T) {
	badJSON := strings.ReplaceAll(chainStepExecutor, `"goto": "plan_step_agent_loop_chat"`, `"goto": "end"`)
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal([]byte(badJSON), &chain); err != nil {
		t.Fatal(err)
	}
	if err := validateExecutorChain(&chain, "broken.json"); err == nil {
		t.Fatal("expected error when execute_tool_calls does not branch back to chat task")
	}
}
