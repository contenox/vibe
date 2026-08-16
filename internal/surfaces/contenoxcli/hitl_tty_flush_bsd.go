//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package contenoxcli

import (
	"os"

	"golang.org/x/sys/unix"
)

// fread is the BSD FREAD bit, spelled out because x/sys/unix does not export it
// on every BSD.
const fread = 0x0001

// flushTerminalInput discards the terminal's pending input queue.
func flushTerminalInput(f *os.File) {
	_ = unix.IoctlSetPointerInt(int(f.Fd()), unix.TIOCFLUSH, fread)
}
