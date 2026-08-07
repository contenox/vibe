package libcipher

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
)

// Configure & init the AES-CBC+HMAC cryptor in encryption mode.
// AES-CBC with PKCS7 padding HMAC for integrity.
//
//	The final encrypted string format:
//	[ MAC | AD-Length | AD | Initialization Vector | Block 1 | Block 2 | ... ]
//	uses rand from the arguments for introducing randomness.
//
// Don't use this for big messages, the whole cypher has to be in mem for computing the Hmac.
//
// The encryption key and integrity key must be distinct. Both keys have to be kept secret.
// Rotating must be done to both keys simultaneously.
//
// Compromised Encryption Key:
//
//	An attacker, possessing the encryption key, could decrypt sensitive data.
//	If you rotate only the integrity key, they still have access to the previously encrypted data.
//
// Compromised Integrity Key:
//
//	An attacker with the integrity key could potentially modify encrypted data,
//	forge HMACs, and tamper with the system without detection.
//	Even if you rotate the encryption key, the integrity of past data is compromised.
//
// the MAC is calculated from ( AD-Length | AD | Initialization Vector | Block 1 | Block 2 | ... )
//
// GCM Comparison:
//
//		Use CBC with HMAC over GCM (or any stream cipher) when avoiding nonce collisions can be challenging is a problem.
//		This is the case if you deal with:
//		- high-volume systems (the probability of nonce collisions increases, especially if the nonce space is limited).
//		- distributed environments (coordinating nonce generation across nodes and ensuring uniqueness becomes even more complex).
//		- or scenarios where encrypted data needs to be stored persistently. For example, if encrypted data is stored in a database.
//	      and later retrieved and re-encrypted, ensuring that a new, unique nonce is used each time can be challenging.
//
// Since this is a one-person project, ensure you review the code before using it to validate its security and correctness.
func NewCBCHMACEncryptor(encryptionKey []byte, integrityKey []byte, calculateMAC func() hash.Hash, rand io.Reader) (Encryptor, error) {
	cry, err := newCBCHMACryptor(encryptionKey, integrityKey, calculateMAC)
	if err != nil {
		return nil, err
	}
	cry.rand = rand

	return (encryptorCBCHMAC)(cry), nil
}

// Configure & init the AES-CBC+HMAC cryptor in decryption mode.
func NewCBCHMACDecryptor(encryptionKey []byte, integrityKey []byte, calculateMAC func() hash.Hash) (Decryptor, error) {
	cry, err := newCBCHMACryptor(encryptionKey, integrityKey, calculateMAC)
	if err != nil {
		return nil, err
	}
	return (decryptorCBCHMAC)(cry), nil
}

// Encryption mode of the cryptor.
type encryptorCBCHMAC cryptorCBCHMAC

func (crytor encryptorCBCHMAC) Crypt(message []byte, additionalData []byte) ([]byte, error) {
	if message == nil {
		return nil, MessageError("message was nil")
	}
	if len(additionalData) > maxAdditionalDataSize {
		return nil, MessageError("additional data too large")
	}
	// Apply PKCS#7 padding to the input data.
	pad := padPKCS7(len(message), crytor.pher.BlockSize())
	payload := make([]byte, len(message)+len(pad))
	// Prepare the message by concatenating the input and padding.
	copy(payload[:len(message)], message)
	copy(payload[len(message):], pad)
	// Generate a random initialization vector (IV).
	iv := make([]byte, crytor.pher.BlockSize())
	if _, err := io.ReadFull(crytor.rand, iv); err != nil {
		return nil, err
	}

	return crytor.seal(iv, payload, additionalData), nil
}

func (crytor encryptorCBCHMAC) seal(iv []byte, plaintext []byte, additionalData []byte) []byte {
	// Calculate the total size needed for HMAC, additionalData header, additionalData, IV, encrypted data.
	cypherLen := len(plaintext) + crytor.pher.BlockSize() + crytor.macLength + additionalDataHeaderLength + len(additionalData)
	// Construct slice to hold the encrypted text & Encrypt.
	cypherParcel := make([]byte, cypherLen)
	// Calculate AD length
	adLength := uint16(len(additionalData))
	// Encode AD length into bytes & copy it into the parcel.
	adHeaderLocation := crytor.macLength
	adLocation := adHeaderLocation + additionalDataHeaderLength
	binary.BigEndian.PutUint16(cypherParcel[adHeaderLocation:adLocation], adLength)
	ivLocation := adLocation + len(additionalData)
	copy(cypherParcel[adLocation:ivLocation], additionalData)
	// Store the IV after HMAC to the destination buffer.
	cipherTextLocation := ivLocation + len(iv)
	copy(cypherParcel[ivLocation:cipherTextLocation], iv)
	// Create a CBC decrypter and encrypt the message.
	mode := cipher.NewCBCEncrypter(crytor.pher, iv)
	mode.CryptBlocks(cypherParcel[cipherTextLocation:], plaintext)
	// Calculate the HMAC signature.
	hmac := generateSignature(crytor.integrityKey, crytor.calcMac, cypherParcel[adHeaderLocation:]...)
	// Store the HMAC at the beginning of destination buffer.
	copy(cypherParcel[:adHeaderLocation], hmac)

	return cypherParcel
}

// Decryption mode of the cryptor.
type decryptorCBCHMAC cryptorCBCHMAC

func (cryptor decryptorCBCHMAC) Crypt(ciphertext []byte) ([]byte, []byte, error) {
	if ciphertext == nil {
		return nil, nil, CipherTextError("cipherText was nil")
	}
	if len(ciphertext) < cryptor.macLength+cryptor.pher.BlockSize() {
		return nil, nil, CipherTextError("cipherText is invalid")
	}
	minCiphertextSize := cryptor.macLength + additionalDataHeaderLength + cryptor.pher.BlockSize()
	if len(ciphertext) < minCiphertextSize {
		return nil, nil, CipherTextError("cipherText is too short")
	}
	// Extract the HMAC from the beginning of the encrypted data.
	adHeaderLocation := cryptor.macLength
	mac := ciphertext[:adHeaderLocation]
	err := verify(mac, cryptor.integrityKey, cryptor.calcMac, generateSignature, ciphertext[adHeaderLocation:]...)
	if err != nil {
		return nil, nil, fmt.Errorf("data integrity compromised %w", err)
	}
	// Extract additionalData macLength.
	adLocation := adHeaderLocation + additionalDataHeaderLength
	adLengthHeader := ciphertext[adHeaderLocation:adLocation]
	adLength := binary.BigEndian.Uint16(adLengthHeader)
	// Extract the additional data.
	ivLocation := adLocation + int(adLength)
	additionalData := ciphertext[adLocation:ivLocation]
	// Extract iv.
	cipherTextLocation := ivLocation + cryptor.pher.BlockSize()
	iv := ciphertext[ivLocation:cipherTextLocation]
	payload := ciphertext[cipherTextLocation:]

	// Init slice to hold the plaintext & decrypt.
	dst := make([]byte, len(payload))
	// Create a CBC decrypter and decrypt the message.
	mode := cipher.NewCBCDecrypter(cryptor.pher, iv)
	mode.CryptBlocks(dst, payload)
	// Calculate the padding index.
	unpadIndex, err := unpadPKCS7(dst)
	if err != nil {
		return nil, nil, err
	}
	message := make([]byte, unpadIndex)
	// Remove padding & return.
	copy(message, dst[:unpadIndex])

	return message, additionalData, nil
}

type cryptorCBCHMAC struct {
	pher         cipher.Block
	macLength    int
	calcMac      func() hash.Hash
	integrityKey []byte
	rand         io.Reader
}

// Configure & init the AES-CBC+HMAC Cryptor in encryption mode.
func newCBCHMACryptor(encryptionKey []byte, integrityKey []byte, calculateMAC func() hash.Hash) (cryptorCBCHMAC, error) {
	const minKeySize = 16 // Replace with the desired minimum key size

	// Check key sizes.
	if len(encryptionKey) < minKeySize {
		return cryptorCBCHMAC{}, EncryptionKeyError("encryption key too short")
	}
	if len(integrityKey) < minKeySize {
		return cryptorCBCHMAC{}, EncryptionKeyError("integrity key too short")
	}

	// Check if the encryption key and integrity key are the same.
	if bytes.Equal(encryptionKey, integrityKey) {
		return cryptorCBCHMAC{}, InvalidUsageError("using same key for encryption and integrity is not allowed")
	}
	block, err := aes.NewCipher(encryptionKey)
	if err != nil {
		return cryptorCBCHMAC{}, err
	}

	newintegrityKey := make([]byte, len(integrityKey))
	copy(newintegrityKey, integrityKey)
	return cryptorCBCHMAC{
		pher:         block,
		macLength:    calculateMAC().Size(),
		integrityKey: newintegrityKey,
		calcMac:      calculateMAC,
	}, nil
}

// unpadPKCS7 returns the index of the PKCS7 padding start.
func unpadPKCS7(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, errors.New("empty input")
	}

	padding := int(data[len(data)-1])
	if padding > len(data) || padding == 0 {
		return 0, errors.New("invalid padding")
	}

	for i := len(data) - padding; i < len(data); i++ {
		if int(data[i]) != padding {
			return 0, errors.New("invalid padding")
		}
	}

	return len(data) - padding, nil
}

// padPKCS7 pads the data to a multiple of blockSize using PKCS7 padding.
func padPKCS7(dataLen int, blockSize int) []byte {
	padding := blockSize - (dataLen % blockSize)
	padText := bytes.Repeat([]byte{byte(padding)}, padding)

	return padText
}

// generateSignature generates HMAC for the message using the given token.
func generateSignature(token []byte, hashing func() hash.Hash, message ...byte) []byte {
	h := hmac.New(hashing, token)
	h.Write(message)

	return h.Sum(nil)
}

// verify verifies the integrity of the message using HMAC.
func verify(hmac, token []byte, hashing func() hash.Hash, should func(token []byte, hashing func() hash.Hash, message ...byte) []byte, message ...byte) error {
	// Constant-time comparison to mitigate timing attacks.
	if subtle.ConstantTimeCompare(should(token, hashing, message...), hmac) != 1 {
		return fmt.Errorf("signature verification failed")
	}

	return nil
}

const additionalDataHeaderLength = 2
const maxAdditionalDataSize = 65535
