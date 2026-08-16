package libcipher

// Encryptor crypts a message with additional data. Misuse may lead to a panic.
type Encryptor interface {
	Crypt(message []byte, additionalData []byte) ([]byte, error)
}

// Decryptor crypts a cipher package. Misuse may lead to a panic.
type Decryptor interface {
	Crypt(cipherpackage []byte) ([]byte, []byte, error)
}

type (
	MessageError       string
	CipherTextError    string
	EncryptionKeyError string
	IntegrityKeyError  string
	InvalidUsageError  string
)

func (e MessageError) Error() string {
	return "libcipher: " + (string)(e)
}
func (e CipherTextError) Error() string {
	return "libcipher: " + (string)(e)
}
func (e EncryptionKeyError) Error() string {
	return "libcipher: " + (string)(e)
}
func (e IntegrityKeyError) Error() string {
	return "libcipher: " + (string)(e)
}
func (e InvalidUsageError) Error() string {
	return "libcipher: " + (string)(e)
}
