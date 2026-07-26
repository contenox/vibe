package modelrepo_test

import (
	"os"
	"strings"
	"testing"
)

const runVLLMTestsEnv = "CONTENOX_RUN_VLLM_TESTS"

func requireVLLMContainerTests(t *testing.T) {
	t.Helper()
	if envTruthy(os.Getenv(runVLLMTestsEnv)) {
		return
	}
	t.Skipf("skipping vLLM container test; set %s=1 to run", runVLLMTestsEnv)
}

func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}
