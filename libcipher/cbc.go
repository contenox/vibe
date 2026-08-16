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

// NewCBCHMACEncryptor returns an Encryptor using AES-CBC with PKCS7 padding and
// an HMAC for integrity. The encryption key and integrity key must be distinct,
// both kept secret, and rotated simultaneously.
//
//	[ MAC | AD-Length | AD | Initialization Vector | Block 1 | Block 2 | ... ]
func NewCBCHMACEncryptor(encryptionKey []byte, integrityKey []byte, calculateMAC func() hash.Hash, rand io.Reader) (Encryptor, error) {
	cry, err := newCBCHMACryptor(encryptionKey, integrityKey, calculateMAC)
	if err != nil {
		return nil, err
	}
	cry.rand = rand

	return (encryptorCBCHMAC)(cry), nil
}

// NewCBCHMACDecryptor returns a Decryptor for data sealed by [NewCBCHMACEncryptor].
func NewCBCHMACDecryptor(encryptionKey []byte, integrityKey []byte, calculateMAC func() hash.Hash) (Decryptor, error) {
	cry, err := newCBCHMACryptor(encryptionKey, integrityKey, calculateMAC)
	if err != nil {
		return nil, err
	}
	return (decryptorCBCHMAC)(cry), nil
}

type encryptorCBCHMAC cryptorCBCHMAC

func (crytor encryptorCBCHMAC) Crypt(message []byte, additionalData []byte) ([]byte, error) {
	if message == nil {
		return nil, MessageError("message was nil")
	}
	if len(additionalData) > maxAdditionalDataSize {
		return nil, MessageError("additional data too large")
	}
	pad := padPKCS7(len(message), crytor.pher.BlockSize())
	payload := make([]byte, len(message)+len(pad))
	copy(payload[:len(message)], message)
	copy(payload[len(message):], pad)
	iv := make([]byte, crytor.pher.BlockSize())
	if _, err := io.ReadFull(crytor.rand, iv); err != nil {
		return nil, err
	}

	return crytor.seal(iv, payload, additionalData), nil
}

func (crytor encryptorCBCHMAC) seal(iv []byte, plaintext []byte, additionalData []byte) []byte {
	cypherLen := len(plaintext) + crytor.pher.BlockSize() + crytor.macLength + additionalDataHeaderLength + len(additionalData)
	cypherParcel := make([]byte, cypherLen)
	adLength := uint16(len(additionalData))
	adHeaderLocation := crytor.macLength
	adLocation := adHeaderLocation + additionalDataHeaderLength
	binary.BigEndian.PutUint16(cypherParcel[adHeaderLocation:adLocation], adLength)
	ivLocation := adLocation + len(additionalData)
	copy(cypherParcel[adLocation:ivLocation], additionalData)
	cipherTextLocation := ivLocation + len(iv)
	copy(cypherParcel[ivLocation:cipherTextLocation], iv)
	mode := cipher.NewCBCEncrypter(crytor.pher, iv)
	mode.CryptBlocks(cypherParcel[cipherTextLocation:], plaintext)
	hmac := generateSignature(crytor.integrityKey, crytor.calcMac, cypherParcel[adHeaderLocation:]...)
	copy(cypherParcel[:adHeaderLocation], hmac)

	return cypherParcel
}

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
	adHeaderLocation := cryptor.macLength
	mac := ciphertext[:adHeaderLocation]
	err := verify(mac, cryptor.integrityKey, cryptor.calcMac, generateSignature, ciphertext[adHeaderLocation:]...)
	if err != nil {
		return nil, nil, fmt.Errorf("data integrity compromised %w", err)
	}
	adLocation := adHeaderLocation + additionalDataHeaderLength
	adLengthHeader := ciphertext[adHeaderLocation:adLocation]
	adLength := binary.BigEndian.Uint16(adLengthHeader)
	ivLocation := adLocation + int(adLength)
	additionalData := ciphertext[adLocation:ivLocation]
	cipherTextLocation := ivLocation + cryptor.pher.BlockSize()
	iv := ciphertext[ivLocation:cipherTextLocation]
	payload := ciphertext[cipherTextLocation:]

	dst := make([]byte, len(payload))
	mode := cipher.NewCBCDecrypter(cryptor.pher, iv)
	mode.CryptBlocks(dst, payload)
	unpadIndex, err := unpadPKCS7(dst)
	if err != nil {
		return nil, nil, err
	}
	message := make([]byte, unpadIndex)
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

func newCBCHMACryptor(encryptionKey []byte, integrityKey []byte, calculateMAC func() hash.Hash) (cryptorCBCHMAC, error) {
	const minKeySize = 16

	if len(encryptionKey) < minKeySize {
		return cryptorCBCHMAC{}, EncryptionKeyError("encryption key too short")
	}
	if len(integrityKey) < minKeySize {
		return cryptorCBCHMAC{}, EncryptionKeyError("integrity key too short")
	}

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

func generateSignature(token []byte, hashing func() hash.Hash, message ...byte) []byte {
	h := hmac.New(hashing, token)
	h.Write(message)

	return h.Sum(nil)
}

func verify(hmac, token []byte, hashing func() hash.Hash, should func(token []byte, hashing func() hash.Hash, message ...byte) []byte, message ...byte) error {
	if subtle.ConstantTimeCompare(should(token, hashing, message...), hmac) != 1 {
		return fmt.Errorf("signature verification failed")
	}

	return nil
}

const additionalDataHeaderLength = 2
const maxAdditionalDataSize = 65535
