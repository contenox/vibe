package contenoxcli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

// chainAgentChatFilename is what the preseeded chat.md declaration compiles to.
const chainAgentChatFilename = "chain-agent-chat.json"

const maxChatLineBytes = 1 << 20

var chatCmd = &cobra.Command{
	Use:   "chat [message]",
	Short: "Hold a conversation in the terminal, in a session that remembers it.",
	Long: `Talk to a declared agent as a person, with nothing drawn on the screen.

A line goes in and an answer comes out. There is no interface to redraw, no
alternate screen and no key bindings to learn, so the conversation lands in your
terminal's own scrollback and copies out of it like any other text.

The conversation is held in the ACTIVE session, so it survives this command:
what you asked yesterday is still context today. Keep separate threads apart
with 'contenox session new <name>' and 'contenox session switch <name>', and
read one back with 'contenox session show'.

With no message it reads from the terminal until you end it with ctrl-D. With a
message it takes that one turn and exits, which is what a shell alias or a
keybinding wants. -e composes the message in $VISUAL or $EDITOR instead of at
the prompt, which is how a long question gets written without the terminal
eating a newline; piped stdin is preloaded as reference.

The chat agent HAS NO TOOLS. It cannot open a file, run a command or reach the
network, and its declaration says so, so it asks rather than pretending. For
work on this machine use 'contenox beam' (an agent with tools, under your
approval) or 'contenox run "<task>"' (one task, carried out, reported).

Examples:
  contenox chat                                # the conversation, until ctrl-D
  contenox chat "what does a mission envelope bound?"
  contenox chat -e                             # compose in $EDITOR
  contenox session new review && contenox chat # a thread of its own`,
	Args: cobra.MaximumNArgs(1),
	RunE: runChat,
}

func init() {
	rootCmd.AddCommand(chatCmd)
}

func runChat(cmd *cobra.Command, args []string) error {
	out, errW := cmd.OutOrStdout(), cmd.ErrOrStderr()

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = libtracker.WithNewRequestID(ctx)

	contenoxDir, err := ResolveContenoxDir(cmd)
	if err != nil {
		return fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return err
	}
	db, err := OpenDBAt(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open database %q: %w", dbPath, err)
	}
	defer db.Close()

	opts, err := buildRunOpts(cmd, db, contenoxDir)
	if err != nil {
		return err
	}
	opts.EffectiveDB = dbPath

	// Before the engine: an editor compose the operator abandons must not have
	// paid for a backend cycle first.
	opening, oneShot, err := chatOpening(cmd, args, opts.EffectiveDefaultModel)
	if err != nil {
		if errors.Is(err, errEmptyPrompt) {
			fmt.Fprintln(errW, "Nothing to send.")
			return errPromptAborted
		}
		return err
	}

	engine, err := BuildEngine(ctx, db, opts)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}
	defer engine.Stop()
	if err := PreflightLLMSetup(errW, engine.SetupCheck); err != nil {
		return err
	}

	chain, err := resolveChatChain(ctx, cmd, contenoxDir)
	if err != nil {
		return err
	}

	workspaceID := ResolveWorkspaceID(contenoxDir)
	sessionID, err := ensureDefaultSession(ctx, db, workspaceID)
	if err != nil {
		fmt.Fprintf(errW, "warning: no session could be resolved — this conversation will not be remembered: %v\n", err)
		sessionID = ""
	}

	agentsMD, agentsMDSource := loadAgentsMDFromCwd()
	raw, _ := cmd.Flags().GetBool("raw")
	ag := agentservice.New(agentservice.Deps{Engine: engine, DB: db, WorkspaceID: workspaceID})

	turn := func(input string) error {
		if !opts.EffectiveTracing {
			fmt.Fprintln(errW, "Thinking...")
		}
		resp, err := ag.Prompt(ctx, agentservice.PromptRequest{
			SessionID:      sessionID,
			Input:          input,
			Chain:          chain,
			TemplateVars:   buildTemplateVars(opts),
			ContextLength:  opts.EffectiveContext,
			HistoryTrim:    opts.HistoryTrim,
			AgentsMD:       agentsMD,
			AgentsMDSource: agentsMDSource,
		})
		if err != nil {
			if isModelResolverFailure(err) {
				PrintSetupIssues(errW, engine.SetupCheck)
			}
			return err
		}
		if resp.StopReason == agentservice.StopSuspended {
			fmt.Fprintf(errW, "This turn is parked on approval %s — answer it with 'contenox approvals'.\n", resp.SuspendedApprovalID)
		}
		printRelevantOutput(out, resp.Output, resp.OutputType, raw)
		return nil
	}

	if oneShot {
		return turn(opening)
	}
	return chatLoop(cmd.InOrStdin(), errW, turn)
}

// chatLoop is the whole interface: a marker on stderr so a redirected stdout
// carries only the answers, and ctrl-D to stop.
func chatLoop(in io.Reader, errW io.Writer, turn func(string) error) error {
	fmt.Fprintln(errW, "Type a message and press enter. ctrl-D ends the conversation; the session keeps it.")
	sc := bufio.NewScanner(in)
	sc.Buffer(nil, maxChatLineBytes)
	for {
		fmt.Fprint(errW, "> ")
		if !sc.Scan() {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if err := turn(line); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read from the terminal: %w", err)
	}
	fmt.Fprintln(errW)
	return nil
}

// chatOpening resolves the turn this invocation carries; oneShot false means
// there is none and the conversation is driven from the terminal instead.
func chatOpening(cmd *cobra.Command, args []string, modelHint string) (opening string, oneShot bool, err error) {
	typed := ""
	if len(args) > 0 {
		typed = strings.TrimSpace(args[0])
	}
	body, piped := pipedStdin()
	seed := chatSeed(typed, body, piped)

	if compose, _ := cmd.Flags().GetBool("editor"); compose {
		message, err := captureFromEditor([]byte(seed), modelHint)
		if err != nil {
			return "", false, err
		}
		if strings.TrimSpace(message) == "" {
			return "", false, errEmptyPrompt
		}
		return strings.TrimSpace(message), true, nil
	}
	if seed == "" {
		return "", false, nil
	}
	return seed, true, nil
}

func chatSeed(typed, body string, piped bool) string {
	switch {
	case !piped:
		return typed
	case typed == "":
		return strings.TrimSpace(body)
	default:
		return attachPipedStdin(typed, body)
	}
}

func resolveChatChain(ctx context.Context, cmd *cobra.Command, contenoxDir string) (*taskengine.TaskChainDefinition, error) {
	if named, _ := cmd.Root().PersistentFlags().GetString("chain"); strings.TrimSpace(named) != "" {
		path, err := filepath.Abs(named)
		if err != nil {
			return nil, fmt.Errorf("invalid --chain path: %w", err)
		}
		return loadChainFromFile(path)
	}
	if err := ensureProfileChain(ctx, contenoxDir, chainAgentChatFilename, "", libtracker.NoopTracker{}); err != nil {
		return nil, err
	}
	// The chat surface names no envelope, so it runs under the one the evaluator
	// falls back to; rendered here for the same reason the chain is.
	if err := ensureProfilePolicy(ctx, contenoxDir, chatProfileEnvelope, false, libtracker.NoopTracker{}); err != nil {
		return nil, err
	}
	path, err := lookupSystemFile(contenoxDir, chainAgentChatFilename)
	if err != nil {
		return nil, err
	}
	return loadChainFromFile(path)
}
