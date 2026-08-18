package contenoxcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func newChatTestCmd(t *testing.T, editor bool) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "chat"}
	cmd.Flags().BoolP("editor", "e", editor, "")
	return cmd
}

// `chat` is the conversational shape and must be a real reserved subcommand: an
// unreserved name would be read as a task and fired at the run agent instead.
func TestUnit_ChatCommandIsWired(t *testing.T) {
	cmd := lookupCommand(t, "chat")
	require.True(t, reservedSubcommands["chat"])
	require.True(t, firstNonFlagIsReserved([]string{"chat", "what is a mission envelope?"}))
	require.False(t, cmd.Hidden)

	require.NotNil(t, cmd.InheritedFlags().Lookup("editor"),
		"chat composes with the root -e flag rather than declaring a second one")

	require.NoError(t, cmd.Args(cmd, []string{}), "no message means the terminal drives the conversation")
	require.NoError(t, cmd.Args(cmd, []string{"one message"}))
	require.Error(t, cmd.Args(cmd, []string{"one", "two"}),
		"an unquoted message must fail loudly rather than lose its words")
}

// The chain chat loads is the one the shipped declaration compiles to; the two
// are wired by filename alone, so a rename on either side is silent otherwise.
func TestUnit_ChatChain_IsWhatTheShippedDeclarationCompilesTo(t *testing.T) {
	dir := t.TempDir()
	_, err := agentdecl.Preseed(dir)
	require.NoError(t, err)

	cfg, err := agentdecl.Shipped()
	require.NoError(t, err)
	generated := filepath.Join(dir, agentdecl.GeneratedDirName)
	_, err = agentdecl.Sync(agentdecl.DiscoverSourceDirs([]string{dir}, nil), generated, cfg)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(generated, chainAgentChatFilename))
	require.NoError(t, err, "no shipped declaration compiles to %q", chainAgentChatFilename)
}

// With no message and no pipe there is nothing to send yet: the conversation is
// driven from the terminal instead of firing an empty turn.
func TestUnit_ChatOpening_NoMessageDrivesFromTheTerminal(t *testing.T) {
	pinPipedStdin(t, "")
	opening, oneShot, err := chatOpening(newChatTestCmd(t, false), nil, "some-model")
	require.NoError(t, err)
	require.False(t, oneShot)
	require.Empty(t, opening)
}

func TestUnit_ChatOpening_AMessageIsOneTurn(t *testing.T) {
	pinPipedStdin(t, "")
	opening, oneShot, err := chatOpening(newChatTestCmd(t, false), []string{"  what changed?  "}, "some-model")
	require.NoError(t, err)
	require.True(t, oneShot)
	require.Equal(t, "what changed?", opening)
}

// A pipe is a turn on its own, and a pipe with a message is that message about
// the piped body — delimited the same way `run` delimits it.
func TestUnit_ChatOpening_PipedStdinIsTheTurn(t *testing.T) {
	pinPipedStdin(t, "\ndiff --git a/x b/x\n")
	opening, oneShot, err := chatOpening(newChatTestCmd(t, false), nil, "some-model")
	require.NoError(t, err)
	require.True(t, oneShot)
	require.Equal(t, "diff --git a/x b/x", opening)

	opening, oneShot, err = chatOpening(newChatTestCmd(t, false), []string{"what does this touch?"}, "some-model")
	require.NoError(t, err)
	require.True(t, oneShot)
	require.Equal(t, attachPipedStdin("what does this touch?", "\ndiff --git a/x b/x\n"), opening)
}

// -e reaches the shared editor-compose helper, and an editor that writes
// nothing aborts instead of sending an empty turn to a model.
func TestUnit_ChatOpening_EditorComposeAbortsOnAnEmptyBuffer(t *testing.T) {
	pinPipedStdin(t, "")
	t.Setenv("VISUAL", "true")
	t.Setenv("EDITOR", "true")

	_, _, err := chatOpening(newChatTestCmd(t, true), nil, "some-model")
	require.ErrorIs(t, err, errEmptyPrompt)
}

// An unedited buffer keeps its seed, so -e over a message or a pipe sends what
// was already there rather than losing it.
func TestUnit_ChatOpening_EditorComposeKeepsAnUneditedSeed(t *testing.T) {
	pinPipedStdin(t, "")
	t.Setenv("VISUAL", "true")
	t.Setenv("EDITOR", "true")

	opening, oneShot, err := chatOpening(newChatTestCmd(t, true), []string{"what changed?"}, "some-model")
	require.NoError(t, err)
	require.True(t, oneShot)
	require.Equal(t, "what changed?", opening)
}

func TestUnit_ChatLoop_SkipsBlankLinesAndEndsAtEOF(t *testing.T) {
	var sent []string
	var errW strings.Builder
	err := chatLoop(strings.NewReader("first\n\n   \nsecond\n"), &errW, func(in string) error {
		sent = append(sent, in)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"first", "second"}, sent)
	require.Contains(t, errW.String(), "ctrl-D", "the only way out has to be on screen")
	// Four lines read, then the read that met EOF: every one of them prompted.
	require.Equal(t, 5, strings.Count(errW.String(), "> "))
}

func TestUnit_ChatLoop_StopsOnTheFirstFailedTurn(t *testing.T) {
	var sent []string
	var errW strings.Builder
	boom := errPromptAborted
	err := chatLoop(strings.NewReader("first\nsecond\n"), &errW, func(in string) error {
		sent = append(sent, in)
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.Equal(t, []string{"first"}, sent)
}
