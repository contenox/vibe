package contenoxcli

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUnit_PromptChoiceOrQuit_EOFAborts asserts EOF on the gating choice returns promptEOF rather than a guessed default, while a real answer or "q" still resolves normally.
func TestUnit_PromptChoiceOrQuit_EOFAborts(t *testing.T) {
	var out bytes.Buffer

	eof := promptChoiceOrQuit(&out, bufio.NewScanner(strings.NewReader("")), "Provider", 3, true)
	require.Equal(t, promptEOF, eof, "EOF at the gating choice must abort, not select the first option")

	choice := promptChoiceOrQuit(&out, bufio.NewScanner(strings.NewReader("2\n")), "Provider", 3, true)
	require.Equal(t, 1, choice, "a real answer resolves to its zero-based index")

	quit := promptChoiceOrQuit(&out, bufio.NewScanner(strings.NewReader("q\n")), "Provider", 3, true)
	require.Equal(t, -1, quit, "an intentional quit stays distinct from EOF")
}

// TestUnit_PreselectOllamaModel asserts Enter never commits a model the daemon
// does not serve.
func TestUnit_PreselectOllamaModel(t *testing.T) {
	models := []string{"llama3.2:3b", "qwen3:8b"}
	require.Equal(t, "qwen3:8b", preselectOllamaModel(models, "qwen3:8b"))
	require.Equal(t, "llama3.2:3b", preselectOllamaModel(models, "gpt-oss:20b"),
		"a suggested default that is not pulled must not be preselected")
}

// TestUnit_ResolveOllamaModelChoice asserts the menu answer mapping: Enter keeps
// the preselected model, a number picks its entry, a typed id is honored, and an
// out-of-range number is rejected instead of silently committing something else.
func TestUnit_ResolveOllamaModelChoice(t *testing.T) {
	models := []string{"llama3.2:3b", "qwen3:8b"}

	got, ok := resolveOllamaModelChoice("", models, "qwen3:8b")
	require.True(t, ok)
	require.Equal(t, "qwen3:8b", got)

	got, ok = resolveOllamaModelChoice(" 1 ", models, "qwen3:8b")
	require.True(t, ok)
	require.Equal(t, "llama3.2:3b", got)

	got, ok = resolveOllamaModelChoice("mistral:7b", models, "qwen3:8b")
	require.True(t, ok)
	require.Equal(t, "mistral:7b", got, "a model the probe could not classify stays reachable")

	_, ok = resolveOllamaModelChoice("9", models, "qwen3:8b")
	require.False(t, ok, "an out-of-range number must be re-asked, not resolved to the default")
}

// TestUnit_PromptOllamaModelMenu asserts the menu lists what is pulled, marks
// the preselected entry, and accepts Enter, a number, and a retry after a bad
// number.
func TestUnit_PromptOllamaModelMenu(t *testing.T) {
	models := []string{"llama3.2:3b", "qwen3:8b"}

	var out bytes.Buffer
	got := promptOllamaModelMenu(&out, bufio.NewScanner(strings.NewReader("\n")), models, "qwen3:8b")
	require.Equal(t, "qwen3:8b", got)
	require.Contains(t, out.String(), "1. llama3.2:3b")
	require.Contains(t, out.String(), "2. qwen3:8b  (default)")
	require.Contains(t, out.String(), "Model [2]")

	out.Reset()
	got = promptOllamaModelMenu(&out, bufio.NewScanner(strings.NewReader("1\n")), models, "qwen3:8b")
	require.Equal(t, "llama3.2:3b", got)

	out.Reset()
	got = promptOllamaModelMenu(&out, bufio.NewScanner(strings.NewReader("7\n2\n")), models, "llama3.2:3b")
	require.Equal(t, "qwen3:8b", got)
	require.Contains(t, out.String(), "Please enter a number between 1 and 2")
}

// TestUnit_PrintSetupNextCommand asserts the wizard ends by naming a command
// the operator can actually run next.
func TestUnit_PrintSetupNextCommand(t *testing.T) {
	var tty bytes.Buffer
	printSetupNextCommand(&tty, true)
	require.Contains(t, tty.String(), `contenox acp`)
	require.NotContains(t, tty.String(), "Close this tab")

	var piped bytes.Buffer
	printSetupNextCommand(&piped, false)
	require.Contains(t, piped.String(), `contenox acp`)
}
