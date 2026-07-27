//go:build linux

package shellsession

import (
	"os"

	"golang.org/x/sys/unix"
)

// disableEcho clears the terminal's input-echo flags. Linux spells the termios
// get/set ioctls TCGETS/TCSETS; the BSD family uses TIOCGETA/TIOCSETA (see
// termios_bsd.go), which is the only thing that differs.
func disableEcho(f *os.File) error {
	return clearEchoFlags(f, unix.TCGETS, unix.TCSETS)
}
