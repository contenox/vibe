package billing

import "time"

// Config holds billing service settings.
type Config struct {
	RequestTimeout time.Duration
}

// Default returns the default billing config.
func Default() Config { return Config{RequestTimeout: 30 * time.Second} }
