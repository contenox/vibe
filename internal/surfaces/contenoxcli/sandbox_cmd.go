package contenoxcli

import (
	"fmt"
	"os"
	"strings"

	"github.com/contenox/beam/internal/libsandbox"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/spf13/cobra"
)

var sandboxCmd = &cobra.Command{
	Use:   "sandbox",
	Short: "Inspect the sandbox that confines the shells contenox spawns.",
	Long: `Inspect the confinement contenox applies to the shells it spawns.

Today this covers the environment scrub: which of the process's own environment
variables an agent-reachable shell (the local_shell tool and the shell_session
PTY) is allowed to inherit, so credentials in that environment do not leak into
a shell an agent can drive.

Configure it with the SANDBOX_SHELL_SCRUB / SANDBOX_TERMINAL_SCRUB modes ("off",
"deny-secrets", "strict") and the SANDBOX_ENV_ALLOW / SANDBOX_ENV_DENY lists
(comma/whitespace-separated names or single-wildcard globs like "LC_*"). Operator-
injected variables layer on top regardless of mode — see 'contenox shell-env'.`,
}

var sandboxEnvCmd = &cobra.Command{
	Use:   "env",
	Short: "Preview which environment variables a spawned shell would inherit.",
	Long: `Print the environment-variable NAMES a spawned shell would inherit under the
currently-configured scrub policy, evaluated against THIS process's environment —
a dry run so you can confirm the policy strips what you expect before trusting it.

By default it shows the agent-shell policy (SANDBOX_SHELL_SCRUB, default
deny-secrets); pass --terminal for the interactive-terminal policy
(SANDBOX_TERMINAL_SCRUB, default off). Values are withheld — only names are
printed — so the output is safe to share.`,
	Args: cobra.NoArgs,
	RunE: runSandboxEnv,
}

func init() {
	sandboxEnvCmd.Flags().Bool("terminal", false, "Show the interactive-terminal policy instead of the agent-shell policy.")
	sandboxCmd.AddCommand(sandboxEnvCmd)
}

func runSandboxEnv(cmd *cobra.Command, _ []string) error {
	config := &sandboxEnvConfig{}
	if err := loadEnvConfig(config); err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	db, _, err := openConfigDB(cmd)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	// Same composition every spawn root applies (resolvedSandboxEnv): this
	// preview can never drift from what a spawned shell actually receives.
	shellScrub, terminalScrub, err := resolvedSandboxEnv(db, libtracker.NoopTracker{}, cmd.ErrOrStderr())
	if err != nil {
		return fmt.Errorf("resolve sandbox env: %w", err)
	}

	// nil warnW below: purely relabeling values the call above already resolved.
	terminal, _ := cmd.Flags().GetBool("terminal")
	surface := "agent shells (local_shell, shell_session)"
	mode := resolveScrubMode(config.SandboxShellScrub, libsandbox.ScrubDenySecrets, nil)
	scrub := shellScrub
	if terminal {
		surface = "interactive terminal"
		mode = resolveScrubMode(config.SandboxTerminalScrub, libsandbox.ScrubOff, nil)
		scrub = terminalScrub
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "# %s — scrub mode: %s\n", surface, mode)
	if scrub == nil {
		fmt.Fprintln(out, "# mode is off: the full environment is inherited (no scrubbing).")
		return nil
	}

	parent := os.Environ()
	kept := scrub(parent) // KEY=VALUE, sorted by name
	fmt.Fprintf(out, "# %d of %d variables pass; values withheld.\n", len(kept), len(parent))
	for _, kv := range kept {
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			fmt.Fprintln(out, kv[:eq])
		}
	}
	return nil
}
