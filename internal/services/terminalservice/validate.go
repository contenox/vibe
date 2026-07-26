package terminalservice

import (
	"fmt"
	"path/filepath"
	"strings"
)

func ValidateShell(shell string) error {
	return validateShell(filepath.Clean(shell))
}

// CwdUnderRoot ensures cwd is the same as allowedRoot or a subdirectory.
func CwdUnderRoot(allowedRoot, cwd string) error {
	_, err := ResolveCwdUnderRoot(allowedRoot, cwd)
	return err
}

// ResolveCwdUnderRoot returns a cleaned, real cwd after validating it is inside allowedRoot.
func ResolveCwdUnderRoot(allowedRoot, cwd string) (string, error) {
	root, err := filepath.Abs(allowedRoot)
	if err != nil {
		return "", fmt.Errorf("terminalservice: allowed root: %w", err)
	}
	root, err = filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("terminalservice: allowed root: %w", err)
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("terminalservice: cwd: %w", err)
	}
	absCwd, err = filepath.EvalSymlinks(filepath.Clean(absCwd))
	if err != nil {
		return "", fmt.Errorf("terminalservice: cwd: %w", err)
	}
	rel, err := filepath.Rel(root, absCwd)
	if err != nil {
		return "", fmt.Errorf("terminalservice: cwd not under allowed root")
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("terminalservice: cwd escapes allowed root")
	}
	return absCwd, nil
}
