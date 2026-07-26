//go:build !windows

package shellsession

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// ptySession is a running shell attached to a pseudo-terminal master. Writing to
// it feeds the shell's stdin (that is how an approved/user line is "typed"),
// reading drains its combined output.
type ptySession struct {
	master *os.File
	cmd    *exec.Cmd
}

// startPTY launches an interactive shell rooted at cwd on a fresh PTY. The PTY
// becomes the shell's controlling terminal, so job control and an interactive
// prompt behave as on a real terminal.
func startPTY(cwd, shell string, scrub func([]string) []string) (*ptySession, error) {
	if shell == "" {
		shell = defaultShell()
	}
	var cmd *exec.Cmd
	switch shell {
	case "/bin/bash", "/usr/bin/bash", "/bin/zsh", "/usr/bin/zsh":
		cmd = exec.Command(shell, "-i")
	default:
		cmd = exec.Command(shell)
	}
	cmd.Dir = cwd
	// Scrub serve's credentials out of the shell's environment when configured;
	// TERM is appended last so it wins regardless of the policy.
	parent := os.Environ()
	if scrub != nil {
		parent = scrub(parent)
	}
	cmd.Env = append(parent, "TERM=xterm-256color")
	master, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	_ = pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 120})
	return &ptySession{master: master, cmd: cmd}, nil
}

func (p *ptySession) Read(b []byte) (int, error)  { return p.master.Read(b) }
func (p *ptySession) Write(b []byte) (int, error) { return p.master.Write(b) }

// close kills the shell process and releases the PTY master.
func (p *ptySession) close() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.master.Close()
}

// wait reaps the shell process (called from the read loop once output ends).
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
