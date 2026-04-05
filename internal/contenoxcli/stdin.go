package contenoxcli

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const maxCLIStdinBytes int64 = 50 << 20

func readStdinIfAvailable(maxBytes int64) (string, bool, error) {
	stat, err := os.Stdin.Stat()
	if err != nil || (stat.Mode()&os.ModeCharDevice) != 0 {
		return "", false, nil
	}

	ready, err := stdinHasData()
	if err != nil {
		return "", false, fmt.Errorf("failed to inspect stdin: %w", err)
	}
	if !ready {
		return "", false, nil
	}

	data, err := io.ReadAll(io.LimitReader(os.Stdin, maxBytes))
	if err != nil {
		return "", false, fmt.Errorf("failed to read from stdin: %w", err)
	}
	return string(data), true, nil
}

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
