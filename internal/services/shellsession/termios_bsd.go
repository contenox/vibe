//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package shellsession

import (
	"os"

	"golang.org/x/sys/unix"
)

func disableEcho(f *os.File) error {
	return clearEchoFlags(f, unix.TIOCGETA, unix.TIOCSETA)
}
