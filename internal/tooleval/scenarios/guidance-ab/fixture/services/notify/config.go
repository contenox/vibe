package notify

import "time"

// Config holds notify service settings.
type Config struct {
	RequestTimeout time.Duration
}

// Default returns the default notify config.
func Default() Config { return Config{RequestTimeout: 8 * time.Second} }
