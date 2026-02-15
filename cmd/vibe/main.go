// Contenox Vibe: run task chains locally with SQLite, in-memory bus, and estimate tokenizer.
// No Postgres, no NATS, no tokenizer service. Use for dev/admin shadow orchestration.
package main

import (
	"github.com/contenox/vibe/internal/vibecli"
)

func main() {
	vibecli.Main()
}
