package contenoxcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/spf13/cobra"
)

// runCmd runs any task chain with any input type.
// Unlike 'contenox chat' (which hardcodes DataTypeChatHistory), 'contenox run'
// lets the caller specify the input type and is fully stateless (no chat history).
var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run any task chain with explicit input type control (stateless).",
	Long: `Run a task chain with explicit control over input type and content.

Unlike 'contenox chat', run is stateless — no chat history is loaded or saved.
It accepts any task chain regardless of the first handler's expected input type.

Input sources (in priority order):
  1. --input <value>         literal string (or @file to read from a file)
  2. Positional arguments    joined with a space
  3. Stdin                   if piped

Input types (--input-type):
  string (default)  Raw string passed to the chain as DataTypeString
  chat              Wrapped as a single user message (DataTypeChatHistory)
  json              Parsed as a JSON object (DataTypeJSON)
  int               Parsed as integer (DataTypeInt)

If --chain is not specified, falls back to .contenox/default-run-chain.json
if that file exists in the current directory.

Examples:
  contenox run --chain .contenox/score-chain.json "is this code safe?"
  cat diff.txt | contenox run --chain .contenox/review.json --input-type chat
  contenox run --chain .contenox/embed.json --input @myfile.go
  contenox run --chain .contenox/parse-chain.json --input-type json '{"key":"value"}'
  git diff | contenox run "suggest a commit message"  # uses default-run-chain.json

  # HITL is on by default (write_file, sed, and local_shell prompt for approval).
  # Run unattended (no prompts) by passing --auto:
  contenox run --shell --auto --chain .contenox/my-chain.json "fix the bug"
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}

		flags := cmd.Flags()

		// Resolve .contenox dir using Git-style parent walk.
		contenoxDir, err := ResolveContenoxDir(cmd)
		if err != nil {
			return fmt.Errorf("failed to resolve .contenox dir: %w", err)
		}

		chainPath, _ := flags.GetString("chain")
		if chainPath == "" && !flags.Changed("chain") {
			if resolved, rerr := lookupSystemFile(contenoxDir, "default-run-chain.json"); rerr == nil {
				chainPath = resolved
			}
		}
		if chainPath == "" {
			fmt.Fprintln(os.Stderr, "No default-run-chain.json found in .contenox/ (workspace) or ~/.contenox/.")
			fmt.Fprintln(os.Stderr, "Run 'contenox init' to scaffold it, or pass --chain explicitly.")
			return errChainRequired
		}

		// Resolve input
		rawInput, err := resolveRunInput(cmd, args)
		if err != nil {
			if errors.Is(err, errEmptyPrompt) {
				fmt.Fprintln(cmd.ErrOrStderr(), "aborted due to empty prompt")
				return errPromptAborted
			}
			return err
		}
		if rawInput == "" {
			return fmt.Errorf(
				"no input provided\n" +
					"  Pass input as positional args, --input, pipe via stdin, or use --input @file.txt",
			)
		}

		// Resolve input type
		inputTypeName, _ := flags.GetString("input-type")
		if !flags.Changed("input-type") && !flags.Changed("chain") {
			inputTypeName = "string"
		}
		inputVal, inputType, err := parseRunInput(rawInput, inputTypeName)
		if err != nil {
			return fmt.Errorf("--input-type %q: %w", inputTypeName, err)
		}

		// Open database (needed for buildRunOpts KV read and engine).
		dbPathAbs, err := resolveDBPath(cmd)
		if err != nil {
			return fmt.Errorf("invalid database path: %w", err)
		}
		dbCtx := libtracker.WithNewRequestID(context.Background())
		db, err := OpenDBAt(dbCtx, dbPathAbs)
		if err != nil {
			return fmt.Errorf("failed to open database: %w", err)
		}
		defer db.Close()

		closeLogs, err := setupTelemetryLogging(dbCtx, runtimetypes.New(db.WithoutTransaction()), contenoxDir)
		if err != nil {
			warnTelemetryLoggingUnavailable(cmd.ErrOrStderr(), err)
		}
		defer closeLogs()

		// Build chatOpts from flags and SQLite KV defaults.
		o, err := buildRunOpts(cmd, db, contenoxDir)
		if err != nil {
			return err
		}
		o.EffectiveDB = dbPathAbs
		engine, err := BuildEngine(ctx, db, o)
		if err != nil {
			return fmt.Errorf("failed to build engine: %w", err)
		}
		defer engine.Stop()

		if err := PreflightLLMSetup(cmd.ErrOrStderr(), engine.SetupCheck); err != nil {
			return err
		}

		// Resolve chain path
		chainPathAbs, err := filepath.Abs(chainPath)
		if err != nil {
			return fmt.Errorf("invalid chain path: %w", err)
		}
		chain, err := loadChainFromFile(chainPathAbs)
		if err != nil {
			return err
		}

		// Template vars
		templateVars := buildTemplateVars(o)

		// Set timeout
		timeout, _ := flags.GetDuration("timeout")
		execCtx := libtracker.WithNewRequestID(ctx)
		timeoutCtx, timeoutCancel := context.WithTimeout(execCtx, timeout)
		defer timeoutCancel()

		// Use signal.NotifyContext so the goroutine is cleaned up automatically
		// when the command returns, instead of leaking a blocked goroutine.
		execCtx, stop := signal.NotifyContext(timeoutCtx, syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		// engine.Tracker is log-backed exactly when --trace is on, Noop otherwise.
		_, _, chainEnd := engine.Tracker.Start(execCtx, "execute", "chain",
			"chain", chainPathAbs, "input_type", inputTypeName)
		defer chainEnd()
		if !o.EffectiveTracing {
			fmt.Fprintln(cmd.ErrOrStderr(), "Thinking...")
		}

		stopTrace := startTraceStream(execCtx, o, engine, cmd.ErrOrStderr())
		defer stopTrace()
		stopPrint := startPrintStream(execCtx, engine, cmd.ErrOrStderr())
		defer stopPrint()
		stopThoughts := startThoughtStream(execCtx, engine, cmd.ErrOrStderr(), o.EffectiveThink)
		defer stopThoughts()

		// Create agent and execute via service layer (stateless — no session).
		workspaceID := ResolveWorkspaceID(o.ContenoxDir)
		ag := agentservice.New(agentservice.Deps{
			Engine:      engine,
			DB:          db,
			WorkspaceID: workspaceID,
		})

		resp, err := ag.Prompt(execCtx, agentservice.PromptRequest{
			Input:         rawInput,
			InputValue:    inputVal,
			InputType:     inputType,
			Chain:         chain,
			TemplateVars:  templateVars,
			ContextLength: o.EffectiveContext,
		})
		if err != nil {
			if isModelResolverFailure(err) {
				PrintSetupIssues(cmd.ErrOrStderr(), engine.SetupCheck)
			}
			return fmt.Errorf("chain execution failed: %w", err)
		}

		effectiveRaw, _ := flags.GetBool("raw")
		effectiveSteps, _ := flags.GetBool("steps")
		if shouldPrintThinking(o.EffectiveThink) {
			if hist, ok := resp.Output.(taskengine.ChatHistory); ok {
				for _, msg := range hist.Messages {
					if msg.Role == "assistant" && msg.Thinking != "" {
						fmt.Fprintln(cmd.ErrOrStderr(), "\nReasoning:")
						fmt.Fprintln(cmd.ErrOrStderr(), msg.Thinking)
					}
				}
			}
		}
		printRelevantOutput(cmd.OutOrStdout(), resp.Output, resp.OutputType, effectiveRaw)
		if effectiveSteps && len(resp.Steps) > 0 {
			fmt.Fprintln(cmd.ErrOrStderr(), "\n📋 Steps:")
			for i, u := range resp.Steps {
				fmt.Fprintf(cmd.ErrOrStderr(), "  %d. %s (%s) %s %s\n", i+1, u.TaskID, u.TaskHandler, formatDuration(u.Duration), u.Transition)
			}
		}
		return nil
	},
}

// resolveRunInput returns the raw input string from --editor, --input, @file, positional args, or stdin.
func resolveRunInput(cmd *cobra.Command, args []string) (string, error) {
	flags := cmd.Flags()

	if useEditor, _ := cmd.Root().PersistentFlags().GetBool("editor"); useEditor {
		var seed []byte
		if data, ok, err := readStdinIfAvailable(maxCLIStdinBytes); err != nil {
			return "", err
		} else if ok {
			seed = []byte(data)
		}
		modelHint, _ := cmd.Root().PersistentFlags().GetString("model")
		return captureFromEditor(seed, modelHint)
	}

	if flags.Changed("input") {
		val, _ := flags.GetString("input")
		return resolveInputFlagValue("--input", val)
	}

	if len(args) > 0 {
		argsInput := strings.Join(args, " ")
		// If stdin is also piped, combine: args = instruction, stdin = data.
		// e.g. git diff | contenox run "suggest a commit message"
		data, ok, err := readStdinIfAvailable(maxCLIStdinBytes)
		if err != nil {
			return "", err
		}
		if ok && len(strings.TrimSpace(data)) > 0 {
			return argsInput + "\n\n" + data, nil
		}
		return argsInput, nil
	}

	data, ok, err := readStdinIfAvailable(maxCLIStdinBytes)
	if err != nil {
		return "", err
	}
	if ok {
		return data, nil
	}

	return "", nil
}

// parseRunInput converts a raw string into the typed value and DataType the engine expects.
func parseRunInput(raw, typeName string) (any, taskengine.DataType, error) {
	switch strings.ToLower(typeName) {
	case "string", "":
		return raw, taskengine.DataTypeString, nil

	case "chat":
		msgs := []taskengine.Message{}
		if cwd, cwdErr := os.Getwd(); cwdErr == nil {
			if content, path, ok := LoadAgentsMD(cwd); ok {
				msgs = append(msgs, AgentsMDMessage(content, path))
			}
		}
		msgs = append(msgs, taskengine.Message{Role: "user", Content: raw, Timestamp: time.Now().UTC()})
		return taskengine.ChatHistory{Messages: msgs}, taskengine.DataTypeChatHistory, nil

	case "json":
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, taskengine.DataTypeAny, fmt.Errorf("input is not valid JSON: %w", err)
		}
		return v, taskengine.DataTypeJSON, nil

	case "int":
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return nil, taskengine.DataTypeAny, fmt.Errorf("input is not a valid integer: %w", err)
		}
		return n, taskengine.DataTypeInt, nil

	default:
		return nil, taskengine.DataTypeAny, fmt.Errorf(
			"unknown input type %q — valid values: string, chat, json, int", typeName,
		)
	}
}

// buildRunOpts resolves effective options from flags and persistent SQLite config.
func buildRunOpts(cmd *cobra.Command, db libdbexec.DBManager, contenoxDir string) (chatOpts, error) {
	flags := cmd.Root().Flags()

	ctx := libtracker.WithNewRequestID(context.Background())
	store := runtimetypes.New(db.WithoutTransaction())

	// Read persistent defaults from SQLite KV; flags always override.
	kvModel, _ := getConfigKV(ctx, store, "default-model")
	kvProvider, _ := getConfigKV(ctx, store, "default-provider")
	kvAltModel, _ := getConfigKV(ctx, store, "default-alt-model")
	kvAltProvider, _ := getConfigKV(ctx, store, "default-alt-provider")
	effectiveMaxTokens, err := resolveEffectiveMaxTokens(ctx, store, flags)
	if err != nil {
		return chatOpts{}, err
	}
	effectiveThink, err := resolveEffectiveThink(ctx, store, flags)
	if err != nil {
		return chatOpts{}, err
	}

	effectiveModel, _ := flags.GetString("model")
	if !flags.Changed("model") && (effectiveModel == "" || effectiveModel == defaultModel) {
		if kvModel != "" {
			effectiveModel = kvModel
		} else {
			effectiveModel = defaultModel
		}
	}

	effectiveDefaultProvider := kvProvider
	if flags.Changed("provider") {
		if v, _ := flags.GetString("provider"); v != "" {
			effectiveDefaultProvider = v
		}
	}

	effectiveAltModel := kvAltModel
	if flags.Changed("alt-model") {
		if v, _ := flags.GetString("alt-model"); v != "" {
			effectiveAltModel = v
		}
	}

	effectiveAltProvider := kvAltProvider
	if flags.Changed("alt-provider") {
		if v, _ := flags.GetString("alt-provider"); v != "" {
			effectiveAltProvider = v
		}
	}

	effectiveContext, _ := flags.GetInt("context")
	effectiveTracing, _ := flags.GetBool("trace")

	effectiveEnableLocalExec, _ := flags.GetBool("shell")
	effectiveLocalExecAllowedDir, _ := flags.GetString("local-exec-allowed-dir")
	autoMode, _ := cmd.Flags().GetBool("auto")
	effectiveHITL := !autoMode

	return chatOpts{
		EffectiveDB:                  "", // resolved separately in RunE
		EffectiveChain:               "", // unused — run loads chain directly
		EffectiveContext:             effectiveContext,
		EffectiveDefaultModel:        effectiveModel,
		EffectiveDefaultProvider:     effectiveDefaultProvider,
		EffectiveConfiguredModel:     kvModel,
		EffectiveConfiguredProvider:  kvProvider,
		EffectiveAltDefaultModel:     effectiveAltModel,
		EffectiveAltDefaultProvider:  effectiveAltProvider,
		EffectiveMaxTokens:           effectiveMaxTokens,
		EffectiveNoDeleteModels:      true,
		EffectiveEnableLocalExec:     effectiveEnableLocalExec,
		EffectiveLocalExecAllowedDir: effectiveLocalExecAllowedDir,
		EffectiveHITL:                effectiveHITL,
		EffectiveTracing:             effectiveTracing,
		EffectiveThink:               effectiveThink,
		ContenoxDir:                  contenoxDir,
		// Shared by `run` and `beam`: both are invoked by a person, so engine
		// construction has somewhere to address an operator warning.
		WarnW: cmd.ErrOrStderr(),
	}, nil
}

func init() {
	f := runCmd.Flags()
	f.String("chain", "", "Path to a task chain JSON file (falls back to .contenox/default-run-chain.json if present)")
	f.String("input", "", "Input value or @path to read from a file (e.g. --input @main.go)")
	f.String("input-type", "string", "Input data type: string, chat, json, int")
	f.Bool("auto", false, "Non-interactive mode: disable HITL approval prompts. Default is HITL on; tools route through the active hitl-policy. Use --auto only in trusted/scripted contexts.")
}
