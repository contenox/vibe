package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/apiframework/middleware"
	"github.com/contenox/contenox/embedservice"
	"github.com/contenox/contenox/execservice"
	"github.com/contenox/contenox/internal/auth"
	usersapi "github.com/contenox/contenox/internal/authapi"
	"github.com/contenox/contenox/internal/llmrepo"
	"github.com/contenox/contenox/internal/ollamatokenizer"
	"github.com/contenox/contenox/internal/runtimestate"
	"github.com/contenox/contenox/internal/web/beam"
	"github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/serverapi"
	"github.com/contenox/contenox/taskchainservice"
	"github.com/contenox/contenox/taskengine"
	"github.com/contenox/contenox/vfsservice"
)

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
	taskService execservice.TasksEnvService,
	embedService embedservice.Service,
	execService execservice.ExecService,
	taskChainService taskchainservice.Service,
	vfsSvc vfsservice.Service,
	vfsRoot string,
) (error, func() error) {
	if tenantID == "" {
		tenantID = "00000000-0000-0000-0000-000000000001"
	}
	if config.Addr == "" {
		config.Addr = "127.0.0.1"
	}
	if config.Port == "" {
		config.Port = "8081"
	}

	internalMux := http.NewServeMux()

	// Create the authentication manager (hardcoded admin user)
	tokenTTL := 24 * time.Hour // TODO: make configurable
	authManager := auth.NewSimpleTokenManager(tokenTTL)

	// Add authentication routes from usersapi
	usersapi.AddAuthRoutes(internalMux, authManager, authManager)

	// Add all other API routes (backend, exec, etc.)
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
		taskService,
		embedService,
		execService,
		taskChainService,
		vfsSvc,
		authManager,
		vfsRoot,
	)
	if err != nil {
		return fmt.Errorf("init API handler: %w", err), cleanupAPI
	}

	// Build the full handler with middleware chain
	var apiHandler http.Handler = internalMux

	// Base middleware (request ID, tracing)
	apiHandler = apiframework.RequestIDMiddleware(apiHandler)
	apiHandler = apiframework.TracingMiddleware(apiHandler)

	// Optional static token (if configured)
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

	// Authentication middleware stack.
	// Execution order must be:
	// Extract -> Refresh -> Auth -> handler
	// so browser cookies can be refreshed before validation rejects an expiring token.
	apiHandler = middleware.JWTAuthMiddleware(authManager, apiHandler)       // innermost: validates token after refresh
	apiHandler = middleware.JWTRefreshMiddleware(authManager, apiHandler)    // refresh browser token before auth validation
	apiHandler = middleware.ExtractAndSetTokenMiddleware(apiHandler)         // outermost: pulls token from cookie/header first

	// Mount under /api/
	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", apiHandler))
	mux.Handle("/", beam.Handler())

	addr := config.Addr + ":" + config.Port
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
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
