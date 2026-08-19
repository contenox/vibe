//go:build !windows

package clientfsterm

import (
	"os/exec"
	"syscall"

	libacp "github.com/contenox/contenox/libacp"
)

// setProcGroup puts the command in its own process group, so killing a
// shell-wrapped command reaps its whole pipeline rather than only the shell.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
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
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		sig := ws.Signal().String()
		status.Signal = &sig
		return status
	}
	code := ps.ExitCode()
	status.ExitCode = &code
	return status
}
