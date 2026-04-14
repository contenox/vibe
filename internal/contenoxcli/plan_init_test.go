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

func TestValidateExecutorChain_embeddedStepExecutorGated(t *testing.T) {
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal([]byte(chainStepExecutorGated), &chain); err != nil {
		t.Fatal(err)
	}
	if err := validateExecutorChain(&chain, "chain-step-executor-gated.json"); err != nil {
		t.Fatal(err)
	}
}

func TestValidatePlanExplorerChain_embedded(t *testing.T) {
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal([]byte(chainPlanExplorer), &chain); err != nil {
		t.Fatal(err)
	}
	if err := validatePlanExplorerChain(&chain, "chain-plan-explorer.json"); err != nil {
		t.Fatalf("embedded explorer should validate: %v", err)
	}
}

func TestValidatePlanExplorerChain_rejectsLocalShell(t *testing.T) {
	bad := strings.Replace(chainPlanExplorer, `"hooks": ["local_fs"]`, `"hooks": ["local_fs", "local_shell"]`, 1)
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal([]byte(bad), &chain); err != nil {
		t.Fatal(err)
	}
	err := validatePlanExplorerChain(&chain, "tampered.json")
	if err == nil {
		t.Fatal("expected error when explorer allowlists local_shell")
	}
	if !strings.Contains(err.Error(), "local_shell") {
		t.Fatalf("error must reference local_shell, got: %v", err)
	}
}

func TestValidatePlanExplorerChain_rejectsWildcardHooks(t *testing.T) {
	bad := strings.Replace(chainPlanExplorer, `"hooks": ["local_fs"]`, `"hooks": ["*"]`, 1)
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal([]byte(bad), &chain); err != nil {
		t.Fatal(err)
	}
	if err := validatePlanExplorerChain(&chain, "tampered.json"); err == nil {
		t.Fatal("expected error on wildcard hooks")
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
