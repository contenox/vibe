package libcipher_test

import (
	"crypto/sha256"
	"testing"

	"github.com/contenox/vibe/libcipher"
)

func TestCheckPasswordHash_Incorrect(t *testing.T) {
	hash, _ := libcipher.NewHash(libcipher.GenerateHashArgs{
		Payload:    []byte("password"),
		SigningKey: []byte("key"),
		Salt:       []byte("salt"),
	}, sha256.New)

	ok, err := libcipher.CheckHash("key", "salt", "wrongpass", hash)
	if err != nil {
		t.Fatalf("CheckPasswordHash failed: %v", err)
	}
	if ok {
		t.Error("Expected password not to match hash")
	}
}
