//go:build !windows

package terminalservice

import (
	"fmt"
	"path/filepath"
)

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

func defaultTerminalShell() string {
	return "/bin/bash"
}

func validateShell(shell string) error {
	if !filepath.IsAbs(shell) {
		return fmt.Errorf("terminalservice: shell must be an absolute path")
	}
	if _, ok := defaultAllowedShells[shell]; !ok {
		return fmt.Errorf("terminalservice: shell %q is not allowed", shell)
	}
	return nil
}

func resolveShell(shell string) (string, error) {
	shell = filepath.Clean(shell)
	if err := validateShell(shell); err != nil {
		return "", err
	}
	return shell, nil
}
