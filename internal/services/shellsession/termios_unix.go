//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package shellsession

import (
	"os"

	"golang.org/x/sys/unix"
)

// clearEchoFlags turns off every local-mode echo bit on the terminal behind
// f, leaving canonical mode, ISIG, and OPOST untouched — raw mode would
// break line assembly, Ctrl-C, and programs that expect a normal terminal.
// ECHOE/ECHOK/ECHONL are cleared alongside ECHO since each is its own echo
// behavior (erase/kill feedback, a stray newline).
func clearEchoFlags(f *os.File, get, set uint) error {
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, get)
	if err != nil {
		return err
	}
	t.Lflag &^= unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ECHONL
	return unix.IoctlSetTermios(fd, set, t)
}
