package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/contenox/contenox/runtime/taskengine"
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
  float             Parsed as float (DataTypeFloat)
  bool              Parsed as boolean: true/false/1/0 (DataTypeBool)

If --chain is not specified, falls back to .contenox/default-run-chain.json
if that file exists in the current directory.

Examples:
  contenox run --chain .contenox/score-chain.json "is this code safe?"
  cat diff.txt | contenox run --chain .contenox/review.json --input-type chat
  contenox run --chain .contenox/embed.json --input @myfile.go
  contenox run --chain .contenox/parse-chain.json --input-type json '{"key":"value"}'
  git diff | contenox run "suggest a commit message"  # uses default-run-chain.json

  # Run with human approval before any write_file, sed, or local_shell tool call:
  contenox run --shell --hitl --chain .contenox/my-chain.json "fix the bug"
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

		// Resolve chain path (fallback to default chain if not specified)
		chainPath, _ := flags.GetString("chain")
		if chainPath == "" && !flags.Changed("chain") {
			wellKnown := filepath.Join(contenoxDir, "default-run-chain.json")
			if _, err := os.Stat(wellKnown); err == nil {
				chainPath = wellKnown
			}
		}
		if chainPath == "" {
			fmt.Fprintln(os.Stderr, "No .contenox/ project found in this directory or any parent directory.")
			fmt.Fprintln(os.Stderr, "Run 'contenox init' to get started, or pass --chain explicitly.")
			return errChainRequired
		}

		// Resolve input
		rawInput, err := resolveRunInput(cmd, args)
		if err != nil {
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

		// Build chatOpts from flags and SQLite KV defaults.
		o := buildRunOpts(cmd, db, contenoxDir)
		o.EffectiveDB = dbPathAbs

		engine, err := BuildEngine(ctx, db, o)
		if err != nil {
			return fmt.Errorf("failed to build engine: %w", err)
		}
		defer engine.Stop()

		if err := PreflightLLMSetup(cmd.ErrOrStderr(), engine.SetupCheck); err != nil {
			return err
		}

		// Load chain
		chainPathAbs, err := filepath.Abs(chainPath)
		if err != nil {
			return fmt.Errorf("invalid chain path: %w", err)
		}
		chainData, err := os.ReadFile(chainPathAbs)
		if err != nil {
			return fmt.Errorf("failed to read chain %q: %w", chainPathAbs, err)
		}

		var chain taskengine.TaskChainDefinition
		if err := json.Unmarshal(chainData, &chain); err != nil {
			return fmt.Errorf("failed to parse chain JSON: %w", err)
		}

		// Set template vars
		templateVars := map[string]string{
			"model":    o.EffectiveDefaultModel,
			"provider": o.EffectiveDefaultProvider,
			"chain":    chain.ID,
		}
		execCtx := taskengine.WithTemplateVars(
			libtracker.WithNewRequestID(ctx),
			templateVars,
		)

		// Set timeout
		timeout, _ := flags.GetDuration("timeout")
		timeoutCtx, timeoutCancel := context.WithTimeout(execCtx, timeout)
		defer timeoutCancel()

		// Use signal.NotifyContext so the goroutine is cleaned up automatically
		// when the command returns, instead of leaking a blocked goroutine.
		execCtx, stop := signal.NotifyContext(timeoutCtx, syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		effectiveThink, err := flags.GetBool("think")
		if err != nil {
			return fmt.Errorf("failed to get think flag: %w", err)
		}
		stopTaskEvents := startCLITaskEventStream(execCtx, engine, cmd.ErrOrStderr(), cliTaskEventRenderOptions{
			Trace:        o.EffectiveTracing,
			ShowThinking: effectiveThink,
		})
		defer stopTaskEvents()

		if o.EffectiveTracing {
			slog.Info("Executing chain", "chain", chainPathAbs, "input_type", inputTypeName)
		} else {
			fmt.Fprintln(cmd.ErrOrStderr(), "Thinking...")
		}

		output, outputType, stateUnits, err := engine.TaskService.Execute(execCtx, &chain, inputVal, inputType)
		if err != nil {
			if isModelResolverFailure(err) {
				PrintSetupIssues(cmd.ErrOrStderr(), engine.SetupCheck)
			}
			return fmt.Errorf("chain execution failed: %w", err)
		}

		effectiveRaw, _ := flags.GetBool("raw")
		effectiveSteps, _ := flags.GetBool("steps")
		if effectiveThink {
			if hist, ok := output.(taskengine.ChatHistory); ok {
				for _, msg := range hist.Messages {
					if msg.Role == "assistant" && msg.Thinking != "" {
						fmt.Fprintln(cmd.ErrOrStderr(), "\n💭 Reasoning:")
						fmt.Fprintln(cmd.ErrOrStderr(), msg.Thinking)
					}
				}
			}
		}
		printRelevantOutput(cmd.OutOrStdout(), output, outputType, effectiveRaw)
		if effectiveSteps && len(stateUnits) > 0 {
			fmt.Fprintln(cmd.ErrOrStderr(), "\n📋 Steps:")
			for i, u := range stateUnits {
				fmt.Fprintf(cmd.ErrOrStderr(), "  %d. %s (%s) %s %s\n", i+1, u.TaskID, u.TaskHandler, formatDuration(u.Duration), u.Transition)
			}
		}
		return nil
	},
}

// resolveRunInput returns the raw input string from --input, @file, positional args, or stdin.
func resolveRunInput(cmd *cobra.Command, args []string) (string, error) {
	flags := cmd.Flags()

	if flags.Changed("input") {
		val, _ := flags.GetString("input")
		if strings.HasPrefix(val, "@") {
			path := strings.TrimPrefix(val, "@")
			data, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("--input @%s: cannot read file: %w", path, err)
			}
			return string(data), nil
		}
		return val, nil
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
		msg := taskengine.Message{Role: "user", Content: raw, Timestamp: time.Now().UTC()}
		return taskengine.ChatHistory{Messages: []taskengine.Message{msg}}, taskengine.DataTypeChatHistory, nil

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

	case "float":
		f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return nil, taskengine.DataTypeAny, fmt.Errorf("input is not a valid float: %w", err)
		}
		return f, taskengine.DataTypeFloat, nil

	case "bool":
		b, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return nil, taskengine.DataTypeAny, fmt.Errorf("input is not a valid bool (use true/false/1/0): %w", err)
		}
		return b, taskengine.DataTypeBool, nil

	default:
		return nil, taskengine.DataTypeAny, fmt.Errorf(
			"unknown input type %q — valid values: string, chat, json, int, float, bool", typeName,
		)
	}
}

// buildRunOpts resolves effective options from flags and persistent SQLite config.
func buildRunOpts(cmd *cobra.Command, db libdbexec.DBManager, contenoxDir string) chatOpts {
	flags := cmd.Root().Flags()

	ctx := libtracker.WithNewRequestID(context.Background())
	store := runtimetypes.New(db.WithoutTransaction())

	// Read persistent defaults from SQLite KV; flags always override.
	kvModel, _ := getConfigKV(ctx, store, "default-model")
	kvProvider, _ := getConfigKV(ctx, store, "default-provider")

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

	effectiveContext, _ := flags.GetInt("context")
	effectiveTracing, _ := flags.GetBool("trace")

	effectiveEnableLocalExec, _ := flags.GetBool("shell")
	effectiveLocalExecAllowedDir, _ := flags.GetString("local-exec-allowed-dir")
	effectiveHITL, _ := cmd.Flags().GetBool("hitl")

	return chatOpts{
		EffectiveDB:                  "", // resolved separately in RunE
		EffectiveChain:               "", // unused — run loads chain directly
		EffectiveContext:             effectiveContext,
		EffectiveDefaultModel:        effectiveModel,
		EffectiveDefaultProvider:     effectiveDefaultProvider,
		EffectiveNoDeleteModels:      true,
		EffectiveEnableLocalExec:     effectiveEnableLocalExec,
		EffectiveLocalExecAllowedDir: effectiveLocalExecAllowedDir,
		EffectiveHITL:                effectiveHITL,
		EffectiveTracing:             effectiveTracing,
		ContenoxDir:                  contenoxDir,
	}
}

func init() {
	f := runCmd.Flags()
	f.String("chain", "", "Path to a task chain JSON file (falls back to .contenox/default-run-chain.json if present)")
	f.String("input", "", "Input value or @path to read from a file (e.g. --input @main.go)")
	f.String("input-type", "string", "Input data type: string, chat, json, int, float, bool")
	f.Bool("hitl", false, "Pause before write_file, sed, and local_shell calls; require y/n approval in the terminal")
}
