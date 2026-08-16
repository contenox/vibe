package liblog

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseSize reads a human-written size — "10MB", "512kb", "1 GiB", "2048" —
// into bytes. A bare number is bytes, and KB and KiB both mean 1024.
func ParseSize(s string) (int64, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, fmt.Errorf("liblog: empty size")
	}
	lower := strings.ToLower(strings.ReplaceAll(raw, " ", ""))

	unit := int64(1)
	for _, u := range []struct {
		suffixes []string
		mult     int64
	}{
		{[]string{"gib", "gb", "g"}, 1 << 30},
		{[]string{"mib", "mb", "m"}, 1 << 20},
		{[]string{"kib", "kb", "k"}, 1 << 10},
		{[]string{"b"}, 1},
	} {
		matched := false
		for _, suffix := range u.suffixes {
			if rest, ok := strings.CutSuffix(lower, suffix); ok {
				lower, unit, matched = rest, u.mult, true
				break
			}
		}
		if matched {
			break
		}
	}

	n, err := strconv.ParseFloat(strings.TrimSpace(lower), 64)
	if err != nil {
		return 0, fmt.Errorf("liblog: %q is not a size — use a number with an optional unit, e.g. 10MB", raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("liblog: size must be positive, got %q", raw)
	}
	return int64(n * float64(unit)), nil
}

// FormatSize renders a byte count the way ParseSize would accept it back.
func FormatSize(n int64) string {
	switch {
	case n >= 1<<30:
		return trimFloat(float64(n)/float64(1<<30)) + "GB"
	case n >= 1<<20:
		return trimFloat(float64(n)/float64(1<<20)) + "MB"
	case n >= 1<<10:
		return trimFloat(float64(n)/float64(1<<10)) + "KB"
	default:
		return strconv.FormatInt(n, 10) + "B"
	}
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
