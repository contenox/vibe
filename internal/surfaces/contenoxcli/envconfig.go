package contenoxcli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// sandboxEnvConfig is the SANDBOX_* environment configuration for shells
// contenox spawns.
type sandboxEnvConfig struct {
	// SandboxShellScrub is "deny-secrets" (default), "strict", or "off".
	SandboxShellScrub string `json:"sandbox_shell_scrub"`
	// SandboxTerminalScrub is the same posture for an interactive terminal,
	// defaulting to "off".
	SandboxTerminalScrub string `json:"sandbox_terminal_scrub"`
	// SandboxEnvAllow adds variable names or globs to whichever scrub is
	// active (comma/whitespace-separated).
	SandboxEnvAllow string `json:"sandbox_env_allow"`
	// SandboxEnvDeny adds variable names or globs always dropped, on top of
	// the built-in denies.
	SandboxEnvDeny string `json:"sandbox_env_deny"`
}

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
