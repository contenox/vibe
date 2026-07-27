//go:build windows

package acpexec

import (
	"errors"
	"os/exec"
)

// setProcessGroup is a no-op on Windows: there is no Setpgid equivalent here;
// descendant cleanup would need Job Objects, which the Windows product
// surface can add when it lands (see docs/development/windows-development.md).
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessTree kills the direct child only — see setProcessGroup for why
// this is weaker than the unix build.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

// exitFromKill on Windows cannot distinguish death-by-our-Kill from other
// abnormal exits (TerminateProcess reports a plain exit code), so any
// ExitError after the kill branch is treated as kill-induced — weaker than
// the unix build, matching killProcessTree above.
func exitFromKill(waitErr error) bool {
	var exitErr *exec.ExitError
	return errors.As(waitErr, &exitErr)
}
