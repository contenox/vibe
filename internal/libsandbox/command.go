package libsandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/contenox/contenox/libtracker"
)

// Command assembles the confined *exec.Cmd for name and args without starting
// it; errors wrap ErrInvalidSpec or ErrInvalidCarveout.
func Command(ctx context.Context, spec Spec, name string, args ...string) (*exec.Cmd, error) {
	tracker := spec.Tracker
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	reportErr, reportChange, end := tracker.Start(ctx, "confine", "sandbox",
		"command", name, "workspace", spec.WorkspaceRoot)
	defer end()

	if strings.TrimSpace(name) == "" {
		err := fmt.Errorf("%w: name (the executable to confine) is required", ErrInvalidSpec)
		reportErr(err)
		return nil, err
	}
	if err := spec.validate(); err != nil {
		reportErr(err)
		return nil, err
	}

	cmd := exec.Command(name, args...)
	cmd.Dir = spec.WorkspaceRoot
	cmd.Env = scrubEnv(os.Environ(), spec.EnvAllow, spec.EnvSet, spec.Home)

	// An explicit EnvSet PATH bypasses the confined PATH floor.
	if _, set := spec.EnvSet["PATH"]; !set {
		cmd.Env = OverlayEnv(cmd.Env, map[string]string{
			"PATH": confinedPATH(os.Getenv("PATH"), spec.Home, spec.FS),
		})
	}

	if err := validatePATH(lookupEnv(cmd.Env, "PATH"), spec.Home, spec.FS); err != nil {
		reportErr(err)
		return nil, err
	}

	if err := applyIsolation(ctx, cmd, spec, tracker); err != nil {
		err = fmt.Errorf("libsandbox: apply isolation for %q: %w", name, err)
		reportErr(err)
		return nil, err
	}

	reportChange(name, map[string]any{
		"dir":  cmd.Dir,
		"args": len(args),
		"fs":   len(spec.FS),
		"net":  len(spec.Net),
		"env":  len(cmd.Env),
	})
	return cmd, nil
}
