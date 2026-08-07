//go:build !windows

package shellsession

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/creack/pty"
)

// ptySession is a running shell attached to a pseudo-terminal master. Writing to
// it feeds the shell's stdin (that is how an approved/user line is "typed"),
// reading drains its combined output.
type ptySession struct {
	master *os.File
	cmd    *exec.Cmd
}

// startPTY launches an interactive shell rooted at spec.cwd on a fresh PTY
// sized rows x cols, becoming its controlling terminal. For an agent-facing
// shell ECHO is cleared and the prompt suppressed before the child execs (via
// pty.Open + an explicit Start, not pty.Start, to avoid a race where the child
// could snapshot termios with ECHO still on). An interactive shell keeps both:
// a human must see their own keystrokes and needs a prompt to orient by.
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

// shellFamily classifies the shell by executable name so a non-standard install
// path (/usr/local/bin/bash, a Nix store path, …) is treated like the usual one.
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

// shellSpawnArgs picks the argv for a shell family. bash/zsh start
// interactive (-i) so the user's rc file defines their aliases. An
// agent-facing bash also gets --noediting: Run submits one complete line at a
// time, so readline buys nothing while adding re-echoed input and
// bracketed-paste escapes to the scrollback. A human's terminal keeps
// readline — line editing and history are the point. rc files, aliases, job
// control, and history still apply either way.
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

// promptSuppressionEnv returns environment additions that stop an
// interactive shell from drawing a prompt into the scrollback (noise and a
// privacy leak: the stock bash prompt embeds login, host, and cwd).
// Clearing PS1 alone is not enough for bash, since an rc file can reassign
// it after the environment loads; PROMPT_COMMAND re-clears it immediately
// before each prompt. Best-effort: a shell owning its own prompt hook can
// still draw one.
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

// winsize builds a pty.Winsize, falling back to the defaults for a
// non-positive dimension so a caller that only knows one of the two is safe.
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

// resize applies a new window size to the live PTY. The kernel delivers SIGWINCH
// to the foreground process group, so a running full-screen program reflows.
func (p *ptySession) resize(rows, cols int) error {
	if p.master == nil {
		return nil
	}
	return pty.Setsize(p.master, winsize(rows, cols))
}

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
