//go:build windows

package term

import (
	"os"
	"time"
)

// notifyResize has no signal to subscribe to on Windows; size changes are
// picked up on the next explicit query instead.
func notifyResize() chan os.Signal { return make(chan os.Signal, 1) }

// stopResize is a no-op counterpart to notifyResize.
func stopResize(ch chan os.Signal) {}

// waitReadable degrades to "always readable", so the reader blocks in Read.
// There is no portable way to poll a console handle through an *os.File, and
// the wake pipe cannot interrupt a blocking console read.
//
// Two contract exceptions follow, and they are exceptions on Windows ONLY:
//
//   - Suspend cannot guarantee the reader is parked before the child starts.
//     It still restores the terminal; a keystroke or two typed in the instant
//     the child takes over may be swallowed by the pending read instead.
//   - Close may return while the reader is still blocked in Read. Everywhere
//     else Close's guarantee is absolute — no engine goroutine touches the tty
//     after it returns — because the wake pipe ends the poll immediately. Here
//     the reader lets go only when the next keystroke arrives or the handle is
//     closed, so Close falls back to its readerStopWait timeout and returns
//     anyway rather than hanging on a key that may never be pressed. A caller
//     that hands the same handle to another process immediately afterwards
//     can therefore lose one keystroke to the engine on this platform.
func waitReadable(fd, wake uintptr, timeout time.Duration) (ready, woken bool, err error) {
	return true, false, nil
}
