//go:build !windows

package contenoxcli

import (
	"os"

	"golang.org/x/term"
)

// openControllingTerminal opens /dev/tty and reports whether it is a real
// terminal. Falling back to os.Stdin is deliberately not done: when stdin is a
// pipe, a "y" in the piped bytes is not operator consent.
func openControllingTerminal() (*os.File, bool, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		// No controlling terminal. Only accept stdin when it is itself a tty.
		if term.IsTerminal(int(os.Stdin.Fd())) {
			return os.Stdin, true, nil
		}
		return nil, false, err
	}
	if !term.IsTerminal(int(tty.Fd())) {
		tty.Close()
		return nil, false, os.ErrInvalid
	}
	return tty, true, nil
}

// flushInput discards keystrokes buffered before the prompt was drawn.
func flushInput(f *os.File) {
	flushTerminalInput(f)
}
