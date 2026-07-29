package vscodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	libbus "github.com/contenox/contenox/internal/libbus"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/models/providerservice"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

const ProtocolVersion = 1

type Server struct {
	db           libdb.DBManager
	store        runtimetypes.Store
	stateDir     string
	workspaceID  string
	workspaceCWD string
	version      string

	buildRuntime RuntimeBuilder
	runtimeMu    sync.Mutex
	runtime      *Runtime

	framer *framer
	sendMu sync.Mutex

	approvals *ApprovalBroker
	events    *bridgeEventSink

	clientReqMu      sync.Mutex
	clientReqNextID  int64
	clientReqPending map[string]chan clientResponse

	sessionCfgMu sync.Mutex
	sessionThink map[string]string

	turnMu    sync.Mutex
	turns     map[string]context.CancelFunc
	requestTo map[string]turnInfo

	rpcMu      sync.Mutex
	rpcCancels map[string]context.CancelFunc
	asyncWG    sync.WaitGroup

	policyNames []string
}

const defaultHITLPolicyName = "hitl-policy-default.json"

type ServerConfig struct {
	DB             libdb.DBManager
	StateDir       string
	WorkspaceID    string
	WorkspaceCWD   string
	Version        string
	RuntimeBuilder RuntimeBuilder
	PolicyNames    []string
}

func New(cfg ServerConfig) (*Server, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("vscodeagent: DB is required")
	}
	s := &Server{
		db:               cfg.DB,
		store:            runtimetypes.New(cfg.DB.WithoutTransaction()),
		stateDir:         cfg.StateDir,
		workspaceID:      cfg.WorkspaceID,
		workspaceCWD:     cfg.WorkspaceCWD,
		version:          cfg.Version,
		buildRuntime:     cfg.RuntimeBuilder,
		sessionThink:     make(map[string]string),
		turns:            make(map[string]context.CancelFunc),
		requestTo:        make(map[string]turnInfo),
		rpcCancels:       make(map[string]context.CancelFunc),
		clientReqPending: make(map[string]chan clientResponse),
		policyNames:      append([]string(nil), cfg.PolicyNames...),
	}
	s.approvals = NewApprovalBroker(s.requestPermission, s.activeHITLPolicy, s.sessionIDFromContext)
	s.events = &bridgeEventSink{server: s}
	return s, nil
}

func (s *Server) Run(ctx context.Context, r io.Reader, w io.Writer) error {
	f := newFramer(r, w)
	s.framer = f
	defer func() {
		s.cancelAllTurns()
		s.closeRuntime()
		s.closeClientRequests()
	}()
	for {
		payload, err := f.readPayload()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(ctx.Err(), context.Canceled) {
				s.cancelAllRPCRequests()
				s.asyncWG.Wait()
				return nil
			}
			_ = s.send(errorResponse(nil, ErrParseError, err.Error(), nil))
			continue
		}

		if handled, err := s.handleClientResponsePayload(payload); err != nil {
			_ = s.send(errorResponse(nil, ErrParseError, err.Error(), nil))
			continue
		} else if handled {
			continue
		}

		var req request
		if err := json.Unmarshal(payload, &req); err != nil {
			_ = s.send(errorResponse(nil, ErrParseError, err.Error(), nil))
			continue
		}
		if req.JSONRPC != jsonrpcVersion || strings.TrimSpace(req.Method) == "" {
			if req.ID != nil {
				_ = s.send(errorResponse(rawID(req.ID), ErrInvalidRequest, "invalid JSON-RPC request", nil))
			}
			continue
		}
		if req.Method == "$/cancelRequest" {
			rpcErr := s.cancelRPCRequest(req.Params)
			if req.ID != nil {
				if rpcErr != nil {
					if err := s.send(errorResponse(rawID(req.ID), rpcErr.Code, rpcErr.Message, rpcErr.Data)); err != nil {
						return err
					}
				} else if err := s.send(response{JSONRPC: jsonrpcVersion, ID: rawID(req.ID), Result: map[string]bool{"ok": true}}); err != nil {
					return err
				}
			}
			continue
		}
		if req.ID != nil && isAsyncRequest(req.Method) {
			s.dispatchAsync(ctx, req)
			continue
		}

		result, rpcErr, shutdown := s.handle(ctx, req)
		if req.ID != nil {
			if rpcErr != nil {
				if err := s.send(errorResponse(rawID(req.ID), rpcErr.Code, rpcErr.Message, rpcErr.Data)); err != nil {
					return err
				}
			} else if err := s.send(response{JSONRPC: jsonrpcVersion, ID: rawID(req.ID), Result: result}); err != nil {
				return err
			}
		}
		if shutdown {
			s.cancelAllRPCRequests()
			s.asyncWG.Wait()
			return nil
		}
	}
}

func isAsyncRequest(method string) bool {
	return method == "autocomplete"
}

func (s *Server) dispatchAsync(ctx context.Context, req request) {
	reqCtx, cancel := context.WithCancel(ctx)
	idKey := rpcIDKey(req.ID)
	if idKey != "" {
		s.registerRPCRequest(idKey, cancel)
	}
	s.asyncWG.Add(1)
	go func() {
		defer s.asyncWG.Done()
		if idKey != "" {
			defer s.unregisterRPCRequest(idKey)
		} else {
			defer cancel()
		}
		result, rpcErr, _ := s.handle(reqCtx, req)
		if req.ID == nil {
			return
		}
		if rpcErr != nil {
			_ = s.send(errorResponse(rawID(req.ID), rpcErr.Code, rpcErr.Message, rpcErr.Data))
			return
		}
		_ = s.send(response{JSONRPC: jsonrpcVersion, ID: rawID(req.ID), Result: result})
	}()
}

func (s *Server) send(v any) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.framer == nil {
		return nil
	}
	return s.framer.writeMessage(v)
}

func (s *Server) notify(method string, params any) error {
	return s.send(notification{JSONRPC: jsonrpcVersion, Method: method, Params: params})
}

type cancelRequestParams struct {
	ID json.RawMessage `json:"id"`
}

func (s *Server) cancelRPCRequest(raw json.RawMessage) *responseError {
	var params cancelRequestParams
	if err := strictDecode(raw, &params); err != nil {
		return invalidParams(err)
	}
	idKey := rpcIDKey(params.ID)
	if idKey == "" {
		return &responseError{Code: ErrInvalidParams, Message: "cancel request id is required"}
	}
	s.rpcMu.Lock()
	cancel := s.rpcCancels[idKey]
	s.rpcMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (s *Server) handle(ctx context.Context, req request) (any, *responseError, bool) {
	switch req.Method {
	case "initialize":
		var params initializeParams
		if err := strictDecode(req.Params, &params); err != nil {
			return nil, invalidParams(err), false
		}
		return s.initialize(ctx, params), nil, false
	case "health":
		if err := strictDecode(req.Params, &struct{}{}); err != nil {
			return nil, invalidParams(err), false
		}
		return s.health(ctx), nil, false
	case "getConfig":
		if err := strictDecode(req.Params, &struct{}{}); err != nil {
			return nil, invalidParams(err), false
		}
		return s.getConfig(ctx), nil, false
	case "setConfig":
		var params setConfigParams
		if err := strictDecode(req.Params, &params); err != nil {
			return nil, invalidParams(err), false
		}
		result, err := s.setConfig(ctx, params)
		if err != nil {
			return nil, &responseError{Code: ErrInvalidParams, Message: err.Error()}, false
		}
		return result, nil, false
	case "listHitlPolicies":
		if err := strictDecode(req.Params, &struct{}{}); err != nil {
			return nil, invalidParams(err), false
		}
		return s.listHitlPolicies(ctx), nil, false
	case "listCommands":
		if err := strictDecode(req.Params, &struct{}{}); err != nil {
			return nil, invalidParams(err), false
		}
		return listSlashCommands(), nil, false
	case "listMCPServers":
		if err := strictDecode(req.Params, &struct{}{}); err != nil {
			return nil, invalidParams(err), false
		}
		result, err := s.listMCPServers(ctx)
		if err != nil {
			return nil, &responseError{Code: ErrInternal, Message: err.Error()}, false
		}
		return result, nil, false
	case "listProviders":
		if err := strictDecode(req.Params, &struct{}{}); err != nil {
			return nil, invalidParams(err), false
		}
		result, err := s.listProviders(ctx)
		if err != nil {
			return nil, &responseError{Code: ErrInternal, Message: err.Error()}, false
		}
		return result, nil, false
	case "listModels":
		var params listModelsParams
		if err := strictDecode(req.Params, &params); err != nil {
			return nil, invalidParams(err), false
		}
		result, err := s.listModels(ctx, params)
		if err != nil {
			return nil, &responseError{Code: ErrInternal, Message: err.Error()}, false
		}
		return result, nil, false
	case "sessionCreate":
		var params sessionCreateParams
		if err := strictDecode(req.Params, &params); err != nil {
			return nil, invalidParams(err), false
		}
		result, err := s.sessionCreate(ctx, params)
		if err != nil {
			return nil, &responseError{Code: ErrInvalidParams, Message: err.Error()}, false
		}
		return result, nil, false
	case "sessionList":
		if err := strictDecode(req.Params, &struct{}{}); err != nil {
			return nil, invalidParams(err), false
		}
		result, err := s.sessionList(ctx)
		if err != nil {
			return nil, &responseError{Code: ErrInternal, Message: err.Error()}, false
		}
		return result, nil, false
	case "sessionLoad":
		var params sessionLoadParams
		if err := strictDecode(req.Params, &params); err != nil {
			return nil, invalidParams(err), false
		}
		result, err := s.sessionLoad(ctx, params)
		if err != nil {
			return nil, &responseError{Code: ErrInvalidParams, Message: err.Error()}, false
		}
		return result, nil, false
	case "sessionRead":
		var params sessionReadParams
		if err := strictDecode(req.Params, &params); err != nil {
			return nil, invalidParams(err), false
		}
		result, err := s.sessionRead(ctx, params)
		if err != nil {
			return nil, &responseError{Code: ErrInvalidParams, Message: err.Error()}, false
		}
		return result, nil, false
	case "sessionDelete":
		var params sessionDeleteParams
		if err := strictDecode(req.Params, &params); err != nil {
			return nil, invalidParams(err), false
		}
		result, err := s.sessionDelete(ctx, params)
		if err != nil {
			return nil, &responseError{Code: ErrInvalidParams, Message: err.Error()}, false
		}
		return result, nil, false
	case "chatSend":
		var params chatSendParams
		if err := strictDecode(req.Params, &params); err != nil {
			return nil, invalidParams(err), false
		}
		result, err := s.chatSend(ctx, params)
		if err != nil {
			return nil, &responseError{Code: ErrInvalidParams, Message: err.Error()}, false
		}
		return result, nil, false
	case "chatCancel":
		var params chatCancelParams
		if err := strictDecode(req.Params, &params); err != nil {
			return nil, invalidParams(err), false
		}
		return s.chatCancel(params), nil, false
	case "autocomplete":
		var params autocompleteParams
		if err := strictDecode(req.Params, &params); err != nil {
			return nil, invalidParams(err), false
		}
		result, err := s.autocomplete(ctx, params)
		if err != nil {
			return nil, &responseError{Code: ErrInternal, Message: err.Error()}, false
		}
		return result, nil, false
	case "shutdown":
		if err := strictDecode(req.Params, &struct{}{}); err != nil {
			return nil, invalidParams(err), false
		}
		return map[string]bool{"ok": true}, nil, true
	default:
		return nil, &responseError{Code: ErrMethodNotFound, Message: "method not found: " + req.Method}, false
	}
}

func errorResponse(id *json.RawMessage, code int, message string, data any) response {
	return response{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Error: &responseError{
			Code:    code,
			Message: message,
			Data:    data,
		},
	}
}

func invalidParams(err error) *responseError {
	return &responseError{Code: ErrInvalidParams, Message: "invalid params: " + err.Error()}
}

type initializeParams struct {
	ClientInfo    *clientInfo `json:"clientInfo,omitempty"`
	Workspace     string      `json:"workspace,omitempty"`
	WorkspacePath string      `json:"workspacePath,omitempty"`
}

type clientInfo struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

type initializeResult struct {
	ProtocolVersion int            `json:"protocolVersion"`
	ServerVersion   string         `json:"serverVersion"`
	StateDir        string         `json:"stateDir"`
	WorkspaceID     string         `json:"workspaceId"`
	WorkspaceMode   string         `json:"workspaceMode"`
	Capabilities    capabilities   `json:"capabilities"`
	Config          configSnapshot `json:"config"`
}

type capabilities struct {
	Config       bool `json:"config"`
	Providers    bool `json:"providers"`
	Models       bool `json:"models"`
	Chat         bool `json:"chat"`
	Autocomplete bool `json:"autocomplete"`
	HITL         bool `json:"hitl"`
	SessionList  bool `json:"sessionList"`
	Commands     bool `json:"commands"`
	MCP          bool `json:"mcp"`
}

func (s *Server) initialize(ctx context.Context, params initializeParams) initializeResult {
	if path := strings.TrimSpace(params.WorkspacePath); path != "" {
		if abs, err := filepath.Abs(path); err == nil {
			s.workspaceCWD = abs
		}
	}
	runtimeCapable := s.buildRuntime != nil
	return initializeResult{
		ProtocolVersion: ProtocolVersion,
		ServerVersion:   s.version,
		StateDir:        s.stateDir,
		WorkspaceID:     s.workspaceID,
		WorkspaceMode:   "global",
		Capabilities: capabilities{
			Config:       true,
			Providers:    true,
			Models:       true,
			Chat:         runtimeCapable,
			Autocomplete: runtimeCapable,
			HITL:         runtimeCapable,
			SessionList:  true,
			Commands:     true,
			MCP:          true,
		},
		Config: s.getConfig(ctx),
	}
}

type healthResult struct {
	Status             string         `json:"status"`
	Configured         bool           `json:"configured"`
	DefaultProvider    string         `json:"defaultProvider,omitempty"`
	DefaultModel       string         `json:"defaultModel,omitempty"`
	Config             configSnapshot `json:"config"`
	ConfiguredBackends int            `json:"configuredBackends"`
}

func (s *Server) health(ctx context.Context) healthResult {
	cfg := s.getConfig(ctx)
	backends, _ := s.store.ListAllBackends(ctx)
	status := "ok"
	configured := cfg.DefaultModel != "" && cfg.DefaultProvider != ""
	if !configured {
		status = "setup_required"
	}
	return healthResult{
		Status:             status,
		Configured:         configured,
		DefaultProvider:    cfg.DefaultProvider,
		DefaultModel:       cfg.DefaultModel,
		Config:             cfg,
		ConfiguredBackends: len(backends),
	}
}

type configSnapshot struct {
	DefaultProvider             string            `json:"defaultProvider,omitempty"`
	DefaultModel                string            `json:"defaultModel,omitempty"`
	DefaultAltProvider          string            `json:"defaultAltProvider,omitempty"`
	DefaultAltModel             string            `json:"defaultAltModel,omitempty"`
	DefaultAutocompleteProvider string            `json:"defaultAutocompleteProvider,omitempty"`
	DefaultAutocompleteModel    string            `json:"defaultAutocompleteModel,omitempty"`
	DefaultMaxTokens            string            `json:"defaultMaxTokens,omitempty"`
	DefaultThink                string            `json:"defaultThink,omitempty"`
	DefaultChain                string            `json:"defaultChain,omitempty"`
	HITLPolicyName              string            `json:"hitlPolicyName,omitempty"`
	Scopes                      map[string]string `json:"scopes,omitempty"`
}

func (s *Server) getConfig(ctx context.Context) configSnapshot {
	read := func(key string) (string, string) {
		return clikv.ReadConfig(ctx, s.store, s.workspaceID, key)
	}
	defaultProvider, defaultProviderScope := read("default-provider")
	defaultModel, defaultModelScope := read("default-model")
	defaultAltProvider, defaultAltProviderScope := read("default-alt-provider")
	defaultAltModel, defaultAltModelScope := read("default-alt-model")
	defaultAutocompleteProvider, defaultAutocompleteProviderScope := read("default-autocomplete-provider")
	defaultAutocompleteModel, defaultAutocompleteModelScope := read("default-autocomplete-model")
	defaultMaxTokens, defaultMaxTokensScope := read("default-max-tokens")
	defaultThink, defaultThinkScope := read("default-think")
	defaultChain, defaultChainScope := read("default-chain")
	hitlPolicyName := clikv.ReadHITLPolicy(ctx, s.store)
	return configSnapshot{
		DefaultProvider:             defaultProvider,
		DefaultModel:                defaultModel,
		DefaultAltProvider:          defaultAltProvider,
		DefaultAltModel:             defaultAltModel,
		DefaultAutocompleteProvider: defaultAutocompleteProvider,
		DefaultAutocompleteModel:    defaultAutocompleteModel,
		DefaultMaxTokens:            defaultMaxTokens,
		DefaultThink:                defaultThink,
		DefaultChain:                defaultChain,
		HITLPolicyName:              hitlPolicyName,
		Scopes: map[string]string{
			"defaultProvider":             defaultProviderScope,
			"defaultModel":                defaultModelScope,
			"defaultAltProvider":          defaultAltProviderScope,
			"defaultAltModel":             defaultAltModelScope,
			"defaultAutocompleteProvider": defaultAutocompleteProviderScope,
			"defaultAutocompleteModel":    defaultAutocompleteModelScope,
			"defaultMaxTokens":            defaultMaxTokensScope,
			"defaultThink":                defaultThinkScope,
			"defaultChain":                defaultChainScope,
			"hitlPolicyName":              "global",
		},
	}
}

type setConfigParams struct {
	DefaultProvider             *string `json:"defaultProvider,omitempty"`
	DefaultModel                *string `json:"defaultModel,omitempty"`
	DefaultAltProvider          *string `json:"defaultAltProvider,omitempty"`
	DefaultAltModel             *string `json:"defaultAltModel,omitempty"`
	DefaultAutocompleteProvider *string `json:"defaultAutocompleteProvider,omitempty"`
	DefaultAutocompleteModel    *string `json:"defaultAutocompleteModel,omitempty"`
	DefaultMaxTokens            *string `json:"defaultMaxTokens,omitempty"`
	DefaultThink                *string `json:"defaultThink,omitempty"`
	DefaultChain                *string `json:"defaultChain,omitempty"`
	HITLPolicyName              *string `json:"hitlPolicyName,omitempty"`
}

func (s *Server) setConfig(ctx context.Context, params setConfigParams) (configSnapshot, error) {
	writes := []struct {
		key string
		val *string
	}{
		{"default-provider", params.DefaultProvider},
		{"default-model", params.DefaultModel},
		{"default-alt-provider", params.DefaultAltProvider},
		{"default-alt-model", params.DefaultAltModel},
		{"default-autocomplete-provider", params.DefaultAutocompleteProvider},
		{"default-autocomplete-model", params.DefaultAutocompleteModel},
		{"default-max-tokens", params.DefaultMaxTokens},
		{"default-think", params.DefaultThink},
		{"default-chain", params.DefaultChain},
		{"hitl-policy-name", params.HITLPolicyName},
	}
	for _, write := range writes {
		if write.val == nil {
			continue
		}
		if write.key == "hitl-policy-name" {
			if err := clikv.SetHITLPolicy(ctx, s.store, *write.val); err != nil {
				return configSnapshot{}, fmt.Errorf("write %s: %w", write.key, err)
			}
			continue
		}
		if err := clikv.WriteConfig(ctx, s.store, s.workspaceID, write.key, *write.val); err != nil {
			return configSnapshot{}, fmt.Errorf("write %s: %w", write.key, err)
		}
	}
	s.resetRuntime()
	snapshot := s.getConfig(ctx)
	_ = s.notify("configChanged", snapshot)
	return snapshot, nil
}

type providerInfo struct {
	Provider             string `json:"provider"`
	Configured           bool   `json:"configured"`
	BackendID            string `json:"backendId,omitempty"`
	BackendName          string `json:"backendName,omitempty"`
	BaseURL              string `json:"baseUrl,omitempty"`
	RequiresBaseURL      bool   `json:"requiresBaseUrl"`
	RequiresSecretConfig bool   `json:"requiresSecretConfig"`
	RecommendedAPIKeyEnv string `json:"recommendedApiKeyEnv,omitempty"`
	DefaultProvider      string `json:"defaultProvider,omitempty"`
	DefaultModel         string `json:"defaultModel,omitempty"`
}

type listProvidersResult struct {
	Providers []providerInfo `json:"providers"`
}

func (s *Server) listProviders(ctx context.Context) (listProvidersResult, error) {
	svc := providerservice.New(s.db, s.workspaceID)
	caps, err := svc.ListSupportedProviders(ctx)
	if err != nil {
		return listProvidersResult{}, err
	}
	configs, err := svc.ListProviderConfigs(ctx, nil, 1000)
	if err != nil {
		return listProvidersResult{}, err
	}
	byProvider := make(map[string]*providerservice.ProviderStatus, len(configs))
	for _, cfg := range configs {
		byProvider[cfg.Provider] = cfg
	}

	out := make([]providerInfo, 0, len(caps))
	for _, cap := range caps {
		info := providerInfo{
			Provider:             cap.Provider,
			RequiresBaseURL:      cap.RequiresBaseURL,
			RequiresSecretConfig: cap.RequiresSecretConfig,
			RecommendedAPIKeyEnv: cap.RecommendedAPIKeyEnv,
		}
		if cfg := byProvider[cap.Provider]; cfg != nil {
			info.Configured = cfg.Configured
			info.BackendID = cfg.BackendID
			info.BackendName = cfg.BackendName
			info.BaseURL = cfg.BaseURL
			info.DefaultProvider = cfg.DefaultProvider
			info.DefaultModel = cfg.DefaultModel
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return listProvidersResult{Providers: out}, nil
}

type listModelsParams struct {
	Provider string `json:"provider,omitempty"`
}

type modelInfo struct {
	ID            string          `json:"id"`
	Provider      string          `json:"provider,omitempty"`
	Name          string          `json:"name"`
	DisplayName   string          `json:"displayName"`
	ContextLength int             `json:"contextLength,omitempty"`
	Capabilities  map[string]bool `json:"capabilities"`
	Source        string          `json:"source"`
}

type listModelsResult struct {
	Models []modelInfo `json:"models"`
}

func (s *Server) listModels(ctx context.Context, params listModelsParams) (listModelsResult, error) {
	provider := strings.TrimSpace(params.Provider)
	cfg := s.getConfig(ctx)

	out := make([]modelInfo, 0, 16)
	seen := map[string]bool{}

	// Live provider catalog first, so provider-attributed models win. This mirrors
	// `contenox model list` (contenoxcli.printLiveModels): the picker used to read
	// only store.ListAllModels (persisted declared/observed rows), which never
	// contains a provider whose models live solely in its live catalog — notably
	// vertex-google (the Vertex AI publisher catalog), and any cloud provider
	// before a chat has been sent. That is why Vertex models were missing here.
	//
	// DRIFT HAZARD: "which providers exist / what models they expose" has exactly
	// one source of truth — the runtime's backend catalog (modelrepo catalog
	// providers, reconciled by runtimestate). Do not hand-list providers or models
	// in this bridge; derive them, as below and as providerservice does for
	// listProviders. FOLLOW-UP: the VS Code path is converging onto ACP, after
	// which provider/model pickers should be fed by ACP config-options backed by a
	// shared live-model service, retiring this bespoke listModels RPC entirely.
	for _, m := range s.liveBackendModels(ctx, provider) {
		if seen[m.Name] {
			continue
		}
		seen[m.Name] = true
		out = append(out, m)
	}

	// Persisted observed/declared models that are not attributed to a live backend.
	// These carry no provider label, so a provider is never invented for them and
	// they are shown regardless of the provider filter.
	models, err := s.store.ListAllModels(ctx)
	if err != nil {
		return listModelsResult{}, err
	}
	for _, model := range models {
		name := strings.TrimSpace(model.Model)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, modelInfo{
			ID:            name,
			Name:          name,
			DisplayName:   displayModelName(name),
			ContextLength: model.ContextLength,
			Capabilities: map[string]bool{
				"chat":   model.CanChat,
				"embed":  model.CanEmbed,
				"prompt": model.CanPrompt,
				"stream": model.CanStream,
			},
			Source: "observed",
		})
	}

	// The configured default may not have been observed on any live backend yet
	// (e.g. the provider is unreachable right now), so surface it explicitly.
	if cfg.DefaultModel != "" && !seen[cfg.DefaultModel] && (provider == "" || provider == cfg.DefaultProvider) {
		out = append(out, modelInfo{
			ID:          cfg.DefaultModel,
			Provider:    cfg.DefaultProvider,
			Name:        cfg.DefaultModel,
			DisplayName: displayModelName(cfg.DefaultModel),
			Capabilities: map[string]bool{
				"chat": true,
			},
			Source: "config",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DisplayName < out[j].DisplayName })
	return listModelsResult{Models: out}, nil
}

// liveBackendModels runs one backend reconciliation cycle and returns the models
// each configured backend currently advertises, mirroring `contenox model list`.
// This is how a provider whose catalog is not persisted as model rows — most
// importantly vertex-google, whose models come from the Vertex AI publisher
// catalog — reaches the model picker. Best effort: an unreachable backend simply
// contributes nothing, so a slow or offline provider never fails the picker.
func (s *Server) liveBackendModels(ctx context.Context, provider string) []modelInfo {
	bus := libbus.NewSQLite(s.db.WithoutTransaction())
	defer bus.Close()
	state, err := runtimestate.New(ctx, s.db, bus,
		runtimestate.WithSkipDeleteUndeclaredModels(),
		runtimestate.WithAutoDiscoverModels(),
	)
	if err != nil {
		return nil
	}
	// Partial results are still useful, so a cycle error is non-fatal.
	_ = state.RunBackendCycle(ctx)

	var out []modelInfo
	seen := map[string]bool{}
	for _, bs := range state.Get(ctx) {
		canonical := modelrepo.CanonicalBackendType(bs.Backend.Type)
		if provider != "" && provider != canonical {
			continue
		}
		if len(bs.PulledModels) > 0 {
			for _, pm := range bs.PulledModels {
				name := strings.TrimSpace(pm.Model)
				if name == "" || seen[name] {
					continue
				}
				seen[name] = true
				out = append(out, modelInfo{
					ID:            name,
					Provider:      canonical,
					Name:          name,
					DisplayName:   displayModelName(name),
					ContextLength: pm.ContextLength,
					Capabilities: map[string]bool{
						"chat":   pm.CanChat,
						"embed":  pm.CanEmbed,
						"prompt": pm.CanPrompt,
						"stream": pm.CanStream,
					},
					Source: "observed",
				})
			}
			continue
		}
		// Some providers only report bare model names (no capability detail).
		for _, name := range bs.Models {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, modelInfo{
				ID:          name,
				Provider:    canonical,
				Name:        name,
				DisplayName: displayModelName(name),
				Capabilities: map[string]bool{
					"chat": true,
				},
				Source: "observed",
			})
		}
	}
	return out
}

func displayModelName(name string) string {
	name = strings.TrimSpace(name)
	if strings.HasPrefix(name, "models/") {
		return strings.TrimPrefix(name, "models/")
	}
	return name
}
