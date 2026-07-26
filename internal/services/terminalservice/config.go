package terminalservice

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Config holds terminal service settings.
type Config struct {
	Enabled      bool
	AllowedRoot  string
	DefaultShell string
	// IdleTimeout is the maximum time a detached session is kept alive.
	// Zero disables idle reaping.
	IdleTimeout time.Duration
	// MaxSessions is the maximum number of concurrent live PTY sessions.
	// Zero means unlimited.
	MaxSessions int
	// ScrubEnv, when set, maps the parent process environment to the one a spawned
	// terminal inherits — the credential-leak fix. Nil (the default) inherits the
	// full environment: the terminal is the operator's own trusted shell, so it is
	// scrubbed only when the operator opts in (SANDBOX_TERMINAL_SCRUB).
	ScrubEnv func([]string) []string
}

const (
	DefaultIdleTimeout = 30 * time.Minute
	DefaultMaxSessions = 8
)

// IsEnabled returns true for accepted truthy terminal feature flag values.
func IsEnabled(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// ParseEnv builds a Config from raw env-backed string fields.
func ParseEnv(terminalEnabled, terminalAllowedRoot, terminalShell, terminalIdleTimeout, terminalMaxSessions string) (Config, error) {
	cfg := Config{Enabled: IsEnabled(terminalEnabled)}
	if !cfg.Enabled {
		return cfg, nil
	}
	cfg.AllowedRoot = strings.TrimSpace(terminalAllowedRoot)
	if cfg.AllowedRoot == "" {
		return Config{}, fmt.Errorf("terminalservice: terminal_enabled=true requires terminal_allowed_root")
	}
	cfg.DefaultShell = strings.TrimSpace(terminalShell)
	if cfg.DefaultShell == "" {
		cfg.DefaultShell = defaultTerminalShell()
	}
	idleStr := strings.TrimSpace(terminalIdleTimeout)
	if idleStr == "" {
		cfg.IdleTimeout = DefaultIdleTimeout
	} else {
		d, err := time.ParseDuration(idleStr)
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("terminalservice: invalid terminal_idle_timeout %q", idleStr)
		}
		cfg.IdleTimeout = d
	}
	maxStr := strings.TrimSpace(terminalMaxSessions)
	if maxStr == "" {
		cfg.MaxSessions = DefaultMaxSessions
		return cfg, nil
	}
	max, err := strconv.Atoi(maxStr)
	if err != nil || max < 0 {
		return Config{}, fmt.Errorf("terminalservice: invalid terminal_max_sessions %q", maxStr)
	}
	cfg.MaxSessions = max
	return cfg, nil
}
