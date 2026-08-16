package libcipher

import (
	"crypto/hmac"
	"hash"
)

// GenerateHashArgs contains the input parameters for generating a sealed hash.
// SigningKey should be kept secret.
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

// NewHash computes an HMAC digest over the payload and salt in args using hashfn.
func NewHash(args GenerateHashArgs, hashfn func() hash.Hash) ([]byte, error) {
	macCompute := hmac.New(hashfn, args.SigningKey)

	_, err := macCompute.Write(args.Payload)
	if err != nil {
		return nil, HashError("failed to write hash data: " + err.Error())
	}

	_, err = macCompute.Write(args.Salt)
	if err != nil {
		return nil, HashError("failed to write salt data: " + err.Error())
	}

	return macCompute.Sum(nil), nil
}

// Equal reports whether two sealed hashes are byte-identical, in constant time.
// Comparing the JSON-encoded form rather than unmarshalling first is what keeps
// the comparison constant-time end to end.
func Equal(sealedHash1, sealedHash2 []byte) bool {
	// hmac.Equal, not bytes.Equal: this compares a value derived from a secret,
	// so it must not leak where two inputs first differ.
	return hmac.Equal(sealedHash1, sealedHash2)
}
