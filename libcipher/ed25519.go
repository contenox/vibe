package libcipher

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Ed25519 signing keys live in this package so that every component that has to
// read, write, or check one agrees on a single set of rules. The alternative —
// each caller reaching for crypto/ed25519 and encoding/base64 on its own — is
// how two ends of the same protocol end up with two encodings, and a key that
// round-trips locally but fails every connection in the field.
//
// Sizes are re-exported so callers can validate lengths without importing
// crypto/ed25519 themselves.
const (
	SigningSeedSize       = ed25519.SeedSize       // 32, the private half's canonical storage form
	SigningPublicKeySize  = ed25519.PublicKeySize  // 32
	SigningPrivateKeySize = ed25519.PrivateKeySize // 64, seed ‖ public key
	SignatureSize         = ed25519.SignatureSize  // 64
)

// SigningPublicKey and SigningPrivateKey are aliases, not new types, so a value
// obtained from crypto/ed25519 elsewhere stays assignable here and callers are
// never forced to convert. The alias exists to spell the intent — these are
// signing keys, distinct from the symmetric material [GenerateKey] produces —
// not to wrap the standard library.
type (
	SigningPublicKey  = ed25519.PublicKey
	SigningPrivateKey = ed25519.PrivateKey
)

// SigningKeyError represents an error while generating, parsing, or using an
// Ed25519 key. Every value is a fixed sentinel wrapped with detail, so callers
// can match with errors.Is and still print something an operator can act on.
type SigningKeyError string

func (e SigningKeyError) Error() string {
	return "libcipher: " + (string)(e)
}

// Signing-key failures. They are all "this is not usable key material", which a
// caller treats as fatal: a malformed key does not become well-formed on retry.
const (
	ErrBadPublicKey  = SigningKeyError("not an ed25519 public key")
	ErrBadPrivateKey = SigningKeyError("not an ed25519 private key")
	ErrBadSeed       = SigningKeyError("not an ed25519 seed")
	ErrKeyGeneration = SigningKeyError("error generating signing key")
)

// SigningKeyEncoding is the one encoding this package writes.
//
// Standard base64 with padding, because that is what encoding/json already
// produces for a []byte field: a service that marshals its key into an
// enrolment payload and a tool that prints one agree without a second
// convention, and no call site has to guess. Note that this deliberately
// differs from [GenerateKey], which hex-encodes symmetric key material; hex
// doubles the length, and these keys travel in JSON documents and command
// lines where the shorter form is what people already see.
//
// Security Considerations:
//   - The encoding is not a security property. [ParsePublicKey] and
//     [ParseSigningSeed] therefore read the unpadded and URL-safe base64
//     variants and lowercase hex as well, since a key is text a human may have
//     moved between machines. Widening what is *read* is safe in a way that
//     widening what is *accepted as valid* is not: the signature check is
//     unchanged by how the key was spelled, and every accepted spelling must
//     still decode to exactly the right number of bytes.
var SigningKeyEncoding = base64.StdEncoding

// GenerateSigningKey returns a fresh Ed25519 keypair from the system CSPRNG.
//
// When to use:
// Use it when a component needs an identity to sign with — a relay proving
// itself to the instances paired with it, for example. The public half is meant
// to be published with [FormatPublicKey]; the private half is a secret and must
// be stored with [FormatSigningSeed] somewhere only that component can read.
//
// Security Considerations:
//   - The keypair is long-lived by intent, so where the seed is written matters
//     more than how it was generated. This package deliberately does not read
//     or write files or environment variables; that is a deployment concern.
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

// ParsePublicKey reads a key produced by [FormatPublicKey]. It also accepts the
// unpadded and URL-safe base64 variants and lowercase hex — see
// [SigningKeyEncoding] for why reading is lenient where writing is not. The
// result is always exactly [SigningPublicKeySize] bytes or an error wrapping
// [ErrBadPublicKey]; it never panics, whatever the input.
func ParsePublicKey(s string) (SigningPublicKey, error) {
	b, err := decodeKeyText(s, SigningPublicKeySize, ErrBadPublicKey)
	if err != nil {
		return nil, err
	}
	return SigningPublicKey(b), nil
}

// FormatSigningSeed renders the private half of a keypair as its 32-byte seed
// in [SigningKeyEncoding]. The seed is the storage form: the full 64-byte
// private key is the seed with the public key appended, so persisting the seed
// loses nothing and halves what has to be kept secret.
//
// Security Considerations:
//   - The returned string is secret material. Do not log it, and prefer passing
//     it through a channel that does not end up in shell history or a crash
//     dump.
func FormatSigningSeed(priv SigningPrivateKey) (string, error) {
	if len(priv) != SigningPrivateKeySize {
		return "", fmt.Errorf("%w: %d bytes, want %d", ErrBadPrivateKey, len(priv), SigningPrivateKeySize)
	}
	return SigningKeyEncoding.EncodeToString(priv.Seed()), nil
}

// ParseSigningSeed reconstructs a keypair from a seed produced by
// [FormatSigningSeed], accepting the same spellings [ParsePublicKey] does.
//
// When to use:
// Use it at start-up, on whatever a deployment hands the process as its signing
// identity. It is the counterpart of [FormatSigningSeed] and the only supported
// way to turn stored text back into a key — parsing a seed at the call site is
// exactly the duplication this package exists to remove.
//
// The seed determines the public key, so a caller that also received a public
// key out of band should compare the two and refuse to start on a mismatch:
// signing with a key nobody pinned produces signatures nobody can verify.
func ParseSigningSeed(s string) (SigningPublicKey, SigningPrivateKey, error) {
	seed, err := decodeKeyText(s, SigningSeedSize, ErrBadSeed)
	if err != nil {
		return nil, nil, err
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv.Public().(SigningPublicKey), priv, nil
}

// Sign returns a detached signature over message. The message is signed as it
// is given: any domain separation or framing is the caller's protocol and
// belongs in the caller, not here.
//
// It returns an error wrapping [ErrBadPrivateKey] rather than panicking on a
// key of the wrong length, which is what crypto/ed25519 would do — a key
// usually arrives from configuration, and bad configuration should surface as
// an error an operator can read.
func Sign(priv SigningPrivateKey, message []byte) ([]byte, error) {
	if len(priv) != SigningPrivateKeySize {
		return nil, fmt.Errorf("%w: %d bytes, want %d", ErrBadPrivateKey, len(priv), SigningPrivateKeySize)
	}
	return ed25519.Sign(priv, message), nil
}

// Verify reports whether sig is a valid signature of message by pub. It is
// total: a key, message, or signature of any length or contents yields false
// rather than a panic, because everything it is handed came off a wire or out
// of a config file and none of it is trusted.
//
// Security Considerations:
//   - A false result must be treated as an authentication failure, never as a
//     transient error to retry past.
func Verify(pub SigningPublicKey, message, sig []byte) bool {
	if len(pub) != SigningPublicKeySize || len(sig) != SignatureSize {
		return false
	}
	return ed25519.Verify(pub, message, sig)
}

// EqualSigningKey compares two private keys in constant time.
//
// Usage:
//
//	if !EqualSigningKey(configured, loaded) {
//	    // the process was handed two different identities
//	}
//
// hmac.Equal, not bytes.Equal, for the same reason [Equal] uses it: the inputs
// are secret, so the comparison must not leak where they first differ. Keys of
// differing length simply compare unequal.
func EqualSigningKey(a, b SigningPrivateKey) bool {
	return hmac.Equal(a, b)
}

// decodeKeyText decodes key text of a known byte length, trying lowercase hex
// first and then every base64 alphabet. It is the single reader behind
// [ParsePublicKey] and [ParseSigningSeed] so the two cannot drift apart.
//
// The invariant callers rely on: either the returned slice is exactly want
// bytes long, or there is an error wrapping badKey. Nothing here panics or
// indexes into the input.
func decodeKeyText(s string, want int, badKey SigningKeyError) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("%w: empty", badKey)
	}
	if len(s) == want*2 {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	// gotLen records a decode that succeeded at the wrong length, so the
	// error can say "32 bytes, want 64" instead of "not base64", which is
	// the difference between an operator finding a truncated key and an
	// operator suspecting the encoding.
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
