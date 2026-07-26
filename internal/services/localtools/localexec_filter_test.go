package localtools_test

// Integration tests for S8 (pando F2-G1) through LocalExecTools.Exec: the
// wedge-class transcript (a verbose `go test -json` run) is measurably
// compressed with zero failure lines lost; the raw stream is always preserved
// in the spool with a notice naming filter and spool path; stderr and the
// exit code are structurally untouchable; filtering happens BEFORE the inline
// size cap so compression buys headroom; the kill switch works end to end.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/stretchr/testify/require"
)

// scriptRunner plays back a canned stdout/stderr and exit code.
type scriptRunner struct {
	stdout string
	stderr string
	exit   int
}

func (r scriptRunner) Run(_ context.Context, _ localtools.CommandSpec, stdout, stderr io.Writer) (int, error) {
	_, _ = io.WriteString(stdout, r.stdout)
	_, _ = io.WriteString(stderr, r.stderr)
	return r.exit, nil
}

// goTestJSONTranscript fabricates a realistic verbose `go test -json` stream:
// `passing` chatty passing tests plus two failing tests. It returns the raw
// transcript and the failure lines that must survive filtering verbatim.
func goTestJSONTranscript(passing int) (string, []string) {
	var b strings.Builder
	ev := func(action, test, output string) {
		if output != "" {
			fmt.Fprintf(&b, `{"Time":"2026-07-27T10:00:00Z","Action":%q,"Package":"example.com/pkg","Test":%q,"Output":%q}`+"\n", action, test, output)
			return
		}
		fmt.Fprintf(&b, `{"Time":"2026-07-27T10:00:00Z","Action":%q,"Package":"example.com/pkg","Test":%q,"Elapsed":0.01}`+"\n", action, test)
	}
	for i := 0; i < passing; i++ {
		name := fmt.Sprintf("TestPass%03d", i)
		ev("run", name, "")
		ev("output", name, "=== RUN   "+name+"\n")
		for j := 0; j < 6; j++ {
			ev("output", name, fmt.Sprintf("    pass_test.go:%d: verbose passing log line %d for %s\n", 10+j, j, name))
		}
		ev("output", name, "--- PASS: "+name+" (0.01s)\n")
		ev("pass", name, "")
	}
	failureLines := []string{
		"--- FAIL: TestBrokenAlpha (0.02s)",
		"    alpha_test.go:41: expected 3 widgets, got 2",
		"    alpha_test.go:44: state dump: {ready:false}",
		"--- FAIL: TestBrokenBeta (0.01s)",
		"    beta_test.go:77: unexpected error: boom",
	}
	ev("run", "TestBrokenAlpha", "")
	ev("output", "TestBrokenAlpha", "=== RUN   TestBrokenAlpha\n")
	ev("output", "TestBrokenAlpha", failureLines[0]+"\n")
	ev("output", "TestBrokenAlpha", failureLines[1]+"\n")
	ev("output", "TestBrokenAlpha", failureLines[2]+"\n")
	ev("fail", "TestBrokenAlpha", "")
	ev("run", "TestBrokenBeta", "")
	ev("output", "TestBrokenBeta", "=== RUN   TestBrokenBeta\n")
	ev("output", "TestBrokenBeta", failureLines[3]+"\n")
	ev("output", "TestBrokenBeta", failureLines[4]+"\n")
	ev("fail", "TestBrokenBeta", "")
	b.WriteString(`{"Time":"2026-07-27T10:00:01Z","Action":"output","Package":"example.com/pkg","Output":"FAIL\texample.com/pkg\t0.31s\n"}` + "\n")
	b.WriteString(`{"Time":"2026-07-27T10:00:01Z","Action":"fail","Package":"example.com/pkg","Elapsed":0.31}` + "\n")
	return b.String(), failureLines
}

// filterTestTools builds LocalExecTools around a canned runner with a hermetic
// engine (builtin defaults only, no host config discovery) and a hermetic
// spool dir.
func filterTestTools(t *testing.T, runner localtools.CommandRunner, opts ...localtools.LocalExecOption) *localtools.LocalExecTools {
	t.Helper()
	t.Setenv("CONTENOX_TOOL_OUTPUT_DIR", t.TempDir())
	engine := localtools.NewOutputFilterEngine(nil, localtools.WithFilterSources())
	all := append([]localtools.LocalExecOption{localtools.WithLocalExecOutputFilter(engine)}, opts...)
	return localtools.NewLocalExecToolsWith(runner, all...).(*localtools.LocalExecTools)
}

func execShell(t *testing.T, h *localtools.LocalExecTools, ctx context.Context, command, args string) *localtools.LocalExecResult {
	t.Helper()
	call := &taskengine.ToolsCall{Name: "local_shell", Args: map[string]string{"command": command}}
	if args != "" {
		call.Args["args"] = args
	}
	out, dt, err := h.Exec(ctx, time.Now().UTC(), nil, false, call)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeJSON, dt)
	return out.(*localtools.LocalExecResult)
}

// The S8 gate: a realistic verbose transcript is measurably compressed with
// ZERO failure lines lost, the raw stream is preserved in the spool, and the
// result metadata names the filter.
func TestUnit_LocalShell_Filter_GoTestTranscriptCompressedLosslessly(t *testing.T) {
	raw, failureLines := goTestJSONTranscript(60)
	h := filterTestTools(t, scriptRunner{stdout: raw, exit: 1})

	res := execShell(t, h, context.Background(), "go", "test -json ./...")

	// Zero failure lines lost.
	for _, line := range failureLines {
		require.Contains(t, res.Stdout, line, "failure line must survive filtering verbatim")
	}
	require.Contains(t, res.Stdout, "go test: 60 passed, 2 failed", "the tally survives")

	// Measurably compressed: the passing bodies are gone.
	require.NotContains(t, res.Stdout, "verbose passing log line")
	require.Less(t, len(res.Stdout), len(raw)/10,
		"compression must be measured: filtered %d bytes vs raw %d bytes", len(res.Stdout), len(raw))

	// Metadata surfaces the applied filter; the notice names filter and spool.
	require.Equal(t, "go-test-json", res.Filter)
	require.Contains(t, res.FilterNotice, `"go-test-json"`)
	require.Contains(t, res.FilterNotice, "raw output preserved: ")

	// The raw stream is byte-identical in the spool.
	spoolPath := strings.TrimSpace(res.FilterNotice[strings.Index(res.FilterNotice, "raw output preserved: ")+len("raw output preserved: "):])
	full, err := os.ReadFile(spoolPath)
	require.NoError(t, err)
	require.Equal(t, raw, string(full), "the spool must hold the untouched raw stream")

	// Failure posture is assembled AFTER filtering and is untouched.
	require.False(t, res.Success)
	require.Equal(t, 1, res.ExitCode)

	t.Logf("S8 gate: raw=%d bytes filtered=%d bytes (%.1f%% of raw)", len(raw), len(res.Stdout), 100*float64(len(res.Stdout))/float64(len(raw)))
}

// Only stdout is filtered: stderr and exit code pass through untouched even
// when the filter rewrote stdout aggressively.
func TestUnit_LocalShell_Filter_StderrAndExitCodeUntouchable(t *testing.T) {
	stderrText := "npm WARN this stderr line would match the npm drop/collapse rules\nnpm ERR! but stderr is untouchable"
	h := filterTestTools(t, scriptRunner{stdout: "added 120 packages in 3s\naudited 200 packages in 1s", stderr: stderrText, exit: 0})

	res := execShell(t, h, context.Background(), "npm", "install")

	require.Equal(t, "npm-install", res.Filter)
	require.Contains(t, res.Stdout, "npm install completed successfully", "stdout collapsed by the builtin filter")
	require.Equal(t, stderrText, res.Stderr, "stderr must be byte-identical")
	require.True(t, res.Success)
	require.Equal(t, 0, res.ExitCode)

	// Raw stdout preserved in the spool even though it fit the inline budget.
	require.Contains(t, res.FilterNotice, "raw output preserved: ")
}

// Filtering happens BEFORE the inline size cap: a stream that would have been
// truncated fits after filtering, so the truncation posture (success=false,
// exit=-1) never triggers — compression bought headroom.
func TestUnit_LocalShell_Filter_CompressionBuysHeadroom(t *testing.T) {
	raw, failureLines := goTestJSONTranscript(300)
	h := filterTestTools(t, scriptRunner{stdout: raw, exit: 1})

	// A budget far below the raw size but comfortably above the filtered size.
	require.Greater(t, len(raw), 64*1024)
	ctx := context.WithValue(context.Background(), taskengine.ContextKeyOutputByteLimit, int64(16*1024))

	res := execShell(t, h, ctx, "go", "test -json ./...")

	require.Equal(t, "go-test-json", res.Filter)
	require.Equal(t, 1, res.ExitCode, "the real exit code survives — no truncation posture")
	require.NotContains(t, res.Error, "Output truncated")
	for _, line := range failureLines {
		require.Contains(t, res.Stdout, line)
	}
}

// The kill switch works through the whole stack: a config that disables the
// engine yields raw passthrough with no filter metadata.
func TestUnit_LocalShell_Filter_KillSwitch(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "filters.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{"disabled": true}`), 0o644))

	t.Setenv("CONTENOX_TOOL_OUTPUT_DIR", t.TempDir())
	engine := localtools.NewOutputFilterEngine(nil, localtools.WithFilterSources(cfgPath))
	h := localtools.NewLocalExecToolsWith(
		scriptRunner{stdout: "added 3 packages in 1s", exit: 0},
		localtools.WithLocalExecOutputFilter(engine),
	).(*localtools.LocalExecTools)

	res := execShell(t, h, context.Background(), "npm", "install")
	require.Empty(t, res.Filter)
	require.Empty(t, res.FilterNotice)
	require.Equal(t, "added 3 packages in 1s", res.Stdout)
}

// Explicitly disabling the engine (nil) is raw passthrough.
func TestUnit_LocalShell_Filter_NilEngineIsRaw(t *testing.T) {
	t.Setenv("CONTENOX_TOOL_OUTPUT_DIR", t.TempDir())
	h := localtools.NewLocalExecToolsWith(
		scriptRunner{stdout: "added 3 packages in 1s", exit: 0},
		localtools.WithLocalExecOutputFilter(nil),
	).(*localtools.LocalExecTools)

	res := execShell(t, h, context.Background(), "npm", "install")
	require.Empty(t, res.Filter)
	require.Equal(t, "added 3 packages in 1s", res.Stdout)
}

// An unmatched command through a live engine keeps today's behavior exactly.
func TestUnit_LocalShell_Filter_NoMatchIsRaw(t *testing.T) {
	h := filterTestTools(t, scriptRunner{stdout: "plain output", exit: 0})
	res := execShell(t, h, context.Background(), "some-tool", "--flag")
	require.Empty(t, res.Filter)
	require.Equal(t, "plain output", res.Stdout)
	require.True(t, res.Success)
}
