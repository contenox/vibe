//go:build windows

package term

import (
	"os"
	"time"
)

// notifyResize has no signal to subscribe to on Windows: the console reports a
// size change as a WINDOW_BUFFER_SIZE_EVENT on the input handle, which this
// engine never reads (it takes the console as a byte stream). The returned
// channel therefore stays silent and resizePollInterval carries the duty.
func notifyResize() chan os.Signal { return make(chan os.Signal, 1) }

// stopResize is a no-op counterpart to notifyResize.
func stopResize(ch chan os.Signal) {}

// resizePollInterval makes the resize watcher poll on Windows, since nothing
// wakes it there. Without it a Windows terminal never reports a resize at all
// and the layout stays frozen at its startup size until Suspend/resume happens
// to re-query it.
//
// 200ms is chosen against a drag, not a single resize: it is short enough that
// the window settles visibly on release, and long enough that the cost is one
// GetConsoleScreenBufferInfo call five times a second while idle. The watcher
// compares the result and only emits on an actual change, so a steady size
// costs no events and no repaints.
func resizePollInterval() time.Duration { return 200 * time.Millisecond }

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
