//go:build !windows

package shellsession

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/creack/pty"
)

type ptySession struct {
	master *os.File
	cmd    *exec.Cmd
}

func startPTY(spec spawnSpec) (*ptySession, error) {
	shell := spec.shell
	if shell == "" {
		shell = defaultShell()
	}
	cmd := exec.Command(shell, shellSpawnArgs(shell, spec.interactive)...)
	cmd.Dir = spec.cwd
	// Scrub serve's credentials when configured; TERM/prompt-suppression vars are appended last so they win.
	parent := os.Environ()
	if spec.scrub != nil {
		parent = spec.scrub(parent)
	}
	cmd.Env = append(parent, "TERM=xterm-256color")
	if !spec.interactive {
		cmd.Env = append(cmd.Env, promptSuppressionEnv(shell)...)
	}

	// pty.Open + explicit Start (not pty.Start) avoids a race where the child could snapshot termios with ECHO still on.
	master, tty, err := pty.Open()
	if err != nil {
		return nil, err
	}
	_ = pty.Setsize(master, winsize(spec.rows, spec.cols))
	// Master and slave share one termios, so clearing ECHO on the slave configures the pair.
	if !spec.interactive {
		_ = disableEcho(tty)
	}

	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	// Setsid + Setctty makes the PTY the shell's controlling terminal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		_ = tty.Close()
		_ = master.Close()
		return nil, err
	}
	// Close our copy so the master reports EOF once the shell exits.
	_ = tty.Close()
	return &ptySession{master: master, cmd: cmd}, nil
}

func shellFamily(shell string) string {
	switch filepath.Base(shell) {
	case "bash":
		return "bash"
	case "zsh":
		return "zsh"
	default:
		return ""
	}
}

func shellSpawnArgs(shell string, interactive bool) []string {
	switch shellFamily(shell) {
	case "bash":
		if interactive {
			return []string{"-i"}
		}
		return []string{"--noediting", "-i"}
	case "zsh":
		return []string{"-i"}
	default:
		return nil
	}
}

func promptSuppressionEnv(shell string) []string {
	switch shellFamily(shell) {
	case "bash":
		return []string{"PS1=", "PS2=", "PROMPT_COMMAND=PS1= PS2="}
	case "zsh":
		// zsh has no PROMPT_COMMAND; PROMPT/RPROMPT are its PS1/right prompt.
		return []string{"PS1=", "PS2=", "PROMPT=", "RPROMPT=", "PROMPT_COMMAND="}
	default:
		return []string{"PS1=", "PS2=", "PROMPT_COMMAND="}
	}
}

func winsize(rows, cols int) *pty.Winsize {
	if rows <= 0 {
		rows = defaultRows
	}
	if cols <= 0 {
		cols = defaultCols
	}
	return &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}
}

func (p *ptySession) Read(b []byte) (int, error)  { return p.master.Read(b) }
func (p *ptySession) Write(b []byte) (int, error) { return p.master.Write(b) }

func (p *ptySession) resize(rows, cols int) error {
	if p.master == nil {
		return nil
	}
	// pty.Setsize triggers SIGWINCH to the foreground process group, so a running full-screen program reflows.
	return pty.Setsize(p.master, winsize(rows, cols))
}

func (p *ptySession) close() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.master.Close()
}

func (p *ptySession) wait() {
	if p.cmd != nil {
		_ = p.cmd.Wait()
	}
}

func defaultShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		if _, err := os.Stat(s); err == nil {
			return s
		}
	}
	for _, candidate := range []string{"/bin/bash", "/bin/zsh", "/bin/sh"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "/bin/sh"
}
