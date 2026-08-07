package librelay

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
func SignWelcome(priv ed25519.PrivateKey, nonce []byte, negotiatedVersion int, instance string) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("librelay: sign welcome: %w", ErrBadPublicKey)
	}
	input, err := SigningInput(nonce, negotiatedVersion, instance)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(priv, input), nil
}

// VerifyWelcome checks a relay's answer against the public key pinned at
// pairing time. A nil error is the only success; every other outcome is one of
// the sentinels above and none of them is retryable.
func VerifyWelcome(pub ed25519.PublicKey, nonce []byte, negotiatedVersion int, instance string, sig []byte) error {
	if len(pub) != ed25519.PublicKeySize {
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
	if !ed25519.Verify(pub, input, sig) {
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
// payload: standard base64, which is what encoding/json already produces for a
// []byte field, so a relay that marshals its key and a tool that prints one
// agree without a second convention.
func FormatPublicKey(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

// ParsePublicKey reads a key produced by [FormatPublicKey]. It also accepts
// unpadded and URL-safe base64 and lowercase hex, because the key arrives as
// text a human may have moved between machines; the encoding it came in is not
// a security property, and every accepted form must still decode to exactly
// [ed25519.PublicKeySize] bytes. Widening what is *read* is safe here in a way
// that widening what is *accepted as valid* would not be — the signature check
// is unchanged by how the key was spelled.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	if s == "" {
		return nil, fmt.Errorf("%w: empty", ErrBadPublicKey)
	}
	if len(s) == ed25519.PublicKeySize*2 {
		if b, err := hex.DecodeString(s); err == nil {
			return ed25519.PublicKey(b), nil
		}
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		b, err := enc.DecodeString(s)
		if err != nil {
			continue
		}
		if len(b) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%w: %d bytes, want %d", ErrBadPublicKey, len(b), ed25519.PublicKeySize)
		}
		return ed25519.PublicKey(b), nil
	}
	return nil, fmt.Errorf("%w: not base64 or hex", ErrBadPublicKey)
}
