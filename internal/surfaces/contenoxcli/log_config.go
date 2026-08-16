package contenoxcli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/liblog"
)

// The stored settings that bound a host's log directory. They are global rather
// than workspace-scoped.
const (
	logMaxSizeKey = "log-max-size"
	logMaxFiles   = "log-max-files"
	logMaxAgeDays = "log-max-age-days"
)

// normalizeLogConfig validates and canonicalises a log setting at `config set`.
// It returns ("", nil) for keys it does not own.
func normalizeLogConfig(key, value string) (string, error) {
	switch key {
	case logMaxSizeKey:
		n, err := liblog.ParseSize(value)
		if err != nil {
			return "", fmt.Errorf("%s: %w", key, err)
		}
		// Stored canonically so `config get` reads back what the log uses.
		return liblog.FormatSize(n), nil
	case logMaxFiles, logMaxAgeDays:
		raw := strings.TrimSpace(value)
		n, err := strconv.Atoi(raw)
		if err != nil {
			return "", fmt.Errorf("%s: %q is not a whole number", key, value)
		}
		if n < 0 {
			return "", fmt.Errorf("%s: must be zero or more, got %d (0 means no limit)", key, n)
		}
		return strconv.Itoa(n), nil
	default:
		return "", nil
	}
}

// logSettingsFromConfig reads the stored log bounds. Anything unset or
// unparseable comes back as a zero, which [liblog.Writer.Reconfigure] treats as
// "leave this bound alone".
func logSettingsFromConfig(ctx context.Context, store runtimetypes.Store) (maxBytes int64, maxFiles int, maxAge time.Duration) {
	if raw := strings.TrimSpace(clikv.Read(ctx, store, logMaxSizeKey)); raw != "" {
		if n, err := liblog.ParseSize(raw); err == nil {
			maxBytes = n
		}
	}
	if raw := strings.TrimSpace(clikv.Read(ctx, store, logMaxFiles)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			maxFiles = orUnlimited(n)
		}
	}
	if raw := strings.TrimSpace(clikv.Read(ctx, store, logMaxAgeDays)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			if n == 0 {
				maxAge = liblog.Unlimited
			} else {
				maxAge = time.Duration(n) * 24 * time.Hour
			}
		}
	}
	return maxBytes, maxFiles, maxAge
}

// orUnlimited maps the operator's 0 ("no limit") onto the sentinel Reconfigure
// understands, since 0 already means "leave unchanged" there.
func orUnlimited(n int) int {
	if n == 0 {
		return liblog.Unlimited
	}
	return n
}
