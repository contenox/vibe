//go:build !windows

package terminalservice

func terminateProcessHandle(uintptr) error { return nil }

func closePlatformHandle(uintptr) error { return nil }

func closePseudoConsoleHandle(uintptr) {}
