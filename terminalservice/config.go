package terminalservice

import (
	"fmt"
	"strconv"
	"strings"
)

// Config holds interactive terminal settings (from env / server config).
type Config struct {
	Enabled      bool
	AllowedRoot  string
	MaxSessions  int
	DefaultShell string
}

// DefaultMaxSessions is used when max sessions is unset.
const DefaultMaxSessions = 8

// ParseEnv builds a [Config] from raw string fields (e.g. [serverapi.Config] terminal_*).
// When Enabled is false, AllowedRoot may be empty.
func ParseEnv(terminalEnabled, terminalAllowedRoot, terminalMaxSessions, terminalShell string) (Config, error) {
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
	maxStr := strings.TrimSpace(terminalMaxSessions)
	if maxStr == "" {
		cfg.MaxSessions = DefaultMaxSessions
	} else {
		n, err := strconv.Atoi(maxStr)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("terminalservice: invalid terminal_max_sessions %q", maxStr)
		}
		cfg.MaxSessions = n
	}
	cfg.DefaultShell = strings.TrimSpace(terminalShell)
	if cfg.DefaultShell == "" {
		cfg.DefaultShell = "/bin/bash"
	}
	return cfg, nil
}
