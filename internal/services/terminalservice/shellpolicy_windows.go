//go:build windows

package terminalservice

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/contenox/contenox/internal/services/localtools"
)

func defaultTerminalShell() string {
	return localtools.DetectPlatformShell().Command
}

func validateShell(shell string) error {
	if !isAllowedWindowsShell(shell) {
		return fmt.Errorf("terminalservice: shell %q is not allowed", shell)
	}
	return nil
}

func resolveShell(shell string) (string, error) {
	shell = filepath.Clean(shell)
	if err := validateShell(shell); err != nil {
		return "", err
	}
	if filepath.IsAbs(shell) {
		return shell, nil
	}
	path, err := exec.LookPath(shell)
	if err != nil {
		return "", fmt.Errorf("terminalservice: shell %q was not found on PATH", shell)
	}
	return path, nil
}

func isAllowedWindowsShell(shell string) bool {
	base := strings.ToLower(filepath.Base(shell))
	switch base {
	case "pwsh", "pwsh.exe", "powershell", "powershell.exe", "cmd", "cmd.exe":
		return true
	default:
		return false
	}
}
