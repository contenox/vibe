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

// Relay authentication lives here because both ends must compute one byte
// string, and a signature format defined twice is defined differently. The
// relay signs with [SignWelcome] and the connector checks with
// [VerifyWelcome]; neither side reimplements [SigningInput].
//
// The direction this covers is relay→instance only. The instance proves itself
// with a bearer credential on the transport's upgrade request, which never
// enters a frame and therefore never enters this package.
//
// This is application-layer authentication and deliberately not TLS pinning:
// the relay's certificate is issued by an ACME CA and rotates every ~90 days,
// so a binary pinning the leaf would break itself in the field on machines
// nobody can reach. The signing key is long-lived and travels with the pairing.

// Nonce sizes. NonceSize is what a connector generates; MaxNonceBytes is what a
// relay will sign over, bounding the work an unauthenticated peer can ask for
// with a single hello.
const (
	NonceSize     = 32
	MaxNonceBytes = 64
)

// Relay-authentication failures. They are all "this peer did not prove it is
// the relay this instance paired with", which a connector treats as fatal
// rather than transient: a relay that cannot prove itself now will not start
// being able to on the next dial.
var (
	ErrNonceSize    = errors.New("librelay: nonce is missing or larger than MaxNonceBytes")
	ErrNoSignature  = errors.New("librelay: welcome carries no signature")
	ErrBadSignature = errors.New("librelay: welcome signature does not verify")
	ErrBadPublicKey = errors.New("librelay: not an ed25519 public key")
	ErrNulByte      = errors.New("librelay: signing input contains a NUL byte")
)

// NewNonce returns a fresh challenge for [Hello.Nonce]. It must be generated
// per connection and never reused: reuse is what would let a signature
// captured from one session be replayed into another.
func NewNonce() ([]byte, error) {
	n := make([]byte, NonceSize)
	if _, err := rand.Read(n); err != nil {
		return nil, fmt.Errorf("librelay: generate nonce: %w", err)
	}
	return n, nil
}

// SigningDomain is the domain-separation tag every [Welcome] signature starts
// with. It stops a signature being replayed into any other context that ever
// signs with the same key: a verifier for a different purpose would have to
// accept this tag to be fooled, and it never will.
const SigningDomain = "contenox-relay/welcome/v1"

// SigningInput is the exact byte string a [Welcome] signature covers. It is
// the single definition of that layout — both the signing and the verifying
// path call it, and a second copy of the concatenation anywhere is the bug
// this function exists to make impossible:
//
//	SigningDomain ‖ 0x00 ‖ base64url(nonce) ‖ 0x00 ‖ decimal(version) ‖ 0x00 ‖ instance
//
// Every part is UTF-8 bytes, the separator is one NUL, the version is ASCII
// digits with no padding, and the nonce is unpadded base64url — the same text
// it travels as in JSON, and text that cannot contain a NUL. Plain
// concatenation would not do: ("a", 11, "c") and ("a", 1, "1c") both flatten
// to "a11c", so one signature would cover two different triples. The
// separators are what make the parse unambiguous, which is why an input that
// could contain one is refused rather than signed over.
func SigningInput(nonce []byte, negotiatedVersion int, instance string) ([]byte, error) {
	if err := checkNonce(nonce); err != nil {
		return nil, err
	}
	if strings.IndexByte(instance, 0) >= 0 {
		// An instance id is an opaque printable identifier; one carrying
		// a NUL is either a bug or an attempt to move the separators,
		// and neither is worth signing.
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
// version is the one the relay selected, not the one the connector offered:
// the connector verifies against what it was told, so signing the offer would
// never verify.
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

// VerifyWelcome checks a relay's answer against the public key pinned at
// pairing time. A nil error is the only success; every other outcome is one of
// the sentinels above and none of them is retryable.
func VerifyWelcome(pub libcipher.SigningPublicKey, nonce []byte, negotiatedVersion int, instance string, sig []byte) error {
	if len(pub) != libcipher.SigningPublicKeySize {
		return ErrBadPublicKey
	}
	input, err := SigningInput(nonce, negotiatedVersion, instance)
	if err != nil {
		return err
	}
	if len(sig) == 0 {
		// Distinguished from a wrong signature on purpose: "the relay did
		// not sign at all" is an operator-visible misconfiguration, and
		// reporting it as a bad signature sends them hunting for a key
		// mismatch that does not exist.
		return ErrNoSignature
	}
	if !libcipher.Verify(pub, input, sig) {
		return ErrBadSignature
	}
	return nil
}

// checkNonce rejects a challenge that cannot bind a signature to a session.
func checkNonce(nonce []byte) error {
	if len(nonce) == 0 || len(nonce) > MaxNonceBytes {
		return fmt.Errorf("%w: %d bytes", ErrNonceSize, len(nonce))
	}
	return nil
}

// FormatPublicKey renders a relay's key for storage and for an enrolment
// payload. The encoding is [libcipher.SigningKeyEncoding]; this is a spelling
// of [libcipher.FormatPublicKey] kept so relay callers do not have to learn a
// second package for one call, not a second implementation.
func FormatPublicKey(pub libcipher.SigningPublicKey) string {
	return libcipher.FormatPublicKey(pub)
}

// ParsePublicKey reads a key produced by [FormatPublicKey], delegating to
// [libcipher.ParsePublicKey] for which spellings are accepted. The error is
// wrapped so it matches both [ErrBadPublicKey] — which relay callers already
// test for — and the libcipher sentinel underneath it.
func ParsePublicKey(s string) (libcipher.SigningPublicKey, error) {
	pub, err := libcipher.ParsePublicKey(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadPublicKey, err)
	}
	return pub, nil
}
