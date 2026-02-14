package runtimesdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/contenox/vibe/affinitygroupservice"
	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/backendservice"
	"github.com/contenox/vibe/openaichatservice"
	"github.com/contenox/vibe/downloadservice"
	"github.com/contenox/vibe/embedservice"
	"github.com/contenox/vibe/eventmappingservice"
	"github.com/contenox/vibe/eventsourceservice"
	"github.com/contenox/vibe/execservice"
	"github.com/contenox/vibe/executor"
	"github.com/contenox/vibe/hookproviderservice"
	"github.com/contenox/vibe/modelservice"
	"github.com/contenox/vibe/providerservice"
	"github.com/contenox/vibe/stateservice"
	"github.com/contenox/vibe/taskchainservice"
)

// Client is the main SDK client that provides access to all services
type Client struct {
	BackendService      backendservice.Service
	ModelService        modelservice.Service
	groupService        affinitygroupservice.Service
	HookService         hookproviderservice.Service
	ExecService         execservice.ExecService
	EnvService          execservice.TasksEnvService
	ProviderService     providerservice.Service
	DownloadService     downloadservice.Service
	StateService        stateservice.Service
	EmbedService        embedservice.Service
	TaskChainService    taskchainservice.Service
	ChatService         openaichatservice.Service
	EventSourceService  eventsourceservice.Service
	ExecutorSyncTrigger executor.ExecutorSyncTrigger
	MappingService      eventmappingservice.Service
}

// Config holds configuration for the SDK client
type Config struct {
	BaseURL string
	Token   string
}

// NewClient creates a new SDK client with the provided configuration
func createClient(config Config, httpClient *http.Client) (*Client, error) {
	return &Client{
		BackendService:      NewHTTPBackendService(config.BaseURL, config.Token, httpClient),
		ModelService:        NewHTTPModelService(config.BaseURL, config.Token, httpClient),
		groupService:        NewHTTPgroupService(config.BaseURL, config.Token, httpClient),
		HookService:         NewHTTPRemoteHookService(config.BaseURL, config.Token, httpClient),
		ExecService:         NewHTTPExecService(config.BaseURL, config.Token, httpClient),
		EnvService:          NewHTTPTasksEnvService(config.BaseURL, config.Token, httpClient),
		ProviderService:     NewHTTPProviderService(config.BaseURL, config.Token, httpClient),
		DownloadService:     NewHTTPDownloadService(config.BaseURL, config.Token, httpClient),
		StateService:        NewHTTPStateService(config.BaseURL, config.Token, httpClient),
		EmbedService:        NewHTTPEmbedService(config.BaseURL, config.Token, httpClient),
		TaskChainService:    NewHTTPTaskChainService(config.BaseURL, config.Token, httpClient),
		ChatService:         NewHTTPChatService(config.BaseURL, config.Token, httpClient),
		EventSourceService:  NewHTTPEvenSourceService(config.BaseURL, config.Token, httpClient),
		ExecutorSyncTrigger: NewHTTPExecutorSyncTrigger(config.BaseURL, config.Token, httpClient),
		MappingService:      NewHTTPMappingService(config.BaseURL, config.Token, httpClient),
	}, nil
}

func NewClient(ctx context.Context, config Config, httpClient *http.Client) (*Client, error) {
	// First validate version compatibility
	about, err := fetchServerVersion(ctx, config, httpClient)
	if err != nil {
		return nil, fmt.Errorf("failed to validate server version: %w", err)
	}

	sdkVersion := apiframework.GetVersion()

	// Special case for development (when version is unknown)
	if about.Version == "unknown" || strings.Contains(about.Version, "dev") {
		return createClient(config, httpClient)
	}

	// Enforce exact version match
	if sdkVersion != about.Version {
		return nil, fmt.Errorf(
			"version mismatch: server=%q, sdk=%q (must be identical)\n"+
				"Hint: Run 'go get github.com/contenox/vibe@%s' to fix",
			about.Version,
			sdkVersion,
			about.Version,
		)
	}

	return createClient(config, httpClient)
}

func fetchServerVersion(ctx context.Context, config Config, httpClient *http.Client) (apiframework.AboutServer, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	baseURL := strings.TrimSuffix(config.BaseURL, "/")
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/version", nil)
	if err != nil {
		return apiframework.AboutServer{}, err
	}

	if config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+config.Token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return apiframework.AboutServer{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apiframework.AboutServer{}, fmt.Errorf("server returned status %d", resp.StatusCode)
	}

	var about apiframework.AboutServer
	if err := json.NewDecoder(resp.Body).Decode(&about); err != nil {
		return apiframework.AboutServer{}, err
	}
	return about, nil
}
