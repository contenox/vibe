//go:build linux

package fleetservice

import (
	"fmt"
	"os"
	"testing"

	"github.com/contenox/beam/internal/libsandbox"
)

// TestMain makes this test binary a valid sandbox-shim host. A fleet dispatch
// brings up an agent instance, which goes through agenthost.buildAgentCmd →
// libsandbox.Command; that re-exec's /proc/self/exe as the sandbox shim to build
// the wall (Landlock, and — in the opt-in NetworkWall mode — the netns) before
// exec'ing the confined agent. Under `go test` /proc/self/exe is THIS binary, so
// without ShimMain at the top of main the re-exec would re-run the suite instead
// of confining the agent, and every dispatch test would see the downstream
// connection close. Mirrors cmd/contenox/main.go and agenthost's
// sandbox_shim_linux_test.go — the entire wiring contract (see libsandbox.ShimMain).
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
