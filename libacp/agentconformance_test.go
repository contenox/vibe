package libacp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests validate libacp's agent-side wire dispatch against real,
// independently-implemented ACP clients from the Rust reference SDK. Opt-in:
// both tests skip unless the caller points at a local build of the reference
// binary via environment variable (see `make acp-conformance`).
const (
	acpValidatorBinEnv = "ACP_VALIDATOR_BIN"
	acpYopoBinEnv      = "ACP_YOPO_BIN"
)

// buildStubAgent compiles the hermetic libacp/cmd/acp-stub-agent (no LLM
// backend needed) into t.TempDir() and returns its path.
func buildStubAgent(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "acp-stub-agent")
	cmd := exec.Command("go", "build", "-o", binPath, "github.com/contenox/contenox/libacp/cmd/acp-stub-agent")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build acp-stub-agent: %v\n%s", err, out)
	}
	return binPath
}

// acpCheckResult mirrors one row of acp-validator --json's output
// (acp-validator/src/report.rs: CheckResult).
type acpCheckResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func (r acpCheckResult) String() string {
	return fmt.Sprintf("%-36s %-6s %s", r.Name, strings.ToUpper(r.Status), r.Detail)
}

// TestConformance_StubAgentPassesACPValidator runs the Rust acp-validator
// conformance checker (initialize, session lifecycle, streaming, permissions,
// fs callbacks, cancellation, set_mode, auth, update ordering, ...) against
// the hermetic Go stub agent.
func TestConformance_StubAgentPassesACPValidator(t *testing.T) {
	validatorBin := os.Getenv(acpValidatorBinEnv)
	if validatorBin == "" {
		t.Skipf("skipping: set %s to a built acp-validator binary to run (see `make acp-conformance`)", acpValidatorBinEnv)
	}
	if _, err := os.Stat(validatorBin); err != nil {
		t.Fatalf("%s=%q is not accessible: %v", acpValidatorBinEnv, validatorBin, err)
	}

	agentBin := buildStubAgent(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, validatorBin, "--agent", agentBin, "--json", "--timeout", "20")
	out, runErr := cmd.Output()
	var stderr string
	if ee, ok := runErr.(*exec.ExitError); ok {
		stderr = string(ee.Stderr)
	}

	var results []acpCheckResult
	if err := json.Unmarshal(out, &results); err != nil {
		t.Fatalf("parse acp-validator --json output: %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
	}
	if len(results) == 0 {
		t.Fatalf("acp-validator reported no checks at all\nstdout:\n%s\nstderr:\n%s", out, stderr)
	}

	var table strings.Builder
	var failed []acpCheckResult
	for _, r := range results {
		table.WriteString(r.String())
		table.WriteByte('\n')
		if strings.EqualFold(r.Status, "fail") {
			failed = append(failed, r)
		}
	}
	t.Logf("acp-validator results (%d checks):\n%s", len(results), table.String())

	if len(failed) > 0 {
		t.Fatalf("acp-validator reported %d failing check(s) against acp-stub-agent:\n%s", len(failed), table.String())
	}
}

// TestConformance_StubAgentYopoSmoke drives the stub agent with yopo, the
// Rust SDK's one-shot reference client, as a smoke test against a second,
// independent client implementation.
func TestConformance_StubAgentYopoSmoke(t *testing.T) {
	yopoBin := os.Getenv(acpYopoBinEnv)
	if yopoBin == "" {
		t.Skipf("skipping: set %s to a built yopo binary to run (see `make acp-conformance`)", acpYopoBinEnv)
	}
	if _, err := os.Stat(yopoBin); err != nil {
		t.Fatalf("%s=%q is not accessible: %v", acpYopoBinEnv, yopoBin, err)
	}

	agentBin := buildStubAgent(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, yopoBin, "hello from the yopo smoke test", agentBin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("yopo one-shot prompt against acp-stub-agent failed: %v\noutput:\n%s", err, out)
	}
	t.Logf("yopo output:\n%s", out)
	if len(strings.TrimSpace(string(out))) == 0 {
		t.Fatalf("yopo produced no output for a one-shot prompt against acp-stub-agent")
	}
}
