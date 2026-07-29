package modeldinstall

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// parseSHA256 extracts the checksum from a `sha256sum`-format file: the first
// whitespace-delimited token on the first non-empty line, lowercased. GNU
// coreutils prefixes the hash with '\' when the filename contains special
// characters, so a single leading backslash is tolerated.
func parseSHA256(text string) (string, error) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		tok := strings.ToLower(strings.TrimPrefix(strings.Fields(line)[0], `\`))
		if len(tok) != 64 {
			return "", fmt.Errorf("modeld setup: malformed sha256 line %q", line)
		}
		if _, err := hex.DecodeString(tok); err != nil {
			return "", fmt.Errorf("modeld setup: malformed sha256 %q: %w", tok, err)
		}
		return tok, nil
	}
	return "", fmt.Errorf("modeld setup: empty sha256 file")
}

// sha256File computes the lowercase-hex SHA-256 of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyChecksum returns ErrChecksumMismatch (wrapped with the values) when the
// file's SHA-256 does not equal want.
func verifyChecksum(path, want string) error {
	got, err := sha256File(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%w: got %s want %s", ErrChecksumMismatch, got, want)
	}
	return nil
}
