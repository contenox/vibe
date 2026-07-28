//go:build !windows

package shellsession

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/beam/internal/libsandbox"
)

// requireBash skips a test that needs the real bash semantics (rc files,
// PROMPT_COMMAND) on a machine that does not have it.
func requireBash(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/bin/bash", "/usr/bin/bash"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("bash not installed; the prompt/alias behaviour under test is bash-specific")
	return ""
}

// TestPTY_DoesNotEchoTheSubmittedLine pins that the submitted command text never appears in the scrollback, only its output.
func TestPTY_DoesNotEchoTheSubmittedLine(t *testing.T) {
	m := newTestManager(t, time.Minute)
	ctx := ctxWithSession("sess-echo")

	const cmd = "echo no-echo-marker"
	if _, err := m.Run(ctx, "sess-echo", cmd); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(m.Read("sess-echo", 0, 0).Content, "no-echo-marker")
	}) {
		t.Fatalf("the command never produced output")
	}
	got := m.Read("sess-echo", 0, 0).Content
	if strings.Contains(got, cmd) {
		t.Fatalf("the PTY echoed the submitted line back into the scrollback; want it absent, got %q", got)
	}
}

// TestPTY_CatRoundTripDeliversInputExactlyOnce pins that a marker typed at a running `cat` appears exactly once (not echoed, not lost).
func TestPTY_CatRoundTripDeliversInputExactlyOnce(t *testing.T) {
	m := newTestManager(t, time.Minute)
	ctx := ctxWithSession("sess-cat")

	if _, err := m.Run(ctx, "sess-cat", "cat"); err != nil {
		t.Fatalf("Run(cat): %v", err)
	}
	const marker = "round-trip-marker"
	if _, err := m.Run(ctx, "sess-cat", marker); err != nil {
		t.Fatalf("Run(marker): %v", err)
	}
	if !waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(m.Read("sess-cat", 0, 0).Content, marker)
	}) {
		t.Fatalf("cat never echoed the marker back: %q", m.Read("sess-cat", 0, 0).Content)
	}
	got := m.Read("sess-cat", 0, 0).Content
	if n := strings.Count(got, marker); n != 1 {
		t.Fatalf("marker must appear exactly once (cat's copy, not a terminal echo); got %d in %q", n, got)
	}
}

// TestPTY_SuppressesAnRcFileProvidedPrompt pins that an rc-assigned PS1 is suppressed while the rc file is still sourced (its alias resolves).
func TestPTY_SuppressesAnRcFileProvidedPrompt(t *testing.T) {
	bash := requireBash(t)

	home := t.TempDir()
	rc := "PS1='RC-PROMPT-LEAK> '\nalias rcalias='echo alias-works'\n"
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte(rc), 0o600); err != nil {
		t.Fatalf("write .bashrc: %v", err)
	}

	m := newTestManagerWith(t, Config{
		Shell:       bash,
		IdleTimeout: time.Minute,
		ScrubEnv:    withHome(home),
	})
	ctx := ctxWithSession("sess-prompt")

	if _, err := m.Run(ctx, "sess-prompt", "rcalias"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(m.Read("sess-prompt", 0, 0).Content, "alias-works")
	}) {
		t.Fatalf("the rc file was never sourced (alias did not resolve): %q", m.Read("sess-prompt", 0, 0).Content)
	}
	got := m.Read("sess-prompt", 0, 0).Content
	if strings.Contains(got, "RC-PROMPT-LEAK") {
		t.Fatalf("the rc file's prompt leaked into the scrollback: %q", got)
	}
}

// TestPromptSuppressionEnv_ShapePerShell pins that bash's clearing rides on PROMPT_COMMAND, not a plain PS1=, since PROMPT_COMMAND alone outruns the rc file.
func TestPromptSuppressionEnv_ShapePerShell(t *testing.T) {
	env := func(shell string) map[string]string {
		out := map[string]string{}
		for _, kv := range promptSuppressionEnv(shell) {
			k, v, _ := strings.Cut(kv, "=")
			out[k] = v
		}
		return out
	}

	bash := env("/usr/local/bin/bash") // non-standard path, classified by base name
	if bash["PS1"] != "" || bash["PS2"] != "" {
		t.Fatalf("bash: PS1/PS2 must be cleared, got %#v", bash)
	}
	pc, ok := bash["PROMPT_COMMAND"]
	if !ok {
		t.Fatalf("bash: PROMPT_COMMAND must be set; it is the only hook that outruns the rc file, got %#v", bash)
	}
	if !strings.Contains(pc, "PS1=") {
		t.Fatalf("bash: PROMPT_COMMAND must re-clear PS1 before every prompt, got %q", pc)
	}

	zsh := env("/bin/zsh")
	for _, k := range []string{"PS1", "PS2", "PROMPT", "RPROMPT"} {
		if v, ok := zsh[k]; !ok || v != "" {
			t.Fatalf("zsh: %s must be cleared (zsh draws from PROMPT/RPROMPT), got %#v", k, zsh)
		}
	}

	other := env("/bin/sh")
	for _, k := range []string{"PS1", "PS2"} {
		if v, ok := other[k]; !ok || v != "" {
			t.Fatalf("sh: %s must be cleared, got %#v", k, other)
		}
	}
}

// TestShellSpawnArgs_KeepsInteractiveDropsLineEditor pins that -i stays (rc/aliases depend on it) while bash loses readline.
func TestShellSpawnArgs_KeepsInteractiveDropsLineEditor(t *testing.T) {
	bash := strings.Join(shellSpawnArgs("/bin/bash"), " ")
	if !strings.Contains(bash, "-i") {
		t.Fatalf("bash must stay interactive so the user's aliases exist, got %q", bash)
	}
	if !strings.Contains(bash, "--noediting") {
		t.Fatalf("bash must run without readline, got %q", bash)
	}
	if zsh := strings.Join(shellSpawnArgs("/bin/zsh"), " "); zsh != "-i" {
		t.Fatalf("zsh must be interactive, got %q", zsh)
	}
	if other := shellSpawnArgs("/bin/sh"); len(other) != 0 {
		t.Fatalf("an unrecognized shell keeps its default argv, got %q", other)
	}
}

// withHome returns a ScrubEnv that redirects HOME to a test's own rc file.
func withHome(home string) func([]string) []string {
	return func(env []string) []string {
		out := make([]string, 0, len(env)+1)
		for _, kv := range env {
			if strings.HasPrefix(kv, "HOME=") {
				continue
			}
			out = append(out, kv)
		}
		return append(out, "HOME="+home)
	}
}

// TestPTY_SingleCommandProducesOnlyItsOutput pins that one submitted line yields exactly its own output, nothing else.
func TestPTY_SingleCommandProducesOnlyItsOutput(t *testing.T) {
	bash := requireBash(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("PS1='leak$ '\n"), 0o600); err != nil {
		t.Fatalf("write .bashrc: %v", err)
	}
	m := newTestManagerWith(t, Config{
		Shell:       bash,
		IdleTimeout: time.Minute,
		ScrubEnv:    withHome(home),
	})
	if _, err := m.Run(ctxWithSession("sess-clean"), "sess-clean", "echo AAA"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(m.Read("sess-clean", 0, 0).Content, "AAA")
	}) {
		t.Fatalf("no output")
	}
	if got := m.Read("sess-clean", 0, 0).Content; got != "AAA\r\n" {
		t.Fatalf("one command must yield exactly its own output; want %q, got %q", "AAA\r\n", got)
	}
}

// TestPTY_ScrubEnv_StripsSecretKeepsPathAndHome pins that the default deny-secrets scrub strips credential-shaped vars while PATH/HOME survive.
func TestPTY_ScrubEnv_StripsSecretKeepsPathAndHome(t *testing.T) {
	t.Setenv("TESTSECRET_API_KEY", "leaked-value")

	scrub := libsandbox.EnvScrub(libsandbox.ScrubDenySecrets, nil, nil)
	m := newTestManagerWith(t, Config{
		IdleTimeout: time.Minute,
		ScrubEnv:    scrub,
	})
	ctx := ctxWithSession("sess-scrub")
	if _, err := m.Run(ctx, "sess-scrub", "env"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(m.Read("sess-scrub", 0, 0).Content, "PATH=")
	}) {
		t.Fatalf("no output: %q", m.Read("sess-scrub", 0, 0).Content)
	}
	got := m.Read("sess-scrub", 0, 0).Content
	if strings.Contains(got, "TESTSECRET_API_KEY") {
		t.Fatalf("the default scrub must strip credential-shaped names, got %q", got)
	}
	if !strings.Contains(got, "PATH=") {
		t.Fatalf("PATH must survive the default scrub or the shell cannot run anything, got %q", got)
	}
	if !strings.Contains(got, "HOME=") {
		t.Fatalf("HOME must survive the default scrub (Allow=\"*\" under deny-secrets), got %q", got)
	}
}
