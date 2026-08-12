//go:build !linux

package libsandbox

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/contenox/contenox/libtracker"
)

func applyIsolation(ctx context.Context, cmd *exec.Cmd, spec Spec, tracker libtracker.ActivityTracker) error {
	return fmt.Errorf("%w: OS-level confinement is implemented only on Linux; "+
		"refusing to hand back an unconfined command (build a bare exec.Command "+
		"yourself if an unconfined run is genuinely intended)", ErrIsolation)
}
