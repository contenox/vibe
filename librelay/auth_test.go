package librelay_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/contenox/contenox/librelay"
)

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pub, priv
}

// TestUnit_WelcomeSignatureRoundTrips is the contract both ends compile
// against: what a relay signs is what a connector checks.
func TestUnit_WelcomeSignatureRoundTrips(t *testing.T) {
	t.Parallel()
	pub, priv := testKey(t)
	nonce, err := librelay.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	sig, err := librelay.SignWelcome(priv, nonce, librelay.ProtocolVersion, "inst-a")
	if err != nil {
		t.Fatalf("SignWelcome: %v", err)
	}
	if err := librelay.VerifyWelcome(pub, nonce, librelay.ProtocolVersion, "inst-a", sig); err != nil {
		t.Fatalf("VerifyWelcome: %v", err)
	}
}

// TestUnit_WelcomeSignatureBindsEveryTerm is why all three terms are signed:
// change any one of them and the signature stops verifying, so a signature
// captured from one session cannot be replayed into another version, another
// instance, or another challenge.
func TestUnit_WelcomeSignatureBindsEveryTerm(t *testing.T) {
	t.Parallel()
	pub, priv := testKey(t)
	nonce, _ := librelay.NewNonce()
	sig, err := librelay.SignWelcome(priv, nonce, 1, "inst-a")
	if err != nil {
		t.Fatalf("SignWelcome: %v", err)
	}
	other, _ := librelay.NewNonce()
	otherPub, _ := testKey(t)

	tests := []struct {
		name     string
		pub      ed25519.PublicKey
		nonce    []byte
		version  int
		instance string
	}{
		{"another nonce", pub, other, 1, "inst-a"},
		{"another version", pub, nonce, 2, "inst-a"},
		{"another instance", pub, nonce, 1, "inst-b"},
		{"another relay", otherPub, nonce, 1, "inst-a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := librelay.VerifyWelcome(tc.pub, tc.nonce, tc.version, tc.instance, sig)
			if !errors.Is(err, librelay.ErrBadSignature) {
				t.Fatalf("VerifyWelcome = %v, want ErrBadSignature", err)
			}
		})
	}
}

// TestUnit_VerifyWelcomeDistinguishesAbsentFromWrong keeps "the relay signed
// nothing" a different report from "the relay signed wrongly": one is a
// misconfiguration and the other is a key mismatch, and conflating them sends
// an operator hunting for the wrong thing.
func TestUnit_VerifyWelcomeDistinguishesAbsentFromWrong(t *testing.T) {
	t.Parallel()
	pub, _ := testKey(t)
	nonce, _ := librelay.NewNonce()
	if err := librelay.VerifyWelcome(pub, nonce, 1, "inst-a", nil); !errors.Is(err, librelay.ErrNoSignature) {
		t.Fatalf("VerifyWelcome with no signature = %v, want ErrNoSignature", err)
	}
	if err := librelay.VerifyWelcome(nil, nonce, 1, "inst-a", []byte("x")); !errors.Is(err, librelay.ErrBadPublicKey) {
		t.Fatalf("VerifyWelcome with no key = %v, want ErrBadPublicKey", err)
	}
	if err := librelay.VerifyWelcome(pub, nil, 1, "inst-a", []byte("x")); !errors.Is(err, librelay.ErrNonceSize) {
		t.Fatalf("VerifyWelcome with no nonce = %v, want ErrNonceSize", err)
	}
	big := make([]byte, librelay.MaxNonceBytes+1)
	if err := librelay.VerifyWelcome(pub, big, 1, "inst-a", []byte("x")); !errors.Is(err, librelay.ErrNonceSize) {
		t.Fatalf("VerifyWelcome with an oversized nonce = %v, want ErrNonceSize", err)
	}
}

// TestUnit_SigningInputIsTheDocumentedBytes pins the exact byte string for a
// known triple, since two implementations agreeing on the algorithm and
// disagreeing on the input is the failure this package exists to prevent. A
// later edit that "tidies" the concatenation fails here, loudly, rather than in
// the field against a relay nobody can debug.
func TestUnit_SigningInputIsTheDocumentedBytes(t *testing.T) {
	t.Parallel()
	got, err := librelay.SigningInput([]byte{0xAA, 0xBB}, 1, "inst-a")
	if err != nil {
		t.Fatalf("SigningInput: %v", err)
	}
	// domain ‖ 0 ‖ base64url("\xAA\xBB") ‖ 0 ‖ "1" ‖ 0 ‖ instance
	var want []byte
	want = append(want, "contenox-relay/welcome/v1"...)
	want = append(want, 0)
	want = append(want, "qrs"...)
	want = append(want, 0)
	want = append(want, '1', 0)
	want = append(want, "inst-a"...)
	if !bytes.Equal(got, want) {
		t.Fatalf("SigningInput = %q, want %q", got, want)
	}
}

// TestUnit_SigningInputIsUnambiguous is why the separators exist: without them
// two different triples would flatten to one byte string and share a signature.
func TestUnit_SigningInputIsUnambiguous(t *testing.T) {
	t.Parallel()
	nonce := []byte{1, 2, 3}
	a, err := librelay.SigningInput(nonce, 11, "c")
	if err != nil {
		t.Fatalf("SigningInput: %v", err)
	}
	b, err := librelay.SigningInput(nonce, 1, "1c")
	if err != nil {
		t.Fatalf("SigningInput: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two different triples produced one signing input")
	}
	if !bytes.HasPrefix(a, []byte(librelay.SigningDomain)) {
		t.Fatalf("signing input = %q, want the domain tag first", a)
	}
}

// TestUnit_SigningInputRefusesANulInstance keeps the separators load-bearing:
// an identifier that could carry one is refused rather than signed over.
func TestUnit_SigningInputRefusesANulInstance(t *testing.T) {
	t.Parallel()
	nonce, _ := librelay.NewNonce()
	if _, err := librelay.SigningInput(nonce, 1, "inst\x00a"); !errors.Is(err, librelay.ErrNulByte) {
		t.Fatalf("SigningInput = %v, want ErrNulByte", err)
	}
	_, priv := testKey(t)
	if _, err := librelay.SignWelcome(priv, nonce, 1, "inst\x00a"); !errors.Is(err, librelay.ErrNulByte) {
		t.Fatalf("SignWelcome = %v, want ErrNulByte", err)
	}
	pub, _ := testKey(t)
	if err := librelay.VerifyWelcome(pub, nonce, 1, "inst\x00a", []byte("sig")); !errors.Is(err, librelay.ErrNulByte) {
		t.Fatalf("VerifyWelcome = %v, want ErrNulByte", err)
	}
}

// TestUnit_NonceIsFullSizeAndUnique guards the one property a challenge has.
func TestUnit_NonceIsFullSizeAndUnique(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 64 {
		n, err := librelay.NewNonce()
		if err != nil {
			t.Fatalf("NewNonce: %v", err)
		}
		if len(n) != librelay.NonceSize {
			t.Fatalf("nonce is %d bytes, want %d", len(n), librelay.NonceSize)
		}
		if seen[string(n)] {
			t.Fatal("NewNonce repeated a challenge")
		}
		seen[string(n)] = true
	}
}

// TestUnit_PublicKeyEncodings covers a key that a human moved between machines
// by hand. Every accepted spelling must still decode to one key; nothing about
// the signature check changes with the encoding.
func TestUnit_PublicKeyEncodings(t *testing.T) {
	t.Parallel()
	pub, _ := testKey(t)
	for _, s := range []string{
		librelay.FormatPublicKey(pub),
		base64.RawStdEncoding.EncodeToString(pub),
		base64.URLEncoding.EncodeToString(pub),
		base64.RawURLEncoding.EncodeToString(pub),
		hex.EncodeToString(pub),
	} {
		got, err := librelay.ParsePublicKey(s)
		if err != nil {
			t.Fatalf("ParsePublicKey(%q): %v", s, err)
		}
		if !got.Equal(pub) {
			t.Fatalf("ParsePublicKey(%q) produced a different key", s)
		}
	}
	for _, bad := range []string{"", "not-a-key", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := librelay.ParsePublicKey(bad); !errors.Is(err, librelay.ErrBadPublicKey) {
			t.Errorf("ParsePublicKey(%q) = %v, want ErrBadPublicKey", bad, err)
		}
	}
}

// TestUnit_HandshakeFieldsAreOnTheWire fixes the JSON names, which are the
// actual contract between two independently built implementations.
func TestUnit_HandshakeFieldsAreOnTheWire(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(librelay.Hello{ProtocolVersion: 1, Instance: "i", Nonce: []byte{1, 2, 3}})
	if err != nil {
		t.Fatalf("marshal hello: %v", err)
	}
	if want := `"nonce":"AQID"`; !bytes.Contains(b, []byte(want)) {
		t.Fatalf("hello = %s, want it to carry %s", b, want)
	}
	b, err = json.Marshal(librelay.Welcome{ProtocolVersion: 1, Signature: []byte{4, 5, 6}})
	if err != nil {
		t.Fatalf("marshal welcome: %v", err)
	}
	if want := `"sig":"BAUG"`; !bytes.Contains(b, []byte(want)) {
		t.Fatalf("welcome = %s, want it to carry %s", b, want)
	}
	// Omitted when absent, so an unauthenticated handshake is byte-for-byte
	// what it was before this field existed.
	b, _ = json.Marshal(librelay.Welcome{ProtocolVersion: 1})
	if bytes.Contains(b, []byte("sig")) {
		t.Fatalf("welcome = %s, want no sig field when unsigned", b)
	}
}
