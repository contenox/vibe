package auth

import "time"

// Config holds auth service settings.
type Config struct {
	RequestTimeout time.Duration
}

// Default returns the default auth config.
func Default() Config { return Config{RequestTimeout: 5 * time.Second} }
