//go:build !windows

package shellsession

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestPTY_DoesNotEchoTheSubmittedLine is the direct check for the echo bug: the
// PTY's line discipline repeated every submitted line back at us, so `!echo AAA`
// showed the command text AND its output, doubling the transcript and making the
// agent pay tokens for reading its own input back.
//
// The assertion is on the literal command text rather than a line count because
// that is what distinguishes echo from output: "AAA" is the result either way,
// "echo AAA" only appears if the terminal repeated it.
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

// TestPTY_CatRoundTripDeliversInputExactlyOnce is the stronger form of the same
// claim, and the one that proves the fix did not simply break stdin: `cat`
// copies whatever it reads straight back out, so a marker line typed at a
// running cat MUST appear exactly once. Twice means the terminal is still
// echoing; zero times means input stopped reaching the foreground process.
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

// TestPTY_SuppressesAnRcFileProvidedPrompt pins the prompt fix against the case
// that actually breaks: clearing PS1 in the environment is NOT enough, because
// an interactive bash sources the user's rc file after importing the
// environment and the stock rc assigns PS1 unconditionally. The rc prompt is a
// privacy leak (login name, hostname, absolute cwd) in a scrollback the agent
// reads and the user shares.
//
// The test gives bash a HOME of its own with an rc that sets a distinctive
// prompt and an alias, so it asserts both halves of the deal at once: the
// prompt is gone, and the shell is still interactive enough to have sourced the
// rc (the alias resolves).
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

// TestPromptSuppressionEnv_ShapePerShell pins the mechanism itself, which the
// behavioural tests above cannot: for bash the assignment MUST ride on
// PROMPT_COMMAND, because that is the only hook that runs after the rc file and
// before the prompt is drawn. A refactor that "simplifies" this back to a plain
// PS1= would pass a naive env test and silently restore the leak.
func TestPromptSuppressionEnv_ShapePerShell(t *testing.T) {
	env := func(shell string) map[string]string {
		out := map[string]string{}
		for _, kv := range promptSuppressionEnv(shell) {
			k, v, _ := strings.Cut(kv, "=")
			out[k] = v
		}
		return out
	}

	bash := env("/usr/local/bin/bash") // non-standard path: classified by base name
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

// TestShellSpawnArgs_KeepsInteractiveDropsLineEditor pins the argv contract:
// -i stays (rc files and therefore aliases depend on it), and bash loses
// readline, which is what removes the bracketed-paste escape noise around every
// prompt. beam submits whole lines, so there is no keystroke for a line editor
// to edit.
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

// withHome returns a ScrubEnv that redirects HOME, so a test can hand the shell
// an rc file of its own instead of depending on whatever the developer's
// machine has in ~/.bashrc.
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

// TestPTY_SingleCommandProducesOnlyItsOutput is the acceptance test for the
// whole `!` experience: `!echo AAA` must put AAA in the scrollback and nothing
// else. Before the fix this one line produced the echoed command, two prompts
// carrying user@host:cwd, and the bracketed-paste escapes around them.
//
// HOME is redirected so the assertion is about beam's spawn contract rather
// than about whatever the developer's own .bashrc does.
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
