//go:build linux

package acpsvc

import (
	"fmt"
	"os"
	"testing"

	"github.com/contenox/contenox/internal/libsandbox"
)

// TestMain makes this test binary a valid sandbox-shim host: libsandbox.Command
// re-execs /proc/self/exe (this binary, under `go test`) to build the sandbox
// wall before exec'ing the confined agent; without ShimMain that re-exec would
// rerun the suite instead. See libsandbox.ShimMain.
func TestMain(m *testing.M) {
	if handled, err := libsandbox.ShimMain(); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "acpsvc sandbox shim:", err)
			os.Exit(1)
		}
		os.Exit(0) // unreachable: a successful shim already execve'd the target
	}
	os.Exit(m.Run())
}
