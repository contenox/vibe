//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package shellsession

import (
	"os"

	"golang.org/x/sys/unix"
)

// clearEchoFlags turns off every local-mode echo bit on the terminal behind f.
//
// Only the echo bits are touched. Putting the PTY in raw mode would be the
// blunt instrument here and would break the surface: canonical mode is what
// assembles a submitted line before the shell sees it, ISIG is what makes a
// Ctrl-C-equivalent reach the foreground job, and OPOST is what a program
// running inside expects from a terminal. The one thing beam does not want is
// the terminal repeating input back at it.
//
// ECHOE/ECHOK/ECHONL are cleared alongside ECHO because they are echo behaviors
// in their own right (erase/kill feedback, and echoing a newline even when ECHO
// is off), and ECHONL in particular would still put a stray blank line into the
// scrollback for every submitted command.
func clearEchoFlags(f *os.File, get, set uint) error {
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, get)
	if err != nil {
		return err
	}
	t.Lflag &^= unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ECHONL
	return unix.IoctlSetTermios(fd, set, t)
}
