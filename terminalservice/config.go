package terminalservice

import (
	"fmt"
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
}

// DefaultIdleTimeout is used when [Config.IdleTimeout] is unset.
const DefaultIdleTimeout = 30 * time.Minute

// ParseEnv builds a [Config] from raw string fields.
// terminalIdleTimeout accepts any [time.ParseDuration] string; empty defaults to [DefaultIdleTimeout].
func ParseEnv(terminalEnabled, terminalAllowedRoot, terminalShell, terminalIdleTimeout string) (Config, error) {
	cfg := Config{
		Enabled: strings.EqualFold(strings.TrimSpace(terminalEnabled), "true"),
	}
	if !cfg.Enabled {
		return cfg, nil
	}
	cfg.AllowedRoot = strings.TrimSpace(terminalAllowedRoot)
	if cfg.AllowedRoot == "" {
		return Config{}, fmt.Errorf("terminalservice: terminal_enabled=true requires terminal_allowed_root")
	}
	cfg.DefaultShell = strings.TrimSpace(terminalShell)
	if cfg.DefaultShell == "" {
		cfg.DefaultShell = "/bin/bash"
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
	return cfg, nil
}
