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

// waitReadable degrades to "always readable" on Windows, so the reader
// blocks in Read: there is no portable way to poll a console handle through
// an *os.File, and the wake pipe cannot interrupt a blocking console read.
// Two contract exceptions follow from this, Windows-only: Suspend cannot
// guarantee the reader is parked before the child starts, so a keystroke or
// two may be swallowed; and Close may return while the reader is still
// blocked in Read, falling back to its readerStopWait timeout rather than
// the wake-pipe guarantee every other platform gives.
func waitReadable(fd, wake uintptr, timeout time.Duration) (ready, woken bool, err error) {
	return true, false, nil
}
