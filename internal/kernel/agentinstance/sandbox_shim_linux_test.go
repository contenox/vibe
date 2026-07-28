//go:build linux

package agentinstance

import (
	"fmt"
	"os"
	"testing"

	"github.com/contenox/contenox/internal/libsandbox"
)

// TestMain makes this test binary a valid sandbox-shim host: spawning an
// agent re-execs /proc/self/exe as the sandbox shim (libsandbox.Command),
// which under `go test` is this binary. Without ShimMain here, the re-exec
// would just re-run the suite instead of confining the agent.
func TestMain(m *testing.M) {
	if handled, err := libsandbox.ShimMain(); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "agentinstance sandbox shim:", err)
			os.Exit(1)
		}
		os.Exit(0) // unreachable: a successful shim already execve'd the target
	}
	os.Exit(m.Run())
}
