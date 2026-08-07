package libcipher

// provides a method to crypt a message with additional data.
// Misuse of this method may lead to a panic.
type Encryptor interface {
	// Encrypts/Decrypts a message, misuse may lead to a panic.
	Crypt(message []byte, additionalData []byte) ([]byte, error)
}

// provides a method to crypt a cipher package.
// Misuse of this method may lead to a panic.
type Decryptor interface {
	// Encrypts/Decrypts a message, misuse may lead to a panic.
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
