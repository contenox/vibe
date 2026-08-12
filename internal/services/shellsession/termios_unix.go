//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package shellsession

import (
	"os"

	"golang.org/x/sys/unix"
)

func clearEchoFlags(f *os.File, get, set uint) error {
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, get)
	if err != nil {
		return err
	}
	// Clears only echo bits (ECHO/ECHOE/ECHOK/ECHONL); canonical mode, ISIG, and OPOST stay untouched, or raw mode would break line assembly, Ctrl-C, and normal terminal behavior.
	t.Lflag &^= unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ECHONL
	return unix.IoctlSetTermios(fd, set, t)
}
