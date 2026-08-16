// Package libcipher provides cryptographic utilities for encryption, decryption,
// integrity verification, and key generation: AES-GCM, AES-CBC with HMAC, sealed
// HMAC hashes, and Ed25519 signing keys. The encryption key and integrity key
// must be kept secret and must be distinct.
package libcipher

import "crypto/sha256"

// CheckHash reports whether shouldBe hashes to hash under signingKey and salt.
func CheckHash(signingKey string, salt string, shouldBe string, hash []byte) (bool, error) {
	sealed, err := NewHash(GenerateHashArgs{
		Payload:    []byte(shouldBe),
		SigningKey: []byte(signingKey),
		Salt:       []byte(salt),
	}, sha256.New)
	if err != nil {
		return false, err
	}

	return Equal(sealed, hash), nil
}
