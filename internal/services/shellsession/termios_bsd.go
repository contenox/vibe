//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package shellsession

import (
	"os"

	"golang.org/x/sys/unix"
)

// disableEcho clears the terminal's input-echo flags. BSD-family kernels spell
// the termios get/set ioctls TIOCGETA/TIOCSETA rather than Linux's TCGETS/TCSETS
// (see termios_linux.go); nothing else differs.
func disableEcho(f *os.File) error {
	return clearEchoFlags(f, unix.TIOCGETA, unix.TIOCSETA)
}
