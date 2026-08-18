//go:build !windows

package term

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// notifyResize subscribes to the terminal size-change signal.
func notifyResize() chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	return ch
}

// resizePollInterval is zero here: SIGWINCH wakes the watcher exactly when the
// size changes, so polling would only add wakeups that find nothing. A zero
// interval tells the watcher to run signal-driven (see ANSI.watchResize).
func resizePollInterval() time.Duration { return 0 }

// stopResize unsubscribes; safe on a nil channel.
func stopResize(ch chan os.Signal) {
	if ch != nil {
		signal.Stop(ch)
	}
}

// waitReadable reports whether fd has input available within timeout, and
// whether the wake descriptor fired instead. Polling rather than blocking in
// Read is what makes Suspend honest — while a child process owns the
// terminal beam must not swallow one keystroke — and what lets Close return
// without waiting for a keystroke that may never come.
func waitReadable(fd, wake uintptr, timeout time.Duration) (ready, woken bool, err error) {
	ms := int(timeout.Milliseconds())
	if ms < 1 {
		ms = 1
	}
	fds := []unix.PollFd{
		{Fd: int32(fd), Events: unix.POLLIN},
		{Fd: int32(wake), Events: unix.POLLIN},
	}
	for {
		n, err := unix.Poll(fds, ms)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, false, err
		}
		if n == 0 {
			return false, false, nil
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			return false, false, os.ErrClosed
		}
		// POLLHUP counts as ready so the following Read observes EOF.
		return fds[0].Revents != 0, fds[1].Revents != 0, nil
	}
}
