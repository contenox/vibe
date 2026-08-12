//go:build linux

package fleetservice

import (
	"fmt"
	"os"
	"testing"

	"github.com/contenox/contenox/internal/libsandbox"
)

// TestMain makes this test binary a valid sandbox-shim host, since a dispatched instance re-execs /proc/self/exe as the sandbox shim before exec'ing the confined agent, and without it that re-exec would rerun the suite instead.
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
