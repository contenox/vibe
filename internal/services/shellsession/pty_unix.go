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

// startPTY launches an interactive shell rooted at cwd on a fresh PTY sized
// rows x cols. The PTY becomes the shell's controlling terminal, so job control
// behaves as on a real terminal.
//
// Two properties are established BEFORE the shell execs, because a shell that
// starts up on a "normal" terminal saves those attributes and can restore them
// later:
//
//   - ECHO is cleared on the pair. beam submits whole lines programmatically and
//     the surface already shows the user what it sent; a terminal that echoes
//     them back makes every `!` line appear twice in the shared scrollback.
//   - The window size is applied to the pair, so the shell's very first prompt
//     already sees the caller's geometry and no startup SIGWINCH is needed.
//
// Doing this through pty.Open + an explicit Start (rather than pty.Start, which
// hides the slave) is what removes the race: with pty.Start the child can be
// running — and can have snapshotted a terminal with ECHO on — before the parent
// gets a chance to touch termios.
func startPTY(cwd, shell string, scrub func([]string) []string, rows, cols int) (*ptySession, error) {
	if shell == "" {
		shell = defaultShell()
	}
	cmd := exec.Command(shell, shellSpawnArgs(shell)...)
	cmd.Dir = cwd
	// Scrub serve's credentials out of the shell's environment when configured;
	// TERM and the prompt-suppression vars are appended last so they win
	// regardless of the policy.
	parent := os.Environ()
	if scrub != nil {
		parent = scrub(parent)
	}
	cmd.Env = append(append(parent, "TERM=xterm-256color"), promptSuppressionEnv(shell)...)

	master, tty, err := pty.Open()
	if err != nil {
		return nil, err
	}
	_ = pty.Setsize(master, winsize(rows, cols))
	// The master and the slave share one termios on every platform we build for,
	// so clearing ECHO on the slave we still hold configures the pair.
	_ = disableEcho(tty)

	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	// Setsid + Setctty (with the slave as fd 0) is what makes the PTY the shell's
	// controlling terminal — the same thing pty.Start does for us normally.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		_ = tty.Close()
		_ = master.Close()
		return nil, err
	}
	// The child holds its own descriptors now; keeping ours open would stop the
	// master from ever reporting EOF when the shell dies.
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

// shellSpawnArgs picks the argv for a shell family.
//
// bash and zsh are started interactive (-i) on purpose: the user's rc file is
// what defines their aliases and functions, and `!ll` has to mean what it means
// in their own terminal.
//
// bash additionally gets --noediting. beam never delivers keystrokes to this
// PTY — Run submits one complete line at a time — so readline buys nothing here
// while costing a line-editing layer that re-echoes input and wraps every prompt
// in bracketed-paste escapes (\e[?2004h/l) that are pure noise in a scrollback
// the agent also reads. Disabling the editor does NOT make the shell
// non-interactive: rc files, aliases, job control and history all still apply.
func shellSpawnArgs(shell string) []string {
	switch shellFamily(shell) {
	case "bash":
		return []string{"--noediting", "-i"}
	case "zsh":
		return []string{"-i"}
	default:
		return nil
	}
}

// promptSuppressionEnv returns the environment additions that stop an
// interactive shell from drawing a prompt into the scrollback.
//
// A prompt is both noise (it doubles the line count of `!echo AAA`) and a
// privacy leak: the stock bash prompt embeds the login name, the hostname and
// the absolute cwd, and this scrollback is read by the agent and shows up in
// shared transcripts.
//
// Clearing PS1 through the environment is not enough on its own for bash: an
// interactive bash sources the user's rc file AFTER importing the environment,
// and distro defaults (Debian/Ubuntu's /etc/skel/.bashrc, for one) assign PS1
// unconditionally. PROMPT_COMMAND is the hook that runs immediately before each
// prompt is drawn, so re-clearing PS1/PS2 from there wins over the rc file —
// including for the very first prompt.
//
// This is best-effort by construction. A shell we do not recognize, or an rc
// file that itself owns PROMPT_COMMAND (starship, oh-my-bash, powerline) or
// zsh's precmd hooks, can still draw a prompt; the surface must tolerate one.
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
