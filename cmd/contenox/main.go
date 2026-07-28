// Contenox CLI: run task chains locally with SQLite-backed state.
package main

import (
	"log"
	"os"

	"github.com/contenox/beam/internal/libsandbox"
	"github.com/contenox/beam/internal/surfaces/contenoxcli"
)

func main() {
	// Must run first: re-exec'd sandbox shim entrypoint. Fail-open-critical —
	// on error we log.Fatal rather than fall through and run the agent unconfined.
	if handled, err := libsandbox.ShimMain(); handled {
		if err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}
	contenoxcli.Main()
}
