package libsandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/contenox/contenox/internal/libtracker"
)

// Command assembles the confined command for name (with args): validates
// spec, pins the working directory to spec.WorkspaceRoot, scrubs the
// environment (HOME forced to spec.Home, PATH emulated to the confined exec
// dirs, see scrubEnv/validatePATH), and applies the platform's isolation
// before returning the ready-to-run *exec.Cmd. If isolation fails to build,
// Command returns an error rather than a runnable-but-unconfined *exec.Cmd.
//
// It does not start the process and does not bind the command's lifetime to
// ctx — the caller owns start/stop/teardown; ctx only scopes the assembly
// (drives the ActivityTracker and is available to the isolation seam).
//
// Errors wrap ErrInvalidSpec (missing name/workspace/home) or
// ErrInvalidCarveout (a malformed hole), and are reported to spec.Tracker.
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

	// Refine the canonical PATH floor into the wall-filtered PATH (confinedPATH)
	// so the agent finds its real toolchain, not just the stock dirs. An
	// explicit EnvSet["PATH"] is left untouched as the caller's own override.
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
