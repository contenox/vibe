package contenoxcli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/contenox/beam/internal/libsandbox"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/shellenvservice"
)

// resolveSandboxScrubs turns the SANDBOX_* configuration into the two environment
// scrub hooks serve threads into the shells it spawns: one for agent-reachable
// shells (the local_shell tool and the shell_session PTY / "!" passthrough), one
// for the interactive terminal panel. Each hook maps the parent environment to
// the one a spawned shell inherits; a nil hook means "inherit everything" (mode
// off). The operator's SANDBOX_ENV_ALLOW / SANDBOX_ENV_DENY extend whichever scrub
// is active. Agent shells default to deny-secrets (strip known credentials, keep
// the toolchain), the operator terminal to off (a trusted human shell).
//
// warnW receives one line per misspelled SANDBOX_*_SCRUB value (see
// resolveScrubMode); nil silences it.
func resolveSandboxScrubs(config *sandboxEnvConfig, injectGlobal func() map[string]string, warnW io.Writer) (shell, terminal func([]string) []string) {
	extraAllow := libsandbox.ParseEnvList(config.SandboxEnvAllow)
	extraDeny := libsandbox.ParseEnvList(config.SandboxEnvDeny)
	shellFilter := libsandbox.EnvScrub(resolveScrubMode(config.SandboxShellScrub, libsandbox.ScrubDenySecrets, warnW), extraAllow, extraDeny)
	terminalFilter := libsandbox.EnvScrub(resolveScrubMode(config.SandboxTerminalScrub, libsandbox.ScrubOff, warnW), extraAllow, extraDeny)
	return composeShellEnv(shellFilter, injectGlobal), composeShellEnv(terminalFilter, injectGlobal)
}

// composeShellEnv builds the env hook a shell-exec site receives: run the scrub
// filter (strip serve's credentials) if one is active, then overlay the operator's
// live global shell-env variables on top — so injected values win over whatever
// the policy passed, and apply even when the scrub is off. Returns nil only when
// there is nothing to do (no filter, no injector), preserving the legacy full
// inherit for an unconfigured serve.
func composeShellEnv(filter func([]string) []string, injectGlobal func() map[string]string) func([]string) []string {
	if filter == nil && injectGlobal == nil {
		return nil
	}
	return func(parent []string) []string {
		env := parent
		if filter != nil {
			env = filter(env)
		}
		if injectGlobal != nil {
			env = libsandbox.OverlayEnv(env, injectGlobal())
		}
		return env
	}
}

// newLiveGlobalShellEnv returns a getter for the operator's global shell-env
// variables, cached for ttl so a frequent local_shell does not read the database
// per command while a Beam/CLI edit still takes effect within the TTL. A read
// error keeps the last known value (and reports it), so a transient DB blip never
// strips the injected variables or breaks a spawning shell.
//
// The failed read is TELEMETRY, not a message: this getter runs per shell spawn
// with no operator watching and no command to speak in the voice of, the
// degradation is silent and self-healing on the next TTL refresh, and there is
// nothing for anyone to do about a blip that already recovered. nil tracker
// degrades to Noop.
func newLiveGlobalShellEnv(svc shellenvservice.Service, ttl time.Duration, tracker libtracker.ActivityTracker) func() map[string]string {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	var (
		mu     sync.Mutex
		at     time.Time
		val    map[string]string
		loaded bool
	)
	return func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		if loaded && time.Since(at) < ttl {
			return val
		}
		ctx := context.Background()
		vars, err := svc.Get(ctx)
		if err != nil {
			reportErr, _, end := tracker.Start(ctx, "read", "global_shell_env")
			reportErr(err)
			end()
			return val
		}
		val, at, loaded = vars, time.Now(), true
		return val
	}
}

// resolveScrubMode returns raw when it names a recognized posture, else def. An
// empty value takes the default silently (unset is normal); a NON-empty but
// unrecognized value is a typo, so it is named on warnW and falls back to the
// default — failing closed to a safe posture rather than silently disabling the
// scrub.
//
// Named, not tracked. The value came from the operator's own environment and is
// being ignored; the only person who can correct SANDBOX_SHELL_SCRUB=deny-secrit
// is the one who typed it, and the one command that reaches this code
// (`contenox sandbox env`) exists precisely to show them the effective posture —
// so a typo that silently downgrades to the default is the exact thing it must
// not hide in a log. nil warnW silences it.
func resolveScrubMode(raw, def string, warnW io.Writer) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	if !libsandbox.ScrubModeValid(raw) {
		if warnW != nil {
			fmt.Fprintf(warnW, "warning: %q is not a sandbox scrub mode; using %q instead — fix the SANDBOX_*_SCRUB value or unset it.\n", raw, def)
		}
		return def
	}
	return raw
}
