package libsandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/contenox/contenox/libtracker"
)

// Command assembles the confined *exec.Cmd for name and args (validated spec, scrubbed env, applied isolation) without starting it; errors wrap ErrInvalidSpec or ErrInvalidCarveout.
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

	// Only applied when EnvSet doesn't already set PATH — an explicit override bypasses the confined PATH floor.
	if _, set := spec.EnvSet["PATH"]; !set {
		cmd.Env = OverlayEnv(cmd.Env, map[string]string{
			"PATH": confinedPATH(os.Getenv("PATH"), spec.Home, spec.FS),
		})
	}

	// Fails only on an explicit EnvSet["PATH"] naming an uncarved dir — beats
	// an opaque Landlock EACCES at run time.
	if err := validatePATH(lookupEnv(cmd.Env, "PATH"), spec.Home, spec.FS); err != nil {
		reportErr(err)
		return nil, err
	}

	if err := applyIsolation(ctx, cmd, spec, tracker); err != nil {
		err = fmt.Errorf("libsandbox: apply isolation for %q: %w", name, err)
		reportErr(err)
		return nil, err
	}

	// Reports shape only (counts, pinned dir) — never the scrubbed values.
	reportChange(name, map[string]any{
		"dir":  cmd.Dir,
		"args": len(args),
		"fs":   len(spec.FS),
		"net":  len(spec.Net),
		"env":  len(cmd.Env),
	})
	return cmd, nil
}
