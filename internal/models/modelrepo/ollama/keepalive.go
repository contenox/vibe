package ollama

import (
	"os"
	"sync"
	"time"
)

const defaultKeepAlive = 10 * time.Minute

const keepAliveEnv = "CONTENOX_OLLAMA_KEEP_ALIVE"

var keepAliveOnce = sync.OnceValue(func() *Duration {
	d := defaultKeepAlive
	if raw := os.Getenv(keepAliveEnv); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			d = parsed
		}
	}
	return &Duration{Duration: d}
})

// Every model-loading call path must set it: an omitted keep_alive resets residency to the
// server default.
func keepAlive() *Duration {
	return keepAliveOnce()
}
