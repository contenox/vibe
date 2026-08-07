package libcipher_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/contenox/contenox/libcipher"
)

func TestUnit_GenerateSigningKeyRoundTrip(t *testing.T) {
	pub, priv, err := libcipher.GenerateSigningKey()
	if err != nil {
		t.Fatalf("GenerateSigningKey: %v", err)
	}
	if len(pub) != libcipher.SigningPublicKeySize || len(priv) != libcipher.SigningPrivateKeySize {
		t.Fatalf("unexpected key sizes: pub=%d priv=%d", len(pub), len(priv))
	}

	sig, err := libcipher.Sign(priv, []byte("message"))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !libcipher.Verify(pub, []byte("message"), sig) {
		t.Fatal("Verify rejected a signature it produced")
	}
	if libcipher.Verify(pub, []byte("message!"), sig) {
		t.Fatal("Verify accepted a signature over different bytes")
	}

	seed, err := libcipher.FormatSigningSeed(priv)
	if err != nil {
		t.Fatalf("FormatSigningSeed: %v", err)
	}
	pub2, priv2, err := libcipher.ParseSigningSeed(seed)
	if err != nil {
		t.Fatalf("ParseSigningSeed: %v", err)
	}
	if !libcipher.EqualSigningKey(priv, priv2) {
		t.Fatal("seed round-trip produced a different private key")
	}
	if libcipher.FormatPublicKey(pub) != libcipher.FormatPublicKey(pub2) {
		t.Fatal("seed round-trip produced a different public key")
	}
}

// TestUnit_ParsePublicKeyAcceptedSpellings pins the leniency the doc promises:
// every encoding a human might have moved the key in must decode to one key.
func TestUnit_ParsePublicKeyAcceptedSpellings(t *testing.T) {
	pub, _, err := libcipher.GenerateSigningKey()
	if err != nil {
		t.Fatalf("GenerateSigningKey: %v", err)
	}
	want := libcipher.FormatPublicKey(pub)
	for _, s := range []string{
		want,
		base64.RawStdEncoding.EncodeToString(pub),
		base64.URLEncoding.EncodeToString(pub),
		base64.RawURLEncoding.EncodeToString(pub),
		hex.EncodeToString(pub),
	} {
		got, err := libcipher.ParsePublicKey(s)
		if err != nil {
			t.Fatalf("ParsePublicKey(%q): %v", s, err)
		}
		if libcipher.FormatPublicKey(got) != want {
			t.Fatalf("ParsePublicKey(%q) produced a different key", s)
		}
	}
}

func TestUnit_SigningKeyParsersRejectMalformed(t *testing.T) {
	long := strings.Repeat("A", 200)
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"not base64", "!!!!"},
		{"too short", base64.StdEncoding.EncodeToString([]byte("short"))},
		{"too long", base64.StdEncoding.EncodeToString(make([]byte, 64))},
		{"odd hex", "abc"},
		{"padding only", "===="},
		{"very long", long},
		{"whitespace", "  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := libcipher.ParsePublicKey(tc.in); !errors.Is(err, libcipher.ErrBadPublicKey) {
				t.Errorf("ParsePublicKey(%q) = %v, want ErrBadPublicKey", tc.in, err)
			}
			if _, _, err := libcipher.ParseSigningSeed(tc.in); !errors.Is(err, libcipher.ErrBadSeed) {
				t.Errorf("ParseSigningSeed(%q) = %v, want ErrBadSeed", tc.in, err)
			}
		})
	}
}

// TestUnit_SignVerifyRejectWrongLengths covers the promise that neither call
// panics on key material that came out of a config file.
func TestUnit_SignVerifyRejectWrongLengths(t *testing.T) {
	pub, priv, err := libcipher.GenerateSigningKey()
	if err != nil {
		t.Fatalf("GenerateSigningKey: %v", err)
	}
	sig, err := libcipher.Sign(priv, []byte("m"))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	if _, err := libcipher.Sign(nil, []byte("m")); !errors.Is(err, libcipher.ErrBadPrivateKey) {
		t.Errorf("Sign with nil key = %v, want ErrBadPrivateKey", err)
	}
	if _, err := libcipher.Sign(priv[:32], []byte("m")); !errors.Is(err, libcipher.ErrBadPrivateKey) {
		t.Errorf("Sign with a seed-length key = %v, want ErrBadPrivateKey", err)
	}
	if _, err := libcipher.FormatSigningSeed(priv[:8]); !errors.Is(err, libcipher.ErrBadPrivateKey) {
		t.Errorf("FormatSigningSeed with a short key = %v, want ErrBadPrivateKey", err)
	}
	if got := libcipher.FormatPublicKey(pub[:8]); got != "" {
		t.Errorf("FormatPublicKey of a short key = %q, want empty", got)
	}

	if libcipher.Verify(nil, []byte("m"), sig) {
		t.Error("Verify accepted a nil key")
	}
	if libcipher.Verify(pub[:8], []byte("m"), sig) {
		t.Error("Verify accepted a short key")
	}
	if libcipher.Verify(pub, []byte("m"), nil) {
		t.Error("Verify accepted an absent signature")
	}
	if libcipher.Verify(pub, []byte("m"), sig[:32]) {
		t.Error("Verify accepted a truncated signature")
	}
}

func TestUnit_EqualSigningKey(t *testing.T) {
	_, a, err := libcipher.GenerateSigningKey()
	if err != nil {
		t.Fatalf("GenerateSigningKey: %v", err)
	}
	_, b, err := libcipher.GenerateSigningKey()
	if err != nil {
		t.Fatalf("GenerateSigningKey: %v", err)
	}
	if !libcipher.EqualSigningKey(a, a) {
		t.Error("EqualSigningKey said a key differs from itself")
	}
	if libcipher.EqualSigningKey(a, b) {
		t.Error("EqualSigningKey said two distinct keys match")
	}
	if libcipher.EqualSigningKey(a, a[:16]) {
		t.Error("EqualSigningKey said a truncated key matches")
	}
}

// FuzzSigningKeyParsers asserts the parsers are total: any input either yields
// a key of exactly the documented length or an error, and never a panic.
func FuzzSigningKeyParsers(f *testing.F) {
	pub, priv, err := libcipher.GenerateSigningKey()
	if err != nil {
		f.Fatalf("GenerateSigningKey: %v", err)
	}
	seed, err := libcipher.FormatSigningSeed(priv)
	if err != nil {
		f.Fatalf("FormatSigningSeed: %v", err)
	}
	f.Add(libcipher.FormatPublicKey(pub))
	f.Add(seed)
	f.Add(hex.EncodeToString(pub))
	f.Add("")
	f.Add("=")
	f.Add("\x00\x00")

	f.Fuzz(func(t *testing.T, s string) {
		if got, err := libcipher.ParsePublicKey(s); err == nil && len(got) != libcipher.SigningPublicKeySize {
			t.Fatalf("ParsePublicKey(%q) returned %d bytes", s, len(got))
		}
		gotPub, gotPriv, err := libcipher.ParseSigningSeed(s)
		if err == nil {
			if len(gotPriv) != libcipher.SigningPrivateKeySize || len(gotPub) != libcipher.SigningPublicKeySize {
				t.Fatalf("ParseSigningSeed(%q) returned pub=%d priv=%d", s, len(gotPub), len(gotPriv))
			}
			// A parsed seed must be usable, not merely well-sized.
			sig, err := libcipher.Sign(gotPriv, []byte("fuzz"))
			if err != nil || !libcipher.Verify(gotPub, []byte("fuzz"), sig) {
				t.Fatalf("ParseSigningSeed(%q) produced an unusable keypair (err=%v)", s, err)
			}
		}
	})
}

// TestUnit_SigningKeyAliasesAreStdlibTypes keeps the aliases assignable from
// crypto/ed25519, which is what lets callers migrate without conversions.
func TestUnit_SigningKeyAliasesAreStdlibTypes(t *testing.T) {
	var pub libcipher.SigningPublicKey = ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))
	var priv libcipher.SigningPrivateKey = ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	if len(pub) != libcipher.SigningPublicKeySize || len(priv) != libcipher.SigningPrivateKeySize {
		t.Fatal("alias sizes disagree with crypto/ed25519")
	}
}
