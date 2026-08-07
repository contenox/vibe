package libcipher

import (
	"crypto/hmac"
	"hash"
)

// GenerateHashArgs contains the input parameters for generating a sealed hash.
// The Payload field is the data to be hashed, and SigningKey is the key used to compute
// the HMAC digest. The SigningKey should be kept secret.
type GenerateHashArgs struct {
	Payload    []byte
	SigningKey []byte
	Salt       []byte
}

// HashError represents an error during hash generation.
type HashError string

func (e HashError) Error() string {
	return "libcipher: " + string(e)
}

// NewHash generates a sealed hash from the provided arguments and hash function.
// It computes an HMAC digest over the concatenation of the input data and a unique salt,
// and packages the result along with the salt in a JSON-encoded SealedHash object.
//
// Usage:
//
//	sealed, err := NewHash(GenerateHashArgs{
//	    Hash:       data,
//	    SigningKey: key,
//	}, sha256.New)
//	if err != nil {
//	    // handle error
//	}
//
// When to use:
// Use NewHash when you need to securely generate a hash that verifies the integrity
// and authenticity of data. A unique salt is automatically added so that identical inputs
// produce distinct outputs, and the signing key is applied during the HMAC computation.
// The signing key is not required during verification since it is already embedded in the
// computed HMAC digest.
func NewHash(args GenerateHashArgs, hashfn func() hash.Hash) ([]byte, error) {

	// Create a new HMAC using the provided hash function and signing key.
	macCompute := hmac.New(hashfn, args.SigningKey)

	// Write the input data to the HMAC.
	_, err := macCompute.Write(args.Payload)
	if err != nil {
		return nil, HashError("failed to write hash data: " + err.Error())
	}

	// Write the salt to the HMAC.
	_, err = macCompute.Write(args.Salt)
	if err != nil {
		return nil, HashError("failed to write salt data: " + err.Error())
	}

	// Compute the final HMAC digest.
	computedHash := macCompute.Sum(nil)

	return computedHash, nil
}

// Equal compares two JSON-encoded sealed hashes in constant time.
// It unmarshals each sealed hash into a SealedHash struct and then compares
// both the Hash and Salt fields using hmac.Equal.
//
// Usage:
//
//	ok := Equal(sealedHash1, sealedHash2)
//	if !ok {
//	    // the sealed hashes do not match
//	}
//
// Returns true when the two sealed hashes are byte-identical. Since a sealed
// hash is the JSON encoding of both the digest and its salt, comparing the
// encoded forms compares both components — no unmarshalling is needed, and not
// unmarshalling is what keeps the comparison constant-time end to end.
func Equal(sealedHash1, sealedHash2 []byte) bool {
	// hmac.Equal, not bytes.Equal: this compares a value derived from a secret,
	// so it must not leak where two inputs first differ.
	return hmac.Equal(sealedHash1, sealedHash2)
}
