//go:build !windows

package contenoxcli

import (
	"os"

	"golang.org/x/sys/unix"
)

func stdinHasData() (bool, error) {
	pollFDs := []unix.PollFd{{
		Fd:     int32(os.Stdin.Fd()),
		Events: unix.POLLIN | unix.POLLHUP,
	}}
	n, err := unix.Poll(pollFDs, 0)
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	return pollFDs[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0, nil
}
