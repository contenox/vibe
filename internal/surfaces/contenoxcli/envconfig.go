package contenoxcli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// sandboxEnvConfig is the SANDBOX_* environment configuration for the shells
// contenox spawns. Formerly part of serve's Config; the scrub outlived the
// server because it guards every spawned shell, not an HTTP surface.
type sandboxEnvConfig struct {
	// SandboxShellScrub selects how the environment handed to an agent-reachable
	// shell (the local_shell tool and the ACP "!"/shell_session PTY) is scrubbed
	// of the parent process's own credentials: "deny-secrets" (the default — pass
	// everything except the control plane and known credential shapes), "strict"
	// (pass only a safe base set plus SandboxEnvAllow), or "off" (inherit
	// everything, the legacy behavior). See libsandbox.EnvPolicyForMode.
	SandboxShellScrub string `json:"sandbox_shell_scrub"`
	// SandboxTerminalScrub is the same posture for an interactive terminal — a
	// trusted operator shell, so it defaults to "off".
	SandboxTerminalScrub string `json:"sandbox_terminal_scrub"`
	// SandboxEnvAllow adds variable names or globs (comma/whitespace-separated,
	// e.g. "GOCACHE,CARGO_HOME,HTTP_PROXY") to whichever scrub is active — the
	// toolchain and deployment variables a shell needs that are not in the safe
	// base set.
	SandboxEnvAllow string `json:"sandbox_env_allow"`
	// SandboxEnvDeny adds variable names or globs always dropped by an active
	// scrub, on top of the built-in denies.
	SandboxEnvDeny string `json:"sandbox_env_deny"`
}

// loadEnvConfig populates cfg from environment variables (lowercased keys
// mapped to json tags).
func loadEnvConfig[T any](cfg *T) error {
	if cfg == nil {
		return fmt.Errorf("config pointer is nil")
	}
	config := map[string]string{}
	for _, kvPair := range os.Environ() {
		ar := strings.SplitN(kvPair, "=", 2)
		if len(ar) < 2 {
			continue
		}
		config[strings.ToLower(ar[0])] = ar[1]
	}
	b, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal env vars: %w", err)
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return fmt.Errorf("failed to unmarshal into config struct: %w", err)
	}
	return nil
}
