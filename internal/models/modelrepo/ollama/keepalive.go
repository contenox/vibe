package ollama

import (
	"os"
	"sync"
	"time"

	"github.com/ollama/ollama/api"
)

// defaultKeepAlive is the model-residency window every ollama request asserts.
// The server's own default is 5m, which is shorter than a thinking user's gap
// between turns — the per-slot KV cache (and the loaded model) would evict
// mid-conversation and the next turn would pay a cold start (provider-kv-cache
// P5). Twice the server default keeps a session warm without pinning a model
// forever on a shared box.
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
// request (chat, stream, generate, embed). Consistency matters as much as the
// value: a request that omits keep_alive resets residency to the server
// default, so one un-annotated call path would silently shorten every
// session's warmth.
func keepAlive() *api.Duration {
	return keepAliveOnce()
}
