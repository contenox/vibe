package ollama

import (
	"os"
	"sync"
	"time"

	"github.com/ollama/ollama/api"
)

// defaultKeepAlive is the model-residency window every ollama request
// asserts: twice the server's 5m default, so a session stays warm across a
// user's thinking gap between turns without pinning a model forever on a
// shared box.
const defaultKeepAlive = 10 * time.Minute

// keepAliveEnv overrides the residency window ("30m", "1h", ollama duration
// syntax). Unparseable or non-positive values keep the default.
const keepAliveEnv = "CONTENOX_OLLAMA_KEEP_ALIVE"

var keepAliveOnce = sync.OnceValue(func() *api.Duration {
	d := defaultKeepAlive
	if raw := os.Getenv(keepAliveEnv); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			d = parsed
		}
	}
	return &api.Duration{Duration: d}
})

// keepAlive returns the residency window to set on every model-loading
// request (chat, stream, generate, embed). Every call path must set it: an
// omitted keep_alive resets residency to the server default.
func keepAlive() *api.Duration {
	return keepAliveOnce()
}
