//go:build windows

package acpexec

import (
	"errors"
	"os/exec"
)

func setProcessGroup(cmd *exec.Cmd) {}

func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

func exitFromKill(waitErr error) bool {
	var exitErr *exec.ExitError
	return errors.As(waitErr, &exitErr)
}
