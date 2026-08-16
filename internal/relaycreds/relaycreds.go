// Package relaycreds reads and writes the relay enrolment a machine obtained
// with `contenox login`: the instance token, the relay's public key, and the
// endpoint both apply to. Every function takes the .contenox directory
// explicitly rather than resolving one, and [Save] writes 0600 through a
// temporary file.
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

// Credentials are what `contenox login` obtained and what dialing a relay needs.
// A copied file is a copied identity, which is why it is written 0600.
type Credentials struct {
	// Endpoint is the relay this enrolment is for.
	Endpoint string `json:"endpoint"`
	// InstanceToken is the secret presented as a bearer credential on the
	// connection's upgrade request.
	InstanceToken string `json:"instance_token"`
	// InstanceID is this machine's identity at the relay, signed into the
	// relay's handshake signature.
	InstanceID string `json:"instance_id"`
	// AccountID owns the instance.
	AccountID string `json:"account_id"`
	// RelayPublicKey is the relay's long-lived Ed25519 key, base64. It is not
	// secret.
	RelayPublicKey string `json:"relay_public_key"`
}

// ErrNotEnrolled is returned by [Load] when no credential file exists. It is the
// ordinary state of a machine nobody has run `contenox login` on.
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

// Save writes c to contenoxDir, replacing whatever was there.
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
	// Chmod before the write: a world-readable window is long enough to copy a
	// token out of.
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

// Delete removes the enrolment. It is idempotent.
func Delete(contenoxDir string) error {
	if err := os.Remove(Path(contenoxDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("relaycreds: remove %s: %w", Path(contenoxDir), err)
	}
	return nil
}
