package libcipher

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"io"
)

// cryptorGCM implements the Encryptor and Decryptor interfaces using AES-GCM.
type cryptorGCM struct {
	gcm       cipher.AEAD
	rand      io.Reader
	blocksize func() int
}

// Encryption mode.
type encryptorGCM cryptorGCM

// Decryption mode.
type decryptorGCM cryptorGCM

// NewGCMEncryptor creates a new Encryptor using AES-GCM with the given key.
func NewGCMEncryptor(encryptionKey []byte, rand io.Reader) (Encryptor, error) {
	cryptor, err := newGCMCryptor(encryptionKey)
	if err != nil {
		return nil, err
	}
	cryptor.rand = rand

	return (encryptorGCM)(cryptor), nil
}

// NewGCMDecryptor creates a new Decryptor using AES-GCM with the given key.
func NewGCMDecryptor(encryptionKey []byte) (Decryptor, error) {
	cryptor, err := newGCMCryptor(encryptionKey)
	if err != nil {
		return nil, err
	}

	return (decryptorGCM)(cryptor), nil
}

func newGCMCryptor(encryptionKey []byte) (cryptorGCM, error) {
	const minKeySize = 16
	// Check key sizes.
	if len(encryptionKey) < minKeySize {
		return cryptorGCM{}, EncryptionKeyError("encryption key too short")
	}
	block, err := aes.NewCipher(encryptionKey)
	if err != nil {
		return cryptorGCM{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return cryptorGCM{}, err
	}
	return cryptorGCM{gcm: gcm, blocksize: block.BlockSize}, nil
}

// Crypt encrypts the given message using AES-GCM with the provided additional data.
func (e encryptorGCM) Crypt(message []byte, additionalData []byte) ([]byte, error) {
	if message == nil {
		return nil, MessageError("message was nil")
	}
	if uint64(len(message)) > uint64(((1<<32)-2)*e.blocksize()) {
		return nil, MessageError("message too large for GCM")
	}
	if len(additionalData) > maxAdditionalDataSize {
		return nil, MessageError("additional data too large")
	}

	nonce := make([]byte, e.gcm.NonceSize())
	if _, err := io.ReadFull(e.rand, nonce); err != nil {
		return nil, err
	}

	// Allocate space for the cipherpackage
	cipherpackage := make([]byte, len(nonce)+additionalDataHeaderLength+len(additionalData)+len(message)+e.gcm.Overhead())

	// Define locations
	nonceLocation := 0
	adHeaderHeaderLocation := nonceLocation + len(nonce)
	adHeaderLocation := adHeaderHeaderLocation + additionalDataHeaderLength
	dataLocation := adHeaderLocation + len(additionalData)

	// Copy nonce to the beginning of the cipherpackage
	copy(cipherpackage[nonceLocation:adHeaderHeaderLocation], nonce)

	// Copy additional data length and additional data into cipherpackage
	binary.BigEndian.PutUint16(cipherpackage[adHeaderHeaderLocation:adHeaderLocation], uint16(len(additionalData)))
	copy(cipherpackage[adHeaderLocation:dataLocation], additionalData)

	// Encrypt the message
	e.gcm.Seal(cipherpackage[dataLocation:dataLocation], nonce, message, additionalData)

	return cipherpackage, nil
}

// Crypt decrypts the given cipher package using AES-GCM.
func (d decryptorGCM) Crypt(cipherpackage []byte) ([]byte, []byte, error) {
	nonceSize := d.gcm.NonceSize()
	const additionalDataHeaderLength = 2

	if len(cipherpackage) < nonceSize+additionalDataHeaderLength {
		return nil, nil, errors.New("cipherpackage too short")
	}

	// Define locations
	nonceLocation := 0
	adHeaderHeaderLocation := nonceLocation + nonceSize
	adHeaderLocation := adHeaderHeaderLocation + additionalDataHeaderLength
	dataLocation := adHeaderLocation + int(binary.BigEndian.Uint16(cipherpackage[adHeaderHeaderLocation:adHeaderLocation]))

	if len(cipherpackage) < dataLocation {
		return nil, nil, errors.New("cipherpackage too short for additional data")
	}

	// Extract the nonce
	nonce := cipherpackage[nonceLocation:adHeaderHeaderLocation]

	// Extract the additional data length
	additionalDataLength := binary.BigEndian.Uint16(cipherpackage[adHeaderHeaderLocation:adHeaderLocation])

	// Extract the additional data
	additionalData := cipherpackage[adHeaderLocation : adHeaderLocation+int(additionalDataLength)]

	// Extract the ciphertext
	ciphertext := cipherpackage[adHeaderLocation+int(additionalDataLength):]

	// Decrypt the ciphertext
	plaintext, err := d.gcm.Open(nil, nonce, ciphertext, additionalData)
	if err != nil {
		return nil, nil, err
	}

	return plaintext, additionalData, nil
}
