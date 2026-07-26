package contenoxcli

import (
	"fmt"
	"io"
	"os"
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
