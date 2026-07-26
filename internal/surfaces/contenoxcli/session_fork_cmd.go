package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/chatservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/spf13/cobra"
)

var sessionForkCmd = &cobra.Command{
	Use:   "fork [name]",
	Short: "Fork the active session to a new session.",
	Long: `Fork the active session into a new session and switch to it.

Without flags, the new session is a verbatim copy of the active session's
history. The original is left untouched.

  contenox session fork                       # bare fork (verbatim copy)
  contenox session fork archive-2026          # named fork

With --summary, an LLM compacts older messages into a single summary
before forking. System messages and the last --keep messages are kept
verbatim. Useful when conversation history exceeds the model's context
window.

  contenox session fork --summary             # compact older history into a summary
  contenox session fork next --summary --keep 12`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSessionFork,
}

func init() {
	sessionForkCmd.Flags().Bool("summary", false, "Summarize older messages before forking (uses LLM)")
	sessionForkCmd.Flags().Int("keep", 8, "With --summary: number of recent messages to keep verbatim")
	sessionCmd.AddCommand(sessionForkCmd)
}

func runSessionFork(cmd *cobra.Command, args []string) error {
	summary, _ := cmd.Flags().GetBool("summary")
	keep, _ := cmd.Flags().GetInt("keep")

	ctx, db, svc, cleanup, err := openSessionService(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	activeID, err := svc.GetActiveID(ctx)
	if err != nil || activeID == "" {
		return fmt.Errorf("no active session; run 'contenox session new' to create one")
	}

	contenoxDir, _ := ResolveContenoxDir(cmd)
	workspaceID := ResolveWorkspaceID(contenoxDir)
	chatMgr := chatservice.NewManager(workspaceID)

	history, err := chatMgr.ListMessages(ctx, db.WithoutTransaction(), activeID)
	if err != nil {
		return fmt.Errorf("failed to load history: %w", err)
	}
	if len(history) == 0 {
		return fmt.Errorf("active session has no messages to fork")
	}

	newMessages := history
	if summary {
		newMessages, err = summarizeForFork(ctx, cmd, db, contenoxDir, history, keep)
		if err != nil {
			return err
		}
	}

	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	newID, err := svc.New(ctx, localIdentity, name)
	if err != nil {
		return fmt.Errorf("failed to create new session: %w", err)
	}

	cleanCtx := context.WithoutCancel(ctx)
	exec, commit, release, err := db.WithTransaction(cleanCtx)
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer release()
	if err := chatMgr.PersistDiff(cleanCtx, exec, newID, newMessages); err != nil {
		return fmt.Errorf("failed to persist messages: %w", err)
	}
	if err := commit(cleanCtx); err != nil {
		return fmt.Errorf("failed to commit: %w", err)
	}

	displayName := name
	if displayName == "" {
		displayName = newID[:8] + "…"
	}
	if summary {
		fmt.Fprintf(cmd.OutOrStdout(), "Forked %d messages to session %q (compacted to %d). Now active.\n", len(history), displayName, len(newMessages))
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Forked %d messages to session %q. Now active.\n", len(history), displayName)
	}
	return nil
}

func summarizeForFork(ctx context.Context, cmd *cobra.Command, db libdb.DBManager, contenoxDir string, history []taskengine.Message, keep int) ([]taskengine.Message, error) {
	model, provider, altModel, altProvider, maxTokens, err := resolveDefaultModelProvider(cmd, db)
	if err != nil {
		return nil, err
	}
	if model == "" {
		return nil, fmt.Errorf("no default model configured; run 'contenox config set default-model <model>'")
	}

	rootFlags := cmd.Root().PersistentFlags()
	noDeleteModels, _ := rootFlags.GetBool("no-delete-models")
	if !rootFlags.Changed("no-delete-models") {
		noDeleteModels = true
	}

	opts := chatOpts{
		EffectiveDefaultModel:       model,
		EffectiveDefaultProvider:    provider,
		EffectiveAltDefaultModel:    altModel,
		EffectiveAltDefaultProvider: altProvider,
		EffectiveMaxTokens:          maxTokens,
		EffectiveNoDeleteModels:     noDeleteModels,
		ContenoxDir:                 contenoxDir,
	}

	engineCtx := libtracker.WithNewRequestID(ctx)
	engine, err := BuildEngine(engineCtx, db, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to build engine: %w", err)
	}
	defer engine.Stop()

	chainPath, err := resolveSystemChain(cmd, contenoxDir, "chain-compact.json")
	if err != nil {
		return nil, err
	}
	chainData, err := os.ReadFile(chainPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read chain %q: %w", chainPath, err)
	}
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal(chainData, &chain); err != nil {
		return nil, fmt.Errorf("invalid chain JSON at %q: %w", chainPath, err)
	}

	templateVars := map[string]string{
		"model":    model,
		"provider": provider,
		"chain":    chain.ID,
	}
	if altModel != "" {
		templateVars["alt_model"] = altModel
	}
	if altProvider != "" {
		templateVars["alt_provider"] = altProvider
	}
	execCtx := taskengine.WithTemplateVars(engineCtx, templateVars)

	fmt.Fprintln(cmd.ErrOrStderr(), "Summarizing...")
	return chatservice.CompactHistory(execCtx, engine.TaskService, &chain, history, keep)
}

func resolveSystemChain(cmd *cobra.Command, contenoxDir, name string) (string, error) {
	if chainFlag, _ := cmd.Root().PersistentFlags().GetString("chain"); chainFlag != "" && cmd.Root().PersistentFlags().Changed("chain") {
		if filepath.Base(chainFlag) == name {
			abs, err := filepath.Abs(chainFlag)
			if err != nil {
				return "", fmt.Errorf("invalid --chain path: %w", err)
			}
			return abs, nil
		}
	}
	return lookupSystemFile(contenoxDir, name)
}

// lookupSystemFile finds a config file by name. Workspace .contenox/ overrides
// home ~/.contenox/. Returns an error if neither exists.
func lookupSystemFile(contenoxDir, name string) (string, error) {
	if contenoxDir != "" {
		workspacePath := filepath.Join(contenoxDir, name)
		if _, err := os.Stat(workspacePath); err == nil {
			return workspacePath, nil
		}
	}
	homeDir, err := globalContenoxDir()
	if err != nil {
		return "", fmt.Errorf("could not resolve ~/.contenox: %w", err)
	}
	homePath := filepath.Join(homeDir, name)
	if _, err := os.Stat(homePath); err == nil {
		return homePath, nil
	}
	return "", fmt.Errorf("file %q not found in workspace %q or ~/.contenox; run 'contenox init' to populate it", name, contenoxDir)
}

func resolveDefaultModelProvider(cmd *cobra.Command, db libdb.DBManager) (model, provider, altModel, altProvider, maxTokens string, err error) {
	flags := cmd.Root().Flags()
	store := runtimetypes.New(db.WithoutTransaction())
	ctx := libtracker.WithNewRequestID(context.Background())

	model, _ = flags.GetString("model")
	if !flags.Changed("model") || model == defaultModel {
		if kv, _ := getConfigKV(ctx, store, "default-model"); kv != "" {
			model = kv
		}
	}
	if kv, _ := getConfigKV(ctx, store, "default-provider"); kv != "" {
		provider = kv
	}
	if flags.Changed("provider") {
		provider, _ = flags.GetString("provider")
	}
	if kv, _ := getConfigKV(ctx, store, "default-alt-model"); kv != "" {
		altModel = kv
	}
	if flags.Changed("alt-model") {
		if v, _ := flags.GetString("alt-model"); v != "" {
			altModel = v
		}
	}
	if kv, _ := getConfigKV(ctx, store, "default-alt-provider"); kv != "" {
		altProvider = kv
	}
	if flags.Changed("alt-provider") {
		if v, _ := flags.GetString("alt-provider"); v != "" {
			altProvider = v
		}
	}
	maxTokens, err = resolveEffectiveMaxTokens(ctx, store, flags)
	if err != nil {
		return "", "", "", "", "", err
	}
	return model, provider, altModel, altProvider, maxTokens, nil
}
