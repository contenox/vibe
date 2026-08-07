// Package libcipher provides a collection of cryptographic utilities for encryption,
// decryption, integrity verification, and secure key generation. It includes implementations
// for AES-GCM (authenticated encryption) and AES-CBC combined with HMAC (for encryption with
// integrity verification), as well as functions for sealed HMAC hash creation and constant-time
// comparison.
//
// The package offers the following functionalities:
//   - AES-GCM based encryption/decryption, which provides both confidentiality and authenticity.
//   - AES-CBC with HMAC for scenarios where nonce collisions are a concern, especially in high-volume
//     or distributed environments. This mode encrypts data using AES-CBC (with PKCS#7 padding) and
//     ensures integrity via an HMAC over the encrypted payload and additional data.
//   - Sealed HMAC hash creation and comparison, where a unique salt is automatically added and the
//     resulting JSON-encoded object encapsulates both the computed HMAC digest and the salt.
//   - Cryptographically secure key generation.
//
// Security Considerations:
//   - The encryption key and integrity key must be kept secret and must be distinct. Reusing keys
//     for different purposes can compromise security.
//   - When using AES-CBC with HMAC, ensure that the entire ciphertext fits in memory as the HMAC is
//     computed over the complete message. For high-volume systems, AES-GCM may be preferred.
package libcipher

import "crypto/sha256"

// CheckHash verifies the shouldBe string against the hash.
// params:
// - signingKey (string) - the signing key for the hash
// - salt (string) - the salt used for the hash
// - password (string) - the password to verify
// - hash ([]byte) - the stored hash to compare against
// returns: (bool, error) - whether the password matches the hash and an error if any
//
// Note: Think twice, maybe bycrypt is what you need.
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
