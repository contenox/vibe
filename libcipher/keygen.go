package libcipher

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

type (
	KeyGenerationError string
)

func (e KeyGenerationError) Error() string {
	return "libcipher/cipher: " + (string)(e)
}

// GenerateKey generates a cryptographically random key with the specified length.
func GenerateKey(keyLength int) (string, error) {
	if keyLength <= 0 {
		return "", KeyGenerationError("key length must be positive")
	}

	key := make([]byte, keyLength)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("%w:%w", KeyGenerationError("error generating key"), err)
	}
	encodedKey := hex.EncodeToString(key)
	return encodedKey, nil
}
