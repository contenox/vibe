package libcipher_test

import (
	"crypto/sha256"
	"testing"

	"github.com/contenox/vibe/libcipher"
	"github.com/google/uuid"
)

func TestNewHash(t *testing.T) {
	data := []byte("hello world")
	key := []byte("secret-key")
	salt := []byte(uuid.NewString())
	hash, err := libcipher.NewHash(libcipher.GenerateHashArgs{
		Payload:    data,
		SigningKey: key,
		Salt:       salt,
	}, sha256.New)
	if err != nil {
		t.Fatalf("NewHash returned error: %v", err)
	}

	if len(hash) == 0 {
		t.Errorf("expected non-empty hash, got empty")
	}
}

func TestEqual_SameHash(t *testing.T) {
	data := []byte("test message")
	key := []byte("another-secret")
	salt := []byte(uuid.NewString())

	sealed, err := libcipher.NewHash(libcipher.GenerateHashArgs{
		Payload:    data,
		SigningKey: key,
		Salt:       salt,
	}, sha256.New)
	if err != nil {
		t.Fatalf("NewHash error: %v", err)
	}

	// Equal should return true when comparing the same sealed hash.
	if !libcipher.Equal(sealed, sealed) {
		t.Errorf("expected Equal to return true when comparing the same sealed hash")
	}
}

func TestEqual_DifferentHashes(t *testing.T) {
	data := []byte("test message")
	key := []byte("another-secret")
	salt := []byte(uuid.NewString())

	// Generate two sealed hashes from the same input.
	// Since a unique salt is added, they should differ.
	sealed1, err := libcipher.NewHash(libcipher.GenerateHashArgs{
		Payload:    data,
		SigningKey: key,
		Salt:       salt,
	}, sha256.New)
	if err != nil {
		t.Fatalf("NewHash error for first hash: %v", err)
	}
	sealed2, err := libcipher.NewHash(libcipher.GenerateHashArgs{
		Payload:    data,
		SigningKey: key,
		Salt:       salt,
	}, sha256.New)
	if err != nil {
		t.Fatalf("NewHash error for second hash: %v", err)
	}
	if libcipher.Equal(sealed1, sealed2[:2]) {
		t.Errorf("expected Equal to return false for two distinct sealed hashes")
	}
}

func TestEqual_ModifiedSalt(t *testing.T) {
	data := []byte("sensitive data")
	key := []byte("my-signing-key")
	salt := []byte(uuid.NewString())
	salt2 := []byte(uuid.NewString())

	// Generate a valid sealed hash.
	sealed, err := libcipher.NewHash(libcipher.GenerateHashArgs{
		Payload:    data,
		SigningKey: key,
		Salt:       salt,
	}, sha256.New)
	if err != nil {
		t.Fatalf("NewHash error: %v", err)
	}
	// Generate a valid sealed hash.
	modified, err := libcipher.NewHash(libcipher.GenerateHashArgs{
		Payload:    data,
		SigningKey: key,
		Salt:       salt2,
	}, sha256.New)
	if err != nil {
		t.Fatalf("NewHash error: %v", err)
	}

	// The modified sealed hash should not be equal to the original.
	if libcipher.Equal(sealed, modified) {
		t.Errorf("expected Equal to return false when the salt is modified")
	}
}
