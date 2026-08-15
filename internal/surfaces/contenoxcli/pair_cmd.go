package contenoxcli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/contenox/contenox/internal/relaycreds"
	"github.com/contenox/contenox/internal/relaypair"
	"github.com/spf13/cobra"
)

// Pairing from the CLI, not only from inside a session: a pairing describes
// the machine and is stored in ~/.contenox, so whichever surface writes it,
// every later process finds it and dials with it. Requiring an editor session
// to type /pair into made the hosted app unreachable for anyone who does not
// drive contenox from an editor — which is most first-time users.
//
// The session command stays: answering "am I paired?" without leaving the
// session is worth its own entry point. Both call the same relaypair.Redeem.
var pairCmd = &cobra.Command{
	Use:   "pair [key] [relay-endpoint]",
	Short: "Attach this machine to a relay so the contenox app can reach it.",
	Long: `Attach this machine to a relay using a key minted in the contenox app.

With no key, prints this machine's current pairing and the app URL.

The key is short-lived and can be redeemed exactly once. A pairing describes
this machine, so it is stored in ~/.contenox/relay.json and every contenox
process on this machine uses it.

Examples:
  contenox pair                 # what is this machine attached to?
  contenox pair K7M-3PQ         # redeem a key from the app
  contenox pair K7M-3PQ https://relay.example.internal   # a self-hosted relay`,
	Args: cobra.MaximumNArgs(2),
	RunE: runPair,
}

var unpairCmd = &cobra.Command{
	Use:   "unpair",
	Short: "Remove this machine's stored pairing.",
	Long: `Delete this machine's stored relay credentials.

This is local: it stops this machine dialling out, but it does not revoke
anything. Revoke an instance in the app — a revoked machine is refused at its
next dial whether or not it still holds the file.`,
	Args: cobra.NoArgs,
	RunE: runUnpair,
}

func init() {
	rootCmd.AddCommand(pairCmd)
	rootCmd.AddCommand(unpairCmd)
}

func runPair(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	dir, err := globalContenoxDir()
	if err != nil {
		return err
	}

	if len(args) == 0 {
		writePairingStatus(out, dir)
		return nil
	}

	explicit := ""
	if len(args) == 2 {
		explicit = args[1]
	}
	endpoint := relaypair.Endpoint(explicit)

	// The hostname, never a typed name: a pairing identifies this machine.
	// Collisions are uniquified by the relay.
	name, err := os.Hostname()
	if err != nil || name == "" {
		return fmt.Errorf("cannot determine this machine's hostname, so it has no name to pair under")
	}

	creds, err := relaypair.Redeem(cmd.Context(), nil, endpoint, args[0], name)
	if err != nil {
		switch {
		case errors.Is(err, relaypair.ErrKeyRejected):
			// The relay's message verbatim: it already names the remedy, and
			// it does not resolve unknown from expired from spent, so neither
			// does this.
			return err
		case errors.Is(err, relaypair.ErrRelayUnusable):
			return err
		default:
			return fmt.Errorf("could not reach the relay: %w", err)
		}
	}
	if err := relaycreds.Save(dir, creds); err != nil {
		return fmt.Errorf("paired, but the credentials could not be stored: %w", err)
	}

	fmt.Fprintf(out, "Paired as %q (instance %s).\n", name, creds.InstanceID)
	if creds.AccountID != "" {
		fmt.Fprintf(out, "Attached to account %s.\n", creds.AccountID)
	}
	fmt.Fprintf(out, "Relay: %s\n", creds.Endpoint)
	fmt.Fprintf(out, "Stored in %s — 'contenox unpair' removes it.\n", relaycreds.Path(dir))
	if origin, err := relaypair.AppOrigin(creds.Endpoint); err == nil {
		fmt.Fprintf(out, "\nOpen %s to reach this machine.\n", origin)
	}
	// Pairing alone attaches the machine; something has to be running for the
	// app to attach to. Say so here rather than leaving a paired-but-silent
	// machine looking broken.
	fmt.Fprintf(out, "Keep it reachable with: contenox serve\n")
	return nil
}

func runUnpair(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	dir, err := globalContenoxDir()
	if err != nil {
		return err
	}
	if _, err := relaycreds.Load(dir); err != nil {
		if errors.Is(err, relaycreds.ErrNotEnrolled) {
			fmt.Fprintln(out, "This machine is not paired with a relay — nothing to do.")
			return nil
		}
		// An unparseable file is still the file this command deletes.
	}
	if err := relaycreds.Delete(dir); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed %s.\n", relaycreds.Path(dir))
	fmt.Fprintln(out, "This machine will no longer dial the relay. Revoking the instance is done in the app.")
	return nil
}

// writePairingStatus renders the stored pairing, never the credential. Shared
// with the serve screen so the two cannot drift into describing the same state
// differently.
func writePairingStatus(w io.Writer, dir string) {
	creds, err := relaycreds.Load(dir)
	if err != nil {
		if errors.Is(err, relaycreds.ErrNotEnrolled) {
			fmt.Fprintln(w, "This machine is not paired with a relay.")
			fmt.Fprintf(w, "\nTo pair it:\n")
			fmt.Fprintf(w, "  1. Sign in at %s and tap Pair device\n", relaypair.DefaultAppEndpoint)
			fmt.Fprintf(w, "  2. contenox pair <key>\n")
			return
		}
		fmt.Fprintf(w, "This machine's stored pairing cannot be read: %v\n", err)
		fmt.Fprintln(w, "Run 'contenox unpair' to clear it.")
		return
	}
	fmt.Fprintf(w, "Paired with %s.\n", creds.Endpoint)
	fmt.Fprintf(w, "Instance %s", creds.InstanceID)
	if creds.AccountID != "" {
		fmt.Fprintf(w, ", account %s", creds.AccountID)
	}
	fmt.Fprintln(w, ".")
	if origin, err := relaypair.AppOrigin(creds.Endpoint); err == nil {
		fmt.Fprintf(w, "App: %s\n", origin)
	}
	fmt.Fprintln(w, "'contenox unpair' removes this pairing.")
}
