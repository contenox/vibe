package acpsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/contenox/contenox/internal/relaycreds"
	"github.com/contenox/contenox/internal/relaypair"
)

// handlePair attaches this machine to a relay with a key minted in the app.
// No argument reports the stored pairing; the instance token is never printed.
//
// A session command rather than a CLI verb: the connection is held by this
// process, so pairing elsewhere would store a credential with nothing running
// to use it.
func (t *Transport) handlePair(ctx context.Context, args string) (string, error) {
	dir := t.deps.ContenoxDir
	if dir == "" {
		return "", fmt.Errorf("this session has no .contenox directory, so there is nowhere to store a pairing")
	}

	fields := strings.Fields(args)
	if len(fields) == 0 {
		return pairingStatus(dir), nil
	}

	key := fields[0]
	explicit := ""
	switch len(fields) {
	case 1:
	case 2:
		explicit = fields[1]
	default:
		return "", fmt.Errorf("usage: /pair <key> [relay-endpoint]")
	}
	endpoint := relaypair.Endpoint(explicit)

	// The hostname, never a typed name: a pairing identifies this machine.
	// Collisions are uniquified by the relay.
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "", fmt.Errorf("cannot determine this machine's hostname, so it has no name to pair under")
	}

	creds, err := relaypair.Redeem(ctx, nil, endpoint, key, name)
	if err != nil {
		switch {
		case errors.Is(err, relaypair.ErrKeyRejected):
			// The relay's message as given; it does not resolve unknown
			// from expired from spent, so neither does this.
			return "", fmt.Errorf("%v — mint a new key in the app and try again", err)
		case errors.Is(err, relaypair.ErrRelayUnusable):
			return "", err
		default:
			return "", fmt.Errorf("could not reach the relay: %w", err)
		}
	}
	if err := relaycreds.Save(dir, creds); err != nil {
		return "", fmt.Errorf("paired, but the credentials could not be stored: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Paired as %q (instance %s).\n", name, creds.InstanceID)
	if creds.AccountID != "" {
		fmt.Fprintf(&b, "Attached to account %s.\n", creds.AccountID)
	}
	fmt.Fprintf(&b, "Relay: %s\n", creds.Endpoint)
	fmt.Fprintf(&b, "Stored in %s — /unpair removes it.", relaycreds.Path(dir))
	return b.String(), nil
}

// handleUnpair deletes this machine's stored pairing. Local only: it does not
// revoke, and a revoked instance is refused at its next dial regardless.
func (t *Transport) handleUnpair(_ context.Context) (string, error) {
	dir := t.deps.ContenoxDir
	if dir == "" {
		return "", fmt.Errorf("this session has no .contenox directory, so there is no pairing to remove")
	}
	if _, err := relaycreds.Load(dir); err != nil {
		if errors.Is(err, relaycreds.ErrNotEnrolled) {
			return "This machine is not paired with a relay — nothing to do.", nil
		}
		// An unparseable file is still the file this command deletes.
	}
	if err := relaycreds.Delete(dir); err != nil {
		return "", err
	}
	return fmt.Sprintf("Removed %s.\nThis machine will no longer dial the relay. "+
		"Revoking the instance is done in the app.", relaycreds.Path(dir)), nil
}

// pairingStatus renders the stored pairing, omitting the credential.
func pairingStatus(dir string) string {
	creds, err := relaycreds.Load(dir)
	if err != nil {
		if errors.Is(err, relaycreds.ErrNotEnrolled) {
			return "This machine is not paired with a relay.\n" +
				"Sign in on the contenox app, tap Pair device, then run /pair <key> here."
		}
		return fmt.Sprintf("This machine's stored pairing cannot be read: %v\nRun /unpair to clear it.", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Paired with %s.\n", creds.Endpoint)
	fmt.Fprintf(&b, "Instance %s", creds.InstanceID)
	if creds.AccountID != "" {
		fmt.Fprintf(&b, ", account %s", creds.AccountID)
	}
	b.WriteString(".\n/unpair removes this pairing.")
	return b.String()
}
