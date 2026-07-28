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
