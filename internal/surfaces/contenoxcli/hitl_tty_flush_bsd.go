//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package contenoxcli

import (
	"os"

	"golang.org/x/sys/unix"
)

// fread is the BSD FREAD bit — the "flush the read queue" selector for
// TIOCFLUSH. x/sys/unix does not export it on every BSD, and the value is
// stable kernel ABI, so it is spelled out here.
const fread = 0x0001

// flushTerminalInput discards the terminal's pending input queue. BSD-family
// kernels spell this TIOCFLUSH with a FREAD bitmask rather than Linux's TCFLSH.
func flushTerminalInput(f *os.File) {
	_ = unix.IoctlSetPointerInt(int(f.Fd()), unix.TIOCFLUSH, fread)
}
