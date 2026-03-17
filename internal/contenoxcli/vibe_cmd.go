package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/contenox/contenox/chatservice"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/localhooks"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/taskengine"
	"github.com/spf13/cobra"
)

var vibeCmd = &cobra.Command{
	Use:   "vibe",
	Short: "Interactive TUI: chat, plan and shell in one persistent session.",
	Long: `Launch the contenox interactive terminal UI.

The TUI keeps the engine — including MCP subprocess connections — alive for the
entire session, so stateful tools (browsers, database connections, etc.) survive
across turns. This is the recommended way to run multi-step plans interactively.

Input modes
  <text>                           send a chat message (same history as 'contenox chat')
  $ <shell-cmd>                    run a shell command; stdout is injected into LLM context
  /plan new <goal>                 generate a new step-by-step plan
  /plan next [--auto]              execute the next pending step (--auto loops until done or failed)
  /plan show                       print the active plan to the chat log
  /plan list                       print all plans (* = active)
  /plan retry <N>                  reset step N back to pending
  /plan skip  <N>                  mark step N as skipped
  /plan replan                     ask the LLM to regenerate remaining steps
  /run --chain <file> [input]      run a chain file statelessly (like 'contenox run')
  /mcp                             refresh MCP worker list in sidebar
  /session show                    show current session ID and message count
  /help                            print this command list
  Ctrl+C                           quit

Flags are the same as 'contenox chat': --shell, --model, --provider, --context,
--chain (sets the chat chain), --trim, --last, --trace, --timeout.

Examples:
  contenox vibe
  contenox vibe --shell --model qwen2.5:7b
  contenox vibe --chain .contenox/my-chain.json --trim 20`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runVibe,
}

func init() {
	// Local flags (not on root, vibe-only).
	vibeCmd.Flags().Int("trim", 0, "Only send last N messages to model (0 = all)")
	vibeCmd.Flags().Int("last", 0, "Show last N turns in the viewport on startup (0 = all)")

	rootCmd.AddCommand(vibeCmd)
	reservedSubcommands["vibe"] = true
}

func runVibe(cmd *cobra.Command, _ []string) error {
	flags := cmd.Root().PersistentFlags()

	contenoxDir, err := ResolveContenoxDir()
	if err != nil {
		return fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}

	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return fmt.Errorf("invalid database path: %w", err)
	}

	dbCtx := libtracker.WithNewRequestID(context.Background())
	db, err := openDBAt(dbCtx, dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	ctx := libtracker.WithNewRequestID(context.Background())
	store := runtimetypes.New(db.WithoutTransaction())

	// ── Resolve model / provider (same priority order as chat_cmd.go) ──────────
	effectiveModel, _ := flags.GetString("model")
	if !flags.Changed("model") || effectiveModel == defaultModel {
		if kv, _ := getConfigKV(ctx, store, "default-model"); kv != "" {
			effectiveModel = kv
		} else {
			effectiveModel = defaultModel
		}
	}
	effectiveProvider := ""
	if kv, _ := getConfigKV(ctx, store, "default-provider"); kv != "" {
		effectiveProvider = kv
	}
	if flags.Changed("provider") {
		if v, _ := flags.GetString("provider"); v != "" {
			effectiveProvider = v
		}
	}

	effectiveContext, _ := flags.GetInt("context")
	effectiveShell, _ := flags.GetBool("shell")
	effectiveAllowedDir, _ := flags.GetString("local-exec-allowed-dir")
	effectiveTracing, _ := flags.GetBool("trace")

	opts := chatOpts{
		EffectiveDB:              dbPath,
		EffectiveDefaultModel:    effectiveModel,
		EffectiveDefaultProvider: effectiveProvider,
		EffectiveContext:         effectiveContext,
		EffectiveNoDeleteModels:  true,
		EffectiveEnableLocalExec: effectiveShell,
		EffectiveLocalExecAllowedDir: effectiveAllowedDir,
		EffectiveTracing:         effectiveTracing,
	}

	// ── HITL approval gate ─────────────────────────────────────────────────────
	// The HITL callback needs a reference to *tea.Program to call prog.Send().
	// We create an atomic pointer here so the closure can capture it now and
	// read it later — the callback is only ever invoked inside p.Run(), by which
	// point prog has already been stored in the pointer.
	var progRef atomic.Pointer[tea.Program]
	hitlAsk := func(ctx context.Context, req localhooks.ApprovalRequest) (bool, error) {
		p := progRef.Load()
		if p == nil {
			return false, nil // program not started yet — deny safely
		}
		respChan := make(chan bool, 1)
		p.Send(vibeHITLPromptMsg{Req: req, Response: respChan})
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case approved := <-respChan:
			return approved, nil
		}
	}
	opts.AskApproval = hitlAsk

	// ── Chat chain (same fallback order as chat_cmd.go) ────────────────────────
	chatChain, err := vibeLoadChain(ctx, store, flags.Changed("chain"),
		func() string { v, _ := flags.GetString("chain"); return v }(),
		contenoxDir)
	if err != nil {
		return err
	}

	// ── Plan chains (embedded JSON, written by ensurePlanChains) ───────────────
	plannerPath, executorPath, err := ensurePlanChains(contenoxDir)
	if err != nil {
		return fmt.Errorf("failed to write plan chains: %w", err)
	}
	plannerChain, err := vibeReadChain(plannerPath)
	if err != nil {
		return fmt.Errorf("failed to load planner chain: %w", err)
	}
	if err := validatePlannerChain(plannerChain, plannerPath); err != nil {
		slog.Warn("Planner chain validation warning", "error", err)
	}
	executorChain, err := vibeReadChain(executorPath)
	if err != nil {
		return fmt.Errorf("failed to load executor chain: %w", err)
	}
	if err := validateExecutorChain(executorChain, executorPath); err != nil {
		slog.Warn("Executor chain validation warning", "error", err)
	}

	opts.PlannerChain = plannerChain
	opts.ExecutorChain = executorChain

	engine, err := BuildEngine(ctx, db, opts)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}
	defer engine.Stop()

	// ── Session ────────────────────────────────────────────────────────────────
	sessionID, err := ensureDefaultSession(ctx, db)
	if err != nil {
		slog.Warn("Failed to resolve session", "error", err)
		sessionID = ""
	}

	// ── History (load + optional trim) ─────────────────────────────────────────
	trimN, _ := cmd.Flags().GetInt("trim")
	initHistory := taskengine.ChatHistory{}
	if sessionID != "" {
		chatMgr := chatservice.NewManager(nil)
		if msgs, err := chatMgr.ListMessages(ctx, db.WithoutTransaction(), sessionID); err == nil {
			if trimN > 0 && len(msgs) > trimN {
				msgs = msgs[len(msgs)-trimN:]
			}
			initHistory.Messages = msgs
		}
	}

	m := newVibeModel(ctx, engine, db, sessionID, contenoxDir,
		chatChain, plannerChain, executorChain,
		effectiveModel, effectiveProvider, initHistory)

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	// Store the program reference so the HITL callback can call p.Send().
	progRef.Store(p)
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}
	return nil
}

// vibeLoadChain resolves the chat chain: --chain flag → config KV → .contenox/default-chain.json
func vibeLoadChain(ctx context.Context, store runtimetypes.Store, flagChanged bool, flagPath string, contenoxDir string) (*taskengine.TaskChainDefinition, error) {
	if flagChanged && flagPath != "" {
		return vibeReadChain(flagPath)
	}
	if kv, _ := getConfigKV(ctx, store, "default-chain"); kv != "" {
		p := kv
		if !filepath.IsAbs(p) {
			p = filepath.Join(contenoxDir, p)
		}
		return vibeReadChain(p)
	}
	if p := filepath.Join(contenoxDir, "default-chain.json"); fileExistsVibe(p) {
		return vibeReadChain(p)
	}
	return nil, fmt.Errorf("no chat chain found — run: contenox init\n  or: contenox config set default-chain <path>")
}

func vibeReadChain(path string) (*taskengine.TaskChainDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read chain %q: %w", path, err)
	}
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal(data, &chain); err != nil {
		return nil, fmt.Errorf("invalid chain JSON in %q: %w", path, err)
	}
	return &chain, nil
}

func fileExistsVibe(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
