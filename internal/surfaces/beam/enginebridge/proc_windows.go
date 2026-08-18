//go:build windows

package enginebridge

import (
	"os/exec"

	libacp "github.com/contenox/contenox/libacp"
)

// setProcGroup is a no-op on Windows; exec.Cmd has no process-group notion
// here and Kill takes the direct process only.
func setProcGroup(*exec.Cmd) {}

func killProcTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

func exitStatus(cmd *exec.Cmd) libacp.TerminalExitStatus {
	var status libacp.TerminalExitStatus
	ps := cmd.ProcessState
	if ps == nil {
		code := -1
		status.ExitCode = &code
		return status
	}
	code := ps.ExitCode()
	status.ExitCode = &code
	return status
}
