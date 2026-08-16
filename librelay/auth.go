package librelay

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/contenox/contenox/libcipher"
)

// Nonce sizes. NonceSize is what a connector generates; MaxNonceBytes is what a
// relay will sign over.
const (
	NonceSize     = 32
	MaxNonceBytes = 64
)

// Relay-authentication failures; a connector treats all of them as fatal.
var (
	ErrNonceSize    = errors.New("librelay: nonce is missing or larger than MaxNonceBytes")
	ErrNoSignature  = errors.New("librelay: welcome carries no signature")
	ErrBadSignature = errors.New("librelay: welcome signature does not verify")
	ErrBadPublicKey = errors.New("librelay: not an ed25519 public key")
	ErrNulByte      = errors.New("librelay: signing input contains a NUL byte")
)

// NewNonce returns a fresh challenge for [Hello.Nonce]. It must be generated per
// connection and never reused.
func NewNonce() ([]byte, error) {
	n := make([]byte, NonceSize)
	if _, err := rand.Read(n); err != nil {
		return nil, fmt.Errorf("librelay: generate nonce: %w", err)
	}
	return n, nil
}

// SigningDomain is the domain-separation tag every [Welcome] signature starts with.
const SigningDomain = "contenox-relay/welcome/v1"

// SigningInput is the exact byte string a [Welcome] signature covers, and the
// single definition of that layout:
//
//	SigningDomain ‖ 0x00 ‖ base64url(nonce) ‖ 0x00 ‖ decimal(version) ‖ 0x00 ‖ instance
func SigningInput(nonce []byte, negotiatedVersion int, instance string) ([]byte, error) {
	if err := checkNonce(nonce); err != nil {
		return nil, err
	}
	if strings.IndexByte(instance, 0) >= 0 {
		return nil, fmt.Errorf("%w: instance", ErrNulByte)
	}
	encoded := base64.RawURLEncoding.EncodeToString(nonce)
	var buf []byte
	buf = append(buf, SigningDomain...)
	buf = append(buf, 0)
	buf = append(buf, encoded...)
	buf = append(buf, 0)
	buf = append(buf, strconv.Itoa(negotiatedVersion)...)
	buf = append(buf, 0)
	return append(buf, instance...), nil
}

// SignWelcome produces the [Welcome.Signature] a relay answers hello with. The
// version is the one the relay selected, not the one the connector offered.
func SignWelcome(priv libcipher.SigningPrivateKey, nonce []byte, negotiatedVersion int, instance string) ([]byte, error) {
	if len(priv) != libcipher.SigningPrivateKeySize {
		return nil, fmt.Errorf("librelay: sign welcome: %w", ErrBadPublicKey)
	}
	input, err := SigningInput(nonce, negotiatedVersion, instance)
	if err != nil {
		return nil, err
	}
	return libcipher.Sign(priv, input)
}

// VerifyWelcome checks a relay's answer against the public key pinned at pairing
// time. Every failure is one of the sentinels above and none is retryable.
func VerifyWelcome(pub libcipher.SigningPublicKey, nonce []byte, negotiatedVersion int, instance string, sig []byte) error {
	if len(pub) != libcipher.SigningPublicKeySize {
		return ErrBadPublicKey
	}
	input, err := SigningInput(nonce, negotiatedVersion, instance)
	if err != nil {
		return err
	}
	if len(sig) == 0 {
		return ErrNoSignature
	}
	if !libcipher.Verify(pub, input, sig) {
		return ErrBadSignature
	}
	return nil
}

func checkNonce(nonce []byte) error {
	if len(nonce) == 0 || len(nonce) > MaxNonceBytes {
		return fmt.Errorf("%w: %d bytes", ErrNonceSize, len(nonce))
	}
	return nil
}

// FormatPublicKey renders a relay's key for storage and for an enrolment
// payload, delegating to [libcipher.FormatPublicKey].
func FormatPublicKey(pub libcipher.SigningPublicKey) string {
	return libcipher.FormatPublicKey(pub)
}

// ParsePublicKey reads a key produced by [FormatPublicKey], delegating to
// [libcipher.ParsePublicKey]. The error matches both [ErrBadPublicKey] and the
// libcipher sentinel underneath it.
func ParsePublicKey(s string) (libcipher.SigningPublicKey, error) {
	pub, err := libcipher.ParsePublicKey(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadPublicKey, err)
	}
	return pub, nil
}
