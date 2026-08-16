//go:build windows

package contenoxcli

import (
	"os"

	"golang.org/x/sys/windows"
)

// openControllingTerminal opens the console input device and reports whether it
// is a real terminal. A piped stdin is never accepted: a "y" in the piped bytes
// is not operator consent.
func openControllingTerminal() (*os.File, bool, error) {
	con, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		// Only accept stdin when it is itself a console.
		if isConsole(os.Stdin) {
			return os.Stdin, true, nil
		}
		return nil, false, err
	}
	if !isConsole(con) {
		con.Close()
		return nil, false, os.ErrInvalid
	}
	return con, true, nil
}

// isConsole reports whether f refers to a console handle rather than a pipe or a
// file.
func isConsole(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}

func flushInput(f *os.File) {
	_ = windows.FlushConsoleInputBuffer(windows.Handle(f.Fd()))
}
