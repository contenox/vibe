package contenoxcli

import (
	"fmt"
	"os"
	"strings"
)

func resolveInputFlagValue(flagName, val string) (string, error) {
	if !strings.HasPrefix(val, "@") {
		return val, nil
	}
	path := strings.TrimPrefix(val, "@")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s @%s: cannot read file: %w", flagName, path, err)
	}
	return string(data), nil
}
