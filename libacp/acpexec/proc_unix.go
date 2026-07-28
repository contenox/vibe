//go:build unix

package acpexec

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcessGroup makes the subprocess the leader of its own process group,
// so killing a wrapper's forked grandchild (npx/uvx, shell shims) doesn't
// leave it alive holding our pipes and blocking the cmd.Wait reaper forever.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessTree kills the subprocess's entire process group (see
// setProcessGroup), falling back to killing just the direct child if group
// delivery fails.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

// exitFromKill reports whether waitErr records death by SIGKILL — the only
// exit Close's kill escalation can have caused. "The kill branch ran" does
// not imply "the kill did it": the Wait reaper can lag past the grace period
// for a process that already exited on its own, whose genuine status must
// still surface.
func exitFromKill(waitErr error) bool {
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return false
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
}
