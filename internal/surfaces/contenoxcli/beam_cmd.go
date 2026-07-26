package contenoxcli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// beamCmd is a teaching stub while the beam TUI is in development. The name is
// reserved (reservedSubcommands) so `contenox beam .` errors honestly instead
// of being injected as chat input and burning a model turn on the words
// "beam ." — the same protection the retired command names get. The command is
// Hidden so --help stays truthful about what actually works; typing the name
// teaches instead.
var beamCmd = &cobra.Command{
	Use:    "beam",
	Short:  "The contenox terminal UI (in development).",
	Hidden: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		out := cmd.OutOrStdout()
		fmt.Fprintln(out, "beam — the contenox terminal UI — is in development.")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Until it ships:")
		fmt.Fprintln(out, "  contenox \"your prompt\"     chat from the terminal")
		fmt.Fprintln(out, "  contenox acp               the same sessions inside Zed, JetBrains, or any ACP editor")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "The plan: docs/development/blueprints/beam-tui.md")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(beamCmd)
}
