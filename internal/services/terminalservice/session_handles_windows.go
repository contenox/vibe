//go:build windows

package terminalservice

import "golang.org/x/sys/windows"

func terminateProcessHandle(handle uintptr) error {
	return windows.TerminateProcess(windows.Handle(handle), 1)
}

func closePlatformHandle(handle uintptr) error {
	return windows.CloseHandle(windows.Handle(handle))
}

func closePseudoConsoleHandle(handle uintptr) {
	windows.ClosePseudoConsole(windows.Handle(handle))
}
