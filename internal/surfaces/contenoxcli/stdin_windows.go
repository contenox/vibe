//go:build windows

package contenoxcli

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var peekNamedPipe = windows.NewLazySystemDLL("kernel32.dll").NewProc("PeekNamedPipe")

func stdinHasData() (bool, error) {
	var available uint32
	r1, _, err := peekNamedPipe.Call(
		uintptr(windows.Handle(os.Stdin.Fd())),
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&available)),
		0,
	)
	if r1 != 0 {
		return available > 0, nil
	}
	if errors.Is(err, windows.ERROR_BROKEN_PIPE) {
		return false, nil
	}
	if errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		return true, nil
	}
	return false, err
}
