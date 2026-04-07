package contenoxcli

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/contenox/contenox/embedservice"
	"github.com/contenox/contenox/execservice"
	"github.com/contenox/contenox/internal/hooks"
	"github.com/contenox/contenox/internal/llmrepo"
	"github.com/contenox/contenox/internal/ollamatokenizer"
	"github.com/contenox/contenox/internal/runtimestate"
	"github.com/contenox/contenox/internal/server"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libkvstore"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/localhooks"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/serverapi"
	"github.com/contenox/contenox/taskchainservice"
	"github.com/contenox/contenox/taskengine"
	"github.com/contenox/contenox/vfsservice"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

func runServer(cmd *cobra.Command, args []string) error {
	tenant, _ := cmd.Flags().GetString("tenant")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return fmt.Errorf("invalid database path: %w", err)
	}
	db, err := OpenDBAt(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	contenoxPath, err := ResolveContenoxDir(cmd)
	if err != nil {
		return fmt.Errorf("invalid contenox path: %w", err)
	}

	components, err := buildServerComponents(ctx, db, tenant, contenoxPath, cmd)
	if err != nil {
		return fmt.Errorf("failed to build server components: %w", err)
	}
	defer components.cleanup() // close bus, etc.

	errRun, cleanupServer := server.Run(
		ctx,
		tenant,
		components.nodeID,
		components.config,
		components.state,
		components.tracker,
		components.bus,
		components.db,
		components.tokenizer,
		components.repo,
		components.envExec,
		components.hookRepo,
		components.taskService,
		components.embedService,
		components.execService,
		components.taskChainService,
		components.vfsSvc,
		contenoxPath,
	)
	defer func() {
		if cleanupServer != nil {
			_ = cleanupServer()
		}
	}()
	if errRun != nil {
		return fmt.Errorf("server failed: %w", errRun)
	}

	return nil
}

type serverComponents struct {
	config           *serverapi.Config
	nodeID           string
	db               libdb.DBManager
	bus              libbus.Messenger
	state            *runtimestate.State
	tracker          libtracker.ActivityTracker
	tokenizer        ollamatokenizer.Tokenizer
	repo             llmrepo.ModelRepo
	envExec          taskengine.EnvExecutor
	hookRepo         taskengine.HookRepo
	taskService      execservice.TasksEnvService
	embedService     embedservice.Service
	execService      execservice.ExecService
	taskChainService taskchainservice.Service
	vfsSvc           vfsservice.Service
	cleanup          func()
}

// buildServerComponents builds all dependencies for the server using SQLite and the CLI's
// configuration system (SQLite KV and models table). No environment variables are required
// for model configuration.
func buildServerComponents(ctx context.Context, db libdb.DBManager, tenantID string, contenoxPath string, cmd *cobra.Command) (*serverComponents, error) {
	// Load server configuration from environment (port, address, token, etc.)
	config := &serverapi.Config{}
	if err := serverapi.LoadConfig(config); err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	// When the PTY terminal is enabled without an explicit ceiling, default to the same
	// directory tree Beam uses for VFS (contenox path) so cwd and file listing stay aligned.
	if strings.EqualFold(strings.TrimSpace(config.TerminalEnabled), "true") && strings.TrimSpace(config.TerminalAllowedRoot) == "" {
		abs, err := filepath.Abs(contenoxPath)
		if err != nil {
			return nil, fmt.Errorf("default terminal_allowed_root: %w", err)
		}
		config.TerminalAllowedRoot = abs
	}
	// Override any database URL that might be set in env; we're using SQLite.
	config.DatabaseURL = ""

	nodeID := uuid.NewString()[0:8]

	// Read CLI configuration (default model and provider) from SQLite KV.
	store := runtimetypes.New(db.WithoutTransaction())
	ctxKV := libtracker.WithNewRequestID(ctx)

	defaultModel, err := getConfigKV(ctxKV, store, "default-model")
	if err != nil {
		return nil, err
	}
	defaultProvider, err := getConfigKV(ctxKV, store, "default-provider")
	if err != nil {
		return nil, err
	}
	// Read any stored context override for the default model.
	// If none exists, use 0 (meaning "use the model's advertised context").
	defaultCtx := 0
	if m, err := store.GetModelByName(ctxKV, defaultModel); err == nil && m != nil {
		defaultCtx = m.ContextLength
	}

	// Create SQLite-backed bus.
	bus := libbus.NewSQLite(db.WithoutTransaction())

	// Runtime state: auto-discover + KV (same family of options as the local CLI engine).
	stateOpts := []runtimestate.Option{
		runtimestate.WithAutoDiscoverModels(),
	}
	kvMgr := libkvstore.NewSQLiteManager(db)
	stateOpts = append(stateOpts, runtimestate.WithKVStore(kvMgr), runtimestate.WithAutoDiscoverModels())

	state, err := runtimestate.New(ctx, db, bus, stateOpts...)
	if err != nil {
		return nil, fmt.Errorf("runtime state: %w", err)
	}

	// Initialize embedder, prompt exec, chat exec using the default model and context.
	if err := runtimestate.InitEmbeder(ctx, &runtimestate.Config{
		DatabaseURL: "",
		EmbedModel:  defaultModel,
		TaskModel:   defaultModel,
		TenantID:    tenantID,
	}, db, defaultCtx, state); err != nil {
		return nil, fmt.Errorf("init embedder: %w", err)
	}

	if err := runtimestate.InitPromptExec(ctx, &runtimestate.Config{
		DatabaseURL: "",
		TaskModel:   defaultModel,
		EmbedModel:  defaultModel,
		TenantID:    tenantID,
	}, db, state, defaultCtx); err != nil {
		return nil, fmt.Errorf("init prompt exec: %w", err)
	}

	if err := runtimestate.InitChatExec(ctx, &runtimestate.Config{
		DatabaseURL: "",
		ChatModel:   defaultModel,
		TenantID:    tenantID,
	}, db, state, defaultCtx); err != nil {
		return nil, fmt.Errorf("init chat exec: %w", err)
	}

	// Keep an internal model row for the default model so bootstrap groups and local
	// overrides still have a stable record, even though OSS no longer exposes model CRUD.
	specs := []runtimestate.ExtraModelSpec{
		{
			Name:          defaultModel,
			ContextLength: defaultCtx,
			CanChat:       true,
			CanPrompt:     true,
			CanEmbed:      false,
		},
	}
	if err := runtimestate.EnsureModels(ctx, db, tenantID, specs); err != nil {
		return nil, fmt.Errorf("ensure models: %w", err)
	}

	// Run backend cycle to discover models (this will pull live model lists from backends).
	if err := state.RunBackendCycle(ctx); err != nil {
		slog.Warn("Backend cycle error", "error", err)
	}

	// Tokenizer (simple estimator, no external service).
	tokenizer := ollamatokenizer.NewEstimateTokenizer()
	tracker := libtracker.NewLogActivityTracker(slog.Default())

	// Model manager.
	repo, err := llmrepo.NewModelManager(state, tokenizer, llmrepo.ModelManagerConfig{
		DefaultPromptModel:    llmrepo.ModelConfig{Name: defaultModel, Provider: defaultProvider},
		DefaultEmbeddingModel: llmrepo.ModelConfig{Name: defaultModel, Provider: defaultProvider},
		DefaultChatModel:      llmrepo.ModelConfig{Name: defaultModel, Provider: defaultProvider},
	}, tracker)
	if err != nil {
		return nil, fmt.Errorf("model manager: %w", err)
	}

	// Hooks (same as in BuildEngine, but we don't need approval callbacks for the server).
	localHooks := map[string]taskengine.HookRepo{
		"echo":     localhooks.NewEchoHook(),
		"print":    localhooks.NewPrint(tracker),
		"webhook":  localhooks.NewWebCaller(),
		"local_fs": localhooks.NewLocalFSHook(""), // empty allowed dir = no restriction
	}
	if sshHook, err := localhooks.NewSSHHook(); err == nil {
		localHooks["ssh"] = sshHook
	}
	// Enable local_shell for the server (trusted environment).
	localHooks["local_shell"] = localhooks.NewLocalExecHook()

	hookRepo := hooks.NewPersistentRepo(localHooks, db, http.DefaultClient, bus)

	// Task engine.
	taskEngineCtx := taskengine.WithTaskEventSink(ctx, taskengine.NewBusTaskEventSink(bus))
	exec, err := taskengine.NewExec(taskEngineCtx, repo, hookRepo, tracker)
	if err != nil {
		return nil, fmt.Errorf("task exec: %w", err)
	}
	envExec, err := taskengine.NewEnv(taskEngineCtx, tracker, exec, taskengine.NewSimpleInspector(), hookRepo)
	if err != nil {
		return nil, fmt.Errorf("task env: %w", err)
	}
	envExec, err = taskengine.NewMacroEnv(envExec, hookRepo)
	if err != nil {
		return nil, fmt.Errorf("macro env: %w", err)
	}

	// Services.
	taskService := execservice.NewTasksEnv(ctx, envExec, hookRepo)
	embedService := embedservice.New(repo, defaultModel, defaultProvider)
	execService := execservice.NewExec(ctx, repo)

	vfsSvc := vfsservice.NewLocalFS(contenoxPath)
	taskChainService := taskchainservice.NewVFS(vfsSvc)
	taskChainService = taskchainservice.WithActivityTracker(taskChainService, tracker)

	// Cleanup function.
	cleanup := func() {
		bus.Close()
	}

	return &serverComponents{
		config:           config,
		nodeID:           nodeID,
		db:               db,
		bus:              bus,
		state:            state,
		tracker:          tracker,
		tokenizer:        tokenizer,
		repo:             repo,
		envExec:          envExec,
		hookRepo:         hookRepo,
		taskService:      taskService,
		embedService:     embedService,
		execService:      execService,
		taskChainService: taskChainService,
		vfsSvc:           vfsSvc,
		cleanup:          cleanup,
	}, nil
}
