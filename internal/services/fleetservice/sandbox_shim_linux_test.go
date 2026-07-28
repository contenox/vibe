//go:build linux

package fleetservice

import (
	"fmt"
	"os"
	"testing"

	"github.com/contenox/beam/internal/libsandbox"
)

// TestMain makes this test binary a valid sandbox-shim host: a dispatched
// agent instance re-exec's /proc/self/exe as the sandbox shim (Landlock, and
// in NetworkWall mode the netns) before exec'ing the confined agent. Under
// `go test`, /proc/self/exe is this binary, so without ShimMain the re-exec
// would rerun the suite instead of confining the agent. Mirrors
// cmd/contenox/main.go (see libsandbox.ShimMain).
func TestMain(m *testing.M) {
	if handled, err := libsandbox.ShimMain(); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "fleetservice sandbox shim:", err)
			os.Exit(1)
		}
		os.Exit(0) // unreachable: a successful shim already execve'd the target
	}
	os.Exit(m.Run())
}
