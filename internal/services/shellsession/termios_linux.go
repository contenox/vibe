//go:build linux

package shellsession

import (
	"os"

	"golang.org/x/sys/unix"
)

func disableEcho(f *os.File) error {
	return clearEchoFlags(f, unix.TCGETS, unix.TCSETS)
}
