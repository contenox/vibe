// Contenox CLI: run task chains locally with SQLite-backed state.
package main

import (
	"log"
	"os"

	"github.com/contenox/beam/internal/libsandbox"
	"github.com/contenox/beam/internal/surfaces/contenoxcli"
)

func main() {
	// Sandbox shim entrypoint — FAIL-OPEN CRITICAL, must run at the very top of
	// main() before any flag parsing or real work. When contenox spawns a
	// confined agent (runtime/agenthost → libsandbox.Command) it re-exec's THIS
	// binary as the sandbox shim; ShimMain detects that (via a sentinel env var),
	// builds the wall — a fresh user+network namespace, the Landlock filesystem
	// ruleset — and syscall.Exec's the real agent inside it, so control never
	// returns here on success.
	//
	//   - handled == false: an ordinary launch (no sentinel). ShimMain did
	//     nothing; fall through to the CLI unchanged.
	//   - handled == true, err != nil: the wall could NOT be built. The one
	//     inviolable rule is to never run the agent unconfined, so this is fatal —
	//     log.Fatal exits non-zero and we do NOT fall through to contenoxcli.Main().
	//   - handled == true, err == nil: unreachable in practice (a successful shim
	//     already execve'd the agent and never returned); os.Exit(0) belt-and-
	//     braces guarantees we still never fall through to the CLI when handled.
	if handled, err := libsandbox.ShimMain(); handled {
		if err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}
	contenoxcli.Main()
}
