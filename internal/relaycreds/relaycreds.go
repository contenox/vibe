// Package relaycreds reads and writes the relay enrolment a machine obtained
// with `contenox login`: the instance token it authenticates with, the relay's
// public key it authenticates the relay with, and the endpoint both apply to.
//
// It is a file and nothing more — no network, no polling, no relay. The
// device-code exchange that produces a [Credentials] lives with the command
// that runs it; this package exists so that whatever later dials the relay can
// read the result without importing a CLI.
//
// # The directory is the caller's decision
//
// Every function takes the .contenox directory explicitly rather than
// resolving one. This package is used from a command that has a --data-dir
// flag and from runtime code that has none, and a package that guessed would
// make those two disagree — a credential written to one directory and looked
// for in another is indistinguishable from never having logged in.
//
// # The token is a secret
//
// [Save] writes 0600 and creates directories 0700, and rewrites through a
// temporary file so a crash cannot leave a half-written credential that parses.
// The permissions are advisory on Windows, which is why the file is written
// into the control-plane directory rather than anywhere a workspace tool can
// reach: the runtime's fs tools already refuse that directory.
package relaycreds

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Filename is where the enrolment lives inside a .contenox directory.
const Filename = "relay.json"

// Credentials are what `contenox login` obtained and what dialing a relay
// needs. Every field comes from the enrolment exchange; none of it is derived
// on this machine, so a copied file is a copied identity and that is why the
// file is 0600.
type Credentials struct {
	// Endpoint is the relay this enrolment is for. It is stored with the
	// credentials because a token is meaningless against a different relay,
	// and keeping them together makes that impossible to get wrong.
	Endpoint string `json:"endpoint"`
	// InstanceToken is the secret presented as a bearer credential on the
	// connection's upgrade request.
	InstanceToken string `json:"instance_token"`
	// InstanceID is this machine's identity at the relay. It is signed into
	// the relay's handshake signature, so it is part of the verification
	// input and not merely a label.
	InstanceID string `json:"instance_id"`
	// AccountID owns the instance. An instance belongs to the account, never
	// to the user who approved the pairing; it is recorded here so an
	// operator can see which account a machine is attached to without
	// asking the relay.
	AccountID string `json:"account_id"`
	// RelayPublicKey is the relay's long-lived Ed25519 key, base64. It is
	// the whole of the relay's identity: the connector refuses any peer that
	// cannot sign with it. It is not secret.
	RelayPublicKey string `json:"relay_public_key"`
}

// ErrNotEnrolled is returned by [Load] when no credential file exists. It is
// the ordinary state of a machine nobody has run `contenox login` on, not a
// failure, and callers are expected to carry on without a relay when they see
// it.
var ErrNotEnrolled = errors.New("relaycreds: this machine is not enrolled with a relay")

// Path is where credentials live inside contenoxDir.
func Path(contenoxDir string) string { return filepath.Join(contenoxDir, Filename) }

// Load reads the enrolment from contenoxDir, answering [ErrNotEnrolled] when
// there is none.
func Load(contenoxDir string) (Credentials, error) {
	var c Credentials
	b, err := os.ReadFile(Path(contenoxDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, ErrNotEnrolled
		}
		return c, fmt.Errorf("relaycreds: read %s: %w", Path(contenoxDir), err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return Credentials{}, fmt.Errorf("relaycreds: parse %s: %w", Path(contenoxDir), err)
	}
	return c, nil
}

// Save writes c to contenoxDir, replacing whatever was there. Enrolling twice
// replaces the enrolment rather than accumulating one, because a machine has
// one identity at a relay and a second file would just be a stale one.
func Save(contenoxDir string, c Credentials) error {
	if err := os.MkdirAll(contenoxDir, 0o700); err != nil {
		return fmt.Errorf("relaycreds: create %s: %w", contenoxDir, err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("relaycreds: encode: %w", err)
	}
	b = append(b, '\n')
	final := Path(contenoxDir)
	tmp, err := os.CreateTemp(contenoxDir, Filename+".tmp*")
	if err != nil {
		return fmt.Errorf("relaycreds: create temp in %s: %w", contenoxDir, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	// Chmod before the write, not after: the window between a world-readable
	// create and a later tightening is exactly long enough to copy a token
	// out of.
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("relaycreds: set permissions on %s: %w", name, err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("relaycreds: write %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("relaycreds: close %s: %w", name, err)
	}
	// Windows will not rename over an existing file.
	_ = os.Remove(final)
	if err := os.Rename(name, final); err != nil {
		return fmt.Errorf("relaycreds: install %s: %w", final, err)
	}
	return nil
}

// Delete removes the enrolment. It is idempotent: logging out of a machine
// that was never logged in is not an error, because the state the caller asked
// for is the state it ends in.
func Delete(contenoxDir string) error {
	if err := os.Remove(Path(contenoxDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("relaycreds: remove %s: %w", Path(contenoxDir), err)
	}
	return nil
}
