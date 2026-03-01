package vibecli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/contenox/vibe/taskengine"
	"github.com/spf13/cobra"
)

// execCmd runs any task chain with any input type.
// Unlike 'vibe run' (which hardcodes DataTypeChatHistory), 'vibe exec'
// lets the caller specify the input type and is fully stateless (no chat history).
var execCmd = &cobra.Command{
	Use:   "exec",
	Short: "Run any task chain with explicit input type control.",
	Long: `Run a task chain with explicit control over input type and content.

Unlike the default 'vibe run', exec is stateless — no chat history is loaded or saved.
It accepts any task chain regardless of the first handler's expected input type.

Input sources (in priority order):
  1. --input <value>         literal string (or @file to read from a file)
  2. Positional arguments    joined with a space
  3. Stdin                   if piped

Input types (--input-type):
  string (default)  Raw string. DataTypeString.
  chat              Wrap as a single user message. DataTypeChatHistory.
  json              Parse as JSON object. DataTypeJSON.
  int               Parse as integer. DataTypeInt.
  float             Parse as float. DataTypeFloat.
  bool              Parse as boolean. DataTypeBool.

Examples:
  vibe exec --chain .contenox/score-chain.json "is this code safe?"
  cat diff.txt | vibe exec --chain .contenox/review.json --input-type chat
  vibe exec --chain .contenox/embed.json --input-type string --input @myfile.go
  vibe exec --chain .contenox/parse-chain.json --input-type json '{"key":"value"}'
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}

		flags := cmd.Flags()
		cfgFilePath := ""
		cfg, cfgFilePath, err := loadLocalConfig()
		if err != nil {
			return err
		}
		// loadLocalConfig returns the path to config.yaml; we need the directory.
		contenoxDir := filepath.Dir(cfgFilePath)
		if cfgFilePath == "" {
			cwd, _ := os.Getwd()
			contenoxDir = filepath.Join(cwd, ".contenox")
		}

		// Resolve chain path (required for exec)
		chainPath, _ := flags.GetString("chain")
		if chainPath == "" {
			return fmt.Errorf("--chain is required for 'vibe exec'\n  Example: vibe exec --chain .contenox/my-chain.json \"your input\"")
		}

		// Resolve input
		rawInput, err := resolveExecInput(cmd, args)
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
		inputVal, inputType, err := parseExecInput(rawInput, inputTypeName)
		if err != nil {
			return fmt.Errorf("--input-type %q: %w", inputTypeName, err)
		}

		// Build runOpts from flags (reuses the same resolution logic as vibe run)
		o := buildExecOpts(cmd, cfg, contenoxDir)

		// Open database
		effectiveDB, _ := flags.GetString("db")
		if effectiveDB == "" && cfg.DB != "" {
			effectiveDB = cfg.DB
		}
		if effectiveDB == "" {
			effectiveDB = filepath.Join(contenoxDir, "local.db")
		}
		dbPathAbs, err := filepath.Abs(effectiveDB)
		if err != nil {
			return fmt.Errorf("invalid database path: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(dbPathAbs), 0755); err != nil {
			return fmt.Errorf("cannot create database directory: %w", err)
		}
		dbCtx := libtracker.WithNewRequestID(context.Background())
		db, err := libdbexec.NewSQLiteDBManager(dbCtx, dbPathAbs, runtimetypes.SchemaSQLite)
		if err != nil {
			return fmt.Errorf("failed to open database: %w", err)
		}
		defer db.Close()

		// Build engine
		engine, err := BuildEngine(ctx, db, o)
		if err != nil {
			return fmt.Errorf("failed to build engine: %w", err)
		}
		defer engine.Stop()

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
		for _, key := range cfg.TemplateVarsFromEnv {
			if v := os.Getenv(key); v != "" {
				templateVars[key] = v
			}
		}
		execCtx := taskengine.WithTemplateVars(
			libtracker.WithNewRequestID(ctx),
			templateVars,
		)

		// Set timeout
		timeout, _ := flags.GetDuration("timeout")
		execCtx, cancel := context.WithTimeout(execCtx, timeout)
		defer cancel()

		if o.EffectiveTracing {
			slog.Info("Executing chain", "chain", chainPathAbs, "input_type", inputTypeName)
		} else {
			fmt.Fprintln(os.Stderr, "Thinking...")
		}

		output, outputType, stateUnits, err := engine.TaskService.Execute(execCtx, &chain, inputVal, inputType)
		if err != nil {
			slog.Error("Chain execution failed", "error", err)
			os.Exit(1)
		}

		effectiveRaw, _ := flags.GetBool("raw")
		effectiveSteps, _ := flags.GetBool("steps")
		printRelevantOutput(output, outputType, effectiveRaw)
		if effectiveSteps && len(stateUnits) > 0 {
			fmt.Fprintln(os.Stderr, "\n📋 Steps:")
			for i, u := range stateUnits {
				fmt.Fprintf(os.Stderr, "  %d. %s (%s) %s %s\n", i+1, u.TaskID, u.TaskHandler, formatDuration(u.Duration), u.Transition)
			}
		}
		return nil
	},
}

// resolveExecInput returns the raw input string from --input, @file, positional args, or stdin.
func resolveExecInput(cmd *cobra.Command, args []string) (string, error) {
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
		return strings.Join(args, " "), nil
	}

	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) == 0 {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("failed to read from stdin: %w", err)
		}
		return string(data), nil
	}

	return "", nil
}

// parseExecInput converts a raw string into the typed value and DataType the engine expects.
func parseExecInput(raw, typeName string) (any, taskengine.DataType, error) {
	switch strings.ToLower(typeName) {
	case "string", "":
		return raw, taskengine.DataTypeString, nil

	case "chat":
		msg := taskengine.Message{Role: "user", Content: raw, Timestamp: time.Now()}
		return taskengine.ChatHistory{Messages: []taskengine.Message{msg}}, taskengine.DataTypeChatHistory, nil

	case "json":
		var v map[string]any
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

// buildExecOpts resolves effective options from flags and config for exec.
// It deliberately reuses the same resolution helpers as the root run command.
func buildExecOpts(cmd *cobra.Command, cfg localConfig, contenoxDir string) runOpts {
	flags := cmd.Root().Flags()

	effectiveModel, _ := flags.GetString("model")
	if !flags.Changed("model") && cfg.Model != "" {
		effectiveModel = cfg.Model
	}

	effectiveOllama, _ := flags.GetString("ollama")
	if !flags.Changed("ollama") && cfg.Ollama != "" {
		effectiveOllama = cfg.Ollama
	}

	effectiveContext, _ := flags.GetInt("context")
	if !flags.Changed("context") && cfg.Context != nil {
		effectiveContext = *cfg.Context
	}

	effectiveTracing, _ := flags.GetBool("trace")
	if !flags.Changed("trace") && cfg.Tracing != nil {

		effectiveTracing = *cfg.Tracing
	}

	effectiveEnableLocalExec := false
	if cfg.EnableLocalExec != nil {
		effectiveEnableLocalExec = *cfg.EnableLocalExec
	}
	if v, _ := flags.GetBool("enable-local-exec"); flags.Changed("enable-local-exec") {
		effectiveEnableLocalExec = v
	}

	effectiveLocalExecAllowedDir, _ := flags.GetString("local-exec-allowed-dir")
	if !flags.Changed("local-exec-allowed-dir") && cfg.LocalExecAllowedDir != "" {
		effectiveLocalExecAllowedDir = cfg.LocalExecAllowedDir
	}

	effectiveLocalExecAllowedCommands, _ := flags.GetString("local-exec-allowed-commands")
	if !flags.Changed("local-exec-allowed-commands") && cfg.LocalExecAllowedCommands != "" {
		effectiveLocalExecAllowedCommands = cfg.LocalExecAllowedCommands
	}

	effectiveLocalExecDeniedCommands := cfg.LocalExecDeniedCommands

	resolvedBackends, effectiveDefaultProvider, effectiveDefaultModel := resolveEffectiveBackends(cfg, effectiveOllama, effectiveModel)

	return runOpts{
		EffectiveDB:                       "", // resolved separately in RunE
		EffectiveChain:                    "", // unused — exec loads chain directly
		EffectiveContext:                  effectiveContext,
		EffectiveDefaultModel:             effectiveDefaultModel,
		EffectiveDefaultProvider:          effectiveDefaultProvider,
		EffectiveNoDeleteModels:           true,
		EffectiveEnableLocalExec:          effectiveEnableLocalExec,
		EffectiveLocalExecAllowedDir:      effectiveLocalExecAllowedDir,
		EffectiveLocalExecAllowedCommands: effectiveLocalExecAllowedCommands,
		EffectiveLocalExecDeniedCommands:  effectiveLocalExecDeniedCommands,
		EffectiveTracing:                  effectiveTracing,
		Cfg:                               cfg,
		ResolvedBackends:                  resolvedBackends,
		ContenoxDir:                       contenoxDir,
	}
}

func init() {
	f := execCmd.Flags()
	f.String("chain", "", "Path to a task chain JSON file (required)")
	f.String("input", "", "Input value or @path to read from a file (e.g. --input @main.go)")
	f.String("input-type", "string", "Input type: string, chat, json, int, float, bool")
}
