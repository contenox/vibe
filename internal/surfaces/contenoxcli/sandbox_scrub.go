package contenoxcli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/libsandbox"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/services/shellenvservice"
)

// shellEnvCacheTTL bounds how stale a shellenvservice read can be; a Beam/CLI
// edit lands within this window with no restart.
const shellEnvCacheTTL = 3 * time.Second

// resolvedSandboxEnv is the one composition every agent-shell spawn root and
// the `sandbox env` preview share, so the preview never drifts from what a
// spawned shell actually receives. db may be a nil interface in tests that
// never invoke the returned hooks.
func resolvedSandboxEnv(db libdb.DBManager, tracker libtracker.ActivityTracker, warnW io.Writer) (shell, terminal func([]string) []string, err error) {
	config := &sandboxEnvConfig{}
	if err := loadEnvConfig(config); err != nil {
		return nil, nil, fmt.Errorf("load sandbox env config: %w", err)
	}
	injectGlobal := newLiveGlobalShellEnv(shellenvservice.New(db), shellEnvCacheTTL, tracker)
	shell, terminal = resolveSandboxScrubs(config, injectGlobal, warnW)
	return shell, terminal, nil
}

// resolveSandboxScrubs turns SANDBOX_* config into the env-scrub hooks for
// agent-reachable shells and the interactive terminal panel; a nil hook means
// inherit everything. Agent shells default to deny-secrets, the terminal to
// off. warnW receives one line per misspelled SANDBOX_*_SCRUB value; nil silences it.
func resolveSandboxScrubs(config *sandboxEnvConfig, injectGlobal func() map[string]string, warnW io.Writer) (shell, terminal func([]string) []string) {
	extraAllow := libsandbox.ParseEnvList(config.SandboxEnvAllow)
	extraDeny := libsandbox.ParseEnvList(config.SandboxEnvDeny)
	shellFilter := libsandbox.EnvScrub(resolveScrubMode(config.SandboxShellScrub, libsandbox.ScrubDenySecrets, warnW), extraAllow, extraDeny)
	terminalFilter := libsandbox.EnvScrub(resolveScrubMode(config.SandboxTerminalScrub, libsandbox.ScrubOff, warnW), extraAllow, extraDeny)
	return composeShellEnv(shellFilter, injectGlobal), composeShellEnv(terminalFilter, injectGlobal)
}

// composeShellEnv runs the scrub filter, if any, then overlays the operator's
// global shell-env variables so they win even when the scrub is off. Returns
// nil when both are absent, preserving full inherit when nothing is configured.
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
// variables, cached for ttl so a frequent local_shell does not hit the
// database per command. A read error keeps the last known value and reports
// it via tracker (telemetry only); nil tracker degrades to Noop.
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

// resolveScrubMode returns raw when it names a recognized posture, else def.
// An empty value takes the default silently; a non-empty but unrecognized
// value is a typo, so it is warned on warnW and falls back to def rather than
// silently disabling the scrub. nil warnW silences the warning.
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
