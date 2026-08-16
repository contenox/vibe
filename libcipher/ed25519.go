package libcipher

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Ed25519 key and signature sizes, re-exported so callers can validate lengths
// without importing crypto/ed25519.
const (
	SigningSeedSize       = ed25519.SeedSize
	SigningPublicKeySize  = ed25519.PublicKeySize
	SigningPrivateKeySize = ed25519.PrivateKeySize
	SignatureSize         = ed25519.SignatureSize
)

// SigningPublicKey and SigningPrivateKey are aliases for the crypto/ed25519 key
// types.
type (
	SigningPublicKey  = ed25519.PublicKey
	SigningPrivateKey = ed25519.PrivateKey
)

// SigningKeyError represents an error while generating, parsing, or using an
// Ed25519 key.
type SigningKeyError string

func (e SigningKeyError) Error() string {
	return "libcipher: " + (string)(e)
}

// Signing-key failures.
const (
	ErrBadPublicKey  = SigningKeyError("not an ed25519 public key")
	ErrBadPrivateKey = SigningKeyError("not an ed25519 private key")
	ErrBadSeed       = SigningKeyError("not an ed25519 seed")
	ErrKeyGeneration = SigningKeyError("error generating signing key")
)

// SigningKeyEncoding is the one encoding this package writes. Parsing is
// deliberately more lenient; see [ParsePublicKey].
var SigningKeyEncoding = base64.StdEncoding

// GenerateSigningKey returns a fresh Ed25519 keypair from the system CSPRNG.
// Publish the public half with [FormatPublicKey] and store the private half
// with [FormatSigningSeed].
func GenerateSigningKey() (SigningPublicKey, SigningPrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("%w:%w", ErrKeyGeneration, err)
	}
	return pub, priv, nil
}

// FormatPublicKey renders a public key for storage or for handing to a peer, in
// [SigningKeyEncoding]. It returns the empty string for a key of the wrong
// length rather than emitting text that would never parse back.
func FormatPublicKey(pub SigningPublicKey) string {
	if len(pub) != SigningPublicKeySize {
		return ""
	}
	return SigningKeyEncoding.EncodeToString(pub)
}

// ParsePublicKey reads a key produced by [FormatPublicKey], also accepting the
// unpadded and URL-safe base64 variants and lowercase hex. The result is
// exactly [SigningPublicKeySize] bytes or an error wrapping [ErrBadPublicKey].
func ParsePublicKey(s string) (SigningPublicKey, error) {
	b, err := decodeKeyText(s, SigningPublicKeySize, ErrBadPublicKey)
	if err != nil {
		return nil, err
	}
	return SigningPublicKey(b), nil
}

// FormatSigningSeed renders the private half of a keypair as its 32-byte seed in
// [SigningKeyEncoding]. The returned string is secret material.
func FormatSigningSeed(priv SigningPrivateKey) (string, error) {
	if len(priv) != SigningPrivateKeySize {
		return "", fmt.Errorf("%w: %d bytes, want %d", ErrBadPrivateKey, len(priv), SigningPrivateKeySize)
	}
	return SigningKeyEncoding.EncodeToString(priv.Seed()), nil
}

// ParseSigningSeed reconstructs a keypair from a seed produced by
// [FormatSigningSeed], accepting the same spellings [ParsePublicKey] does.
func ParseSigningSeed(s string) (SigningPublicKey, SigningPrivateKey, error) {
	seed, err := decodeKeyText(s, SigningSeedSize, ErrBadSeed)
	if err != nil {
		return nil, nil, err
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv.Public().(SigningPublicKey), priv, nil
}

// Sign returns a detached signature over message as given; domain separation
// and framing are the caller's. A key of the wrong length yields an error
// wrapping [ErrBadPrivateKey] rather than a panic.
func Sign(priv SigningPrivateKey, message []byte) ([]byte, error) {
	if len(priv) != SigningPrivateKeySize {
		return nil, fmt.Errorf("%w: %d bytes, want %d", ErrBadPrivateKey, len(priv), SigningPrivateKeySize)
	}
	return ed25519.Sign(priv, message), nil
}

// Verify reports whether sig is a valid signature of message by pub. It never
// panics: malformed input yields false, which is an authentication failure and
// not a transient error.
func Verify(pub SigningPublicKey, message, sig []byte) bool {
	if len(pub) != SigningPublicKeySize || len(sig) != SignatureSize {
		return false
	}
	return ed25519.Verify(pub, message, sig)
}

// EqualSigningKey compares two private keys in constant time. Keys of differing
// length compare unequal.
func EqualSigningKey(a, b SigningPrivateKey) bool {
	return hmac.Equal(a, b)
}

// decodeKeyText decodes key text of a known byte length, trying lowercase hex
// first and then every base64 alphabet. It returns exactly want bytes or an
// error wrapping badKey.
func decodeKeyText(s string, want int, badKey SigningKeyError) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("%w: empty", badKey)
	}
	if len(s) == want*2 {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	// Records a decode that succeeded at the wrong length, for a better error.
	gotLen := -1
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		b, err := enc.DecodeString(s)
		if err != nil {
			continue
		}
		if len(b) != want {
			gotLen = len(b)
			continue
		}
		return b, nil
	}
	if gotLen >= 0 {
		return nil, fmt.Errorf("%w: %d bytes, want %d", badKey, gotLen, want)
	}
	return nil, fmt.Errorf("%w: not base64 or hex", badKey)
}
