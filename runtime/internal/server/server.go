package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/apiframework/middleware"
	"github.com/contenox/contenox/runtime/embedservice"
	"github.com/contenox/contenox/runtime/execservice"
	"github.com/contenox/contenox/runtime/internal/auth"
	usersapi "github.com/contenox/contenox/runtime/internal/authapi"
	"github.com/contenox/contenox/runtime/internal/llmrepo"
	"github.com/contenox/contenox/runtime/internal/openapidocs"
	"github.com/contenox/contenox/runtime/internal/ollamatokenizer"
	"github.com/contenox/contenox/runtime/internal/runtimestate"
	"github.com/contenox/contenox/runtime/internal/web/beam"
	"github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/hitlservice"
	"github.com/contenox/contenox/runtime/serverapi"
	"github.com/contenox/contenox/runtime/taskchainservice"
	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/contenox/contenox/runtime/vfsservice"
)

// Build creates the combined HTTP handler (API routes + Beam SPA) without starting
// a network listener. The returned cleanup func must be called when done.
// Used by Run (HTTP server) and the Wails desktop entry point.
func Build(
	ctx context.Context,
	tenantID, nodeInstanceID string,
	config *serverapi.Config,
	state *runtimestate.State,
	tracker libtracker.ActivityTracker,
	ps libbus.Messenger,
	dbInstance libdbexec.DBManager,
	tokenizerSvc ollamatokenizer.Tokenizer,
	repo llmrepo.ModelRepo,
	environmentExec taskengine.EnvExecutor,
	hookRepo taskengine.HookRepo,
	hitlSvc hitlservice.Service,
	taskService execservice.TasksEnvService,
	embedService embedservice.Service,
	execService execservice.ExecService,
	taskChainService taskchainservice.Service,
	vfsSvc vfsservice.Service,
	chainVFS vfsservice.Service,
	vfsRoot string,
) (http.Handler, func() error, error) {
	if tenantID == "" {
		tenantID = "00000000-0000-0000-0000-000000000001"
	}

	internalMux := http.NewServeMux()

	tokenTTL := 24 * time.Hour
	authManager := auth.NewSimpleTokenManager(tokenTTL)

	usersapi.AddAuthRoutes(internalMux, authManager, authManager)

	cleanupAPI, err := serverapi.New(
		ctx,
		internalMux,
		nodeInstanceID,
		tenantID,
		config,
		dbInstance,
		ps,
		repo,
		environmentExec,
		state,
		hookRepo,
		hookRepo,
		hitlSvc,
		taskService,
		embedService,
		execService,
		taskChainService,
		vfsSvc,
		chainVFS,
		authManager,
		vfsRoot,
	)
	if err != nil {
		return nil, cleanupAPI, fmt.Errorf("init API handler: %w", err)
	}

	var apiHandler http.Handler = internalMux
	apiHandler = apiframework.RequestIDMiddleware(apiHandler)
	apiHandler = apiframework.TracingMiddleware(apiHandler)
	if config.Token != "" {
		apiHandler = apiframework.TokenMiddleware(apiHandler)
		apiHandler = apiframework.EnforceToken(config.Token, apiHandler)
	}
	corsConfig := middleware.CORSConfig{
		AllowedAPIOrigins: middleware.DefaultAllowedAPIOrigins,
		AllowedMethods:    middleware.DefaultAllowedMethods,
		AllowedHeaders:    middleware.DefaultAllowedHeaders,
	}
	apiHandler = middleware.EnableCORS(&corsConfig, apiHandler)

	// Execution order: Extract -> Refresh -> Auth -> handler
	apiHandler = middleware.JWTAuthMiddleware(authManager, apiHandler)
	apiHandler = middleware.JWTRefreshMiddleware(authManager, apiHandler)
	apiHandler = middleware.ExtractAndSetTokenMiddleware(apiHandler)

	mux := http.NewServeMux()
	openapidocs.Register(mux)
	mux.Handle("/api/", http.StripPrefix("/api", apiHandler))
	mux.Handle("/", beam.Handler())

	return mux, cleanupAPI, nil
}

// Run starts the Contenox server with the given tenant ID and blocks until ctx is cancelled.
// The server loads its configuration from environment variables (see serverapi.Config).
// If tenantID is empty, a default local tenant ID is used.
func Run(
	ctx context.Context,
	tenantID, nodeInstanceID string,
	config *serverapi.Config,
	state *runtimestate.State,
	tracker libtracker.ActivityTracker,
	ps libbus.Messenger,
	dbInstance libdbexec.DBManager,
	tokenizerSvc ollamatokenizer.Tokenizer,
	repo llmrepo.ModelRepo,
	environmentExec taskengine.EnvExecutor,
	hookRepo taskengine.HookRepo,
	hitlSvc hitlservice.Service,
	taskService execservice.TasksEnvService,
	embedService embedservice.Service,
	execService execservice.ExecService,
	taskChainService taskchainservice.Service,
	vfsSvc vfsservice.Service,
	chainVFS vfsservice.Service,
	vfsRoot string,
) (error, func() error) {
	if config.Addr == "" {
		config.Addr = "127.0.0.1"
	}
	if config.Port == "" {
		config.Port = "8081"
	}

	h, cleanupAPI, err := Build(
		ctx, tenantID, nodeInstanceID, config, state, tracker, ps, dbInstance,
		tokenizerSvc, repo, environmentExec, hookRepo, hitlSvc, taskService,
		embedService, execService, taskChainService, vfsSvc, chainVFS, vfsRoot,
	)
	if err != nil {
		return err, cleanupAPI
	}

	addr := config.Addr + ":" + config.Port
	srv := &http.Server{
		Addr:    addr,
		Handler: h,
	}
	serveErrCh := make(chan error, 1)

	go func() {
		log.Printf("%s server listening on %s", nodeInstanceID, addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("%s server error: %v", nodeInstanceID, err)
			serveErrCh <- err
			return
		}
		serveErrCh <- nil
	}()

	select {
	case err := <-serveErrCh:
		if err != nil {
			return fmt.Errorf("server serve: %w", err), cleanupAPI
		}
		return nil, cleanupAPI
	case <-ctx.Done():
	}

	log.Printf("%s shutting down server", nodeInstanceID)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server shutdown: %w", err), cleanupAPI
	}
	if err := <-serveErrCh; err != nil {
		return fmt.Errorf("server serve: %w", err), cleanupAPI
	}

	return nil, cleanupAPI
}
