package terminalservice

import (
	"fmt"
	"path/filepath"
	"strings"
)

// defaultAllowedShells restricts which executables may be used as an interactive shell.
var defaultAllowedShells = map[string]struct{}{
	"/bin/bash":     {},
	"/bin/sh":       {},
	"/usr/bin/bash": {},
	"/usr/bin/sh":   {},
	"/bin/zsh":      {},
	"/usr/bin/zsh":  {},
	"/bin/dash":     {},
	"/usr/bin/dash": {},
}

// ValidateShell returns an error if shell is not in the allowlist.
func ValidateShell(shell string) error {
	shell = filepath.Clean(shell)
	if !filepath.IsAbs(shell) {
		return fmt.Errorf("terminalservice: shell must be an absolute path")
	}
	if _, ok := defaultAllowedShells[shell]; !ok {
		return fmt.Errorf("terminalservice: shell %q is not allowed", shell)
	}
	return nil
}

// CwdUnderRoot ensures cwd is the same as allowedRoot or a subdirectory (after clean/abs).
func CwdUnderRoot(allowedRoot, cwd string) error {
	absRoot, err := filepath.Abs(allowedRoot)
	if err != nil {
		return fmt.Errorf("terminalservice: allowed root: %w", err)
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return fmt.Errorf("terminalservice: cwd: %w", err)
	}
	absRoot = filepath.Clean(absRoot)
	absCwd = filepath.Clean(absCwd)
	rel, err := filepath.Rel(absRoot, absCwd)
	if err != nil {
		return fmt.Errorf("terminalservice: cwd not under allowed root")
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("terminalservice: cwd escapes allowed root")
	}
	return nil
}
