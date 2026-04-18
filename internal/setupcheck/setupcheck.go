// Package setupcheck evaluates local runtime readiness (defaults, backends) for Beam and CLI.
package setupcheck

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/llmresolver"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/statetype"
)

const (
	CategoryDefaults     = "defaults"
	CategoryRegistration = "registration"
	CategoryHealth       = "health"
)

// Input is everything needed to compute readiness; callers gather from DB + runtime state.
type Input struct {
	DefaultModel    string
	DefaultProvider string
	DefaultChain    string
	HITLPolicyName  string
	States          []statetype.BackendRuntimeState
	// RegisteredBackendCount, if non-nil, overrides len(RegisteredBackends) / len(States)
	// for BackendCount. CLI doctor sets this from ListBackends when runtime state sync is unavailable.
	RegisteredBackendCount *int
	RegisteredBackends     []runtimetypes.Backend
}

// Issue describes one setup problem and how to fix it.
type Issue struct {
	Code       string `json:"code"`
	Severity   string `json:"severity"`
	Category   string `json:"category,omitempty"`
	Message    string `json:"message"`
	FixPath    string `json:"fixPath,omitempty"`
	CLICommand string `json:"cliCommand,omitempty"`
}

// BackendCheck reports the runtime status of one registered backend.
type BackendCheck struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Type            string   `json:"type"`
	BaseURL         string   `json:"baseUrl"`
	Status          string   `json:"status"`
	Reachable       bool     `json:"reachable"`
	DefaultProvider bool     `json:"defaultProvider"`
	ModelCount      int      `json:"modelCount"`
	ChatModelCount  int      `json:"chatModelCount"`
	ChatModels      []string `json:"chatModels,omitempty"`
	Error           string   `json:"error,omitempty"`
	Hint            string   `json:"hint,omitempty"`
}

// Result is returned by GET /setup-status and contenox doctor.
type Result struct {
	DefaultModel          string         `json:"defaultModel"`
	DefaultProvider       string         `json:"defaultProvider"`
	DefaultChain          string         `json:"defaultChain"`
	HITLPolicyName        string         `json:"hitlPolicyName"`
	BackendCount          int            `json:"backendCount"`
	ReachableBackendCount int            `json:"reachableBackendCount"`
	Issues                []Issue        `json:"issues"`
	BackendChecks         []BackendCheck `json:"backendChecks,omitempty"`
}

type backendErrorKind string

const (
	backendErrorNone          backendErrorKind = ""
	backendErrorPending       backendErrorKind = "pending"
	backendErrorAPIKeyMissing backendErrorKind = "api_key_missing"
	backendErrorAuth          backendErrorKind = "auth"
	backendErrorUnreachable   backendErrorKind = "unreachable"
	backendErrorOther         backendErrorKind = "other"
)

// Evaluate returns readiness from gathered input (no I/O).
func Evaluate(in Input) Result {
	r := Result{
		DefaultModel:    strings.TrimSpace(in.DefaultModel),
		DefaultProvider: strings.TrimSpace(in.DefaultProvider),
		DefaultChain:    strings.TrimSpace(in.DefaultChain),
		HITLPolicyName:  strings.TrimSpace(in.HITLPolicyName),
		BackendCount:    len(in.RegisteredBackends),
	}

	if r.BackendCount == 0 {
		r.BackendCount = len(in.States)
	}
	if in.RegisteredBackendCount != nil {
		r.BackendCount = *in.RegisteredBackendCount
	}

	r.BackendChecks = buildBackendChecks(in.RegisteredBackends, in.States, r.DefaultProvider)
	if len(r.BackendChecks) > 0 {
		r.ReachableBackendCount = countReachableChecks(r.BackendChecks)
	} else {
		for _, s := range in.States {
			if strings.TrimSpace(s.Error) == "" {
				r.ReachableBackendCount++
			}
		}
	}

	if r.DefaultModel == "" {
		addIssue(&r, Issue{
			Code:       "missing_default_model",
			Severity:   "error",
			Category:   CategoryDefaults,
			Message:    "No default model is set. Internal chat and chains using {{var:model}} need it.",
			FixPath:    "/backends",
			CLICommand: "contenox config set default-model <name>",
		})
	}
	if r.DefaultProvider == "" {
		addIssue(&r, Issue{
			Code:       "missing_default_provider",
			Severity:   "error",
			Category:   CategoryDefaults,
			Message:    "No default provider is set. Internal chat and chains using {{var:provider}} need it.",
			FixPath:    "/backends",
			CLICommand: "contenox config set default-provider ollama",
		})
	}

	if r.BackendCount == 0 {
		addIssue(&r, Issue{
			Code:       "no_backends",
			Severity:   "warning",
			Category:   CategoryRegistration,
			Message:    "No LLM backends are registered yet. Saving defaults does not create a backend—you still need at least one in Backends.",
			FixPath:    providerFixPath(r.DefaultProvider),
			CLICommand: providerAddCommand(r.DefaultProvider),
		})
		return r
	}

	if len(in.States) == 0 {
		addIssue(&r, Issue{
			Code:       "runtime_state_empty",
			Severity:   "error",
			Category:   CategoryHealth,
			Message:    "Backends are registered but runtime state has no synced entries yet. Ensure providers are reachable, then run again (do not use --skip-backend-cycle unless you know state is current).",
			FixPath:    "/backends?tab=backends",
			CLICommand: "contenox backend list   # confirm URLs; start Ollama or fix API keys, then retry",
		})
	} else if r.ReachableBackendCount == 0 {
		addIssue(&r, Issue{
			Code:     "all_backends_unreachable",
			Severity: "error",
			Category: CategoryHealth,
			Message:  "Every backend reported an error (e.g. Ollama not running, API key missing, or provider auth failure).",
			FixPath:  "/backends?tab=backends",
		})
	}

	if len(r.BackendChecks) > 0 {
		addDefaultProviderIssues(&r)
	}
	return r
}

func addDefaultProviderIssues(r *Result) {
	defaultProvider := strings.ToLower(strings.TrimSpace(r.DefaultProvider))
	if defaultProvider == "" {
		return
	}

	defaultChecks := filterBackendChecks(r.BackendChecks, func(check BackendCheck) bool {
		return strings.ToLower(strings.TrimSpace(check.Type)) == defaultProvider
	})
	if len(defaultChecks) == 0 && r.BackendCount > 0 {
		addIssue(r, Issue{
			Code:       "default_provider_backend_missing",
			Severity:   "error",
			Category:   CategoryRegistration,
			Message:    fmt.Sprintf("Default provider %q is set, but no registered backend uses that provider. Add a %s backend or change default-provider.", r.DefaultProvider, providerDisplayName(defaultProvider)),
			FixPath:    providerFixPath(defaultProvider),
			CLICommand: providerAddCommand(defaultProvider),
		})
		return
	}
	if len(defaultChecks) == 0 {
		return
	}

	reachableChecks := filterBackendChecks(defaultChecks, func(check BackendCheck) bool { return check.Reachable })
	fixPath := providerFixPathForChecks(defaultProvider, defaultChecks)
	if len(reachableChecks) == 0 {
		switch {
		case anyBackendKind(defaultChecks, backendErrorAPIKeyMissing):
			addIssue(r, Issue{
				Code:       "default_provider_api_key_missing",
				Severity:   "error",
				Category:   CategoryHealth,
				Message:    fmt.Sprintf("Default provider %q cannot be used because its backend credentials are missing. Affected backend(s): %s.", r.DefaultProvider, joinBackendNames(defaultChecks)),
				FixPath:    fixPath,
				CLICommand: repairBackendCommand(firstBackendWithKind(defaultChecks, backendErrorAPIKeyMissing)),
			})
		case anyBackendKind(defaultChecks, backendErrorAuth):
			addIssue(r, Issue{
				Code:       "default_provider_auth_failed",
				Severity:   "error",
				Category:   CategoryHealth,
				Message:    fmt.Sprintf("Default provider %q rejected the stored credentials. Affected backend(s): %s.", r.DefaultProvider, joinBackendNames(filterByKinds(defaultChecks, backendErrorAuth, backendErrorOther))),
				FixPath:    fixPath,
				CLICommand: repairBackendCommand(firstBackendWithKind(defaultChecks, backendErrorAuth)),
			})
		case anyBackendKind(defaultChecks, backendErrorPending):
			addIssue(r, Issue{
				Code:       "default_provider_not_synced",
				Severity:   "error",
				Category:   CategoryHealth,
				Message:    fmt.Sprintf("Default provider %q is registered, but runtime state has not produced an entry for backend(s): %s.", r.DefaultProvider, joinBackendNames(filterByKinds(defaultChecks, backendErrorPending))),
				FixPath:    "/backends?tab=backends",
				CLICommand: "contenox doctor   # rerun after the backend cycle finishes",
			})
		case anyBackendKind(defaultChecks, backendErrorUnreachable), anyBackendKind(defaultChecks, backendErrorOther):
			addIssue(r, Issue{
				Code:       "default_provider_unreachable",
				Severity:   "error",
				Category:   CategoryHealth,
				Message:    fmt.Sprintf("No reachable backend is available for default provider %q. %s", r.DefaultProvider, summarizeBackendFailures(defaultChecks)),
				FixPath:    fixPath,
				CLICommand: primaryDiagnosticCommand(defaultProvider),
			})
		}
		return
	}

	chatModels := collectChatModelNames(reachableChecks)
	if len(chatModels) == 0 {
		addIssue(r, Issue{
			Code:       "no_chat_models",
			Severity:   "error",
			Category:   CategoryHealth,
			Message:    fmt.Sprintf("Default provider %q is reachable, but runtime state contains no chat-capable models for backend(s): %s.", r.DefaultProvider, joinBackendNames(reachableChecks)),
			FixPath:    fixPath,
			CLICommand: noChatModelsCommand(defaultProvider),
		})
		return
	}

	if r.DefaultModel == "" || modelNamePresent(chatModels, r.DefaultModel) {
		return
	}

	available := strings.Join(chatModels, ", ")
	cmd := fmt.Sprintf("contenox config set default-model %q", chatModels[0])
	addIssue(r, Issue{
		Code:       "default_model_not_available",
		Severity:   "error",
		Category:   CategoryHealth,
		Message:    fmt.Sprintf("Default model %q is not currently available for provider %q. Available chat models: %s.", r.DefaultModel, r.DefaultProvider, available),
		FixPath:    "/backends?tab=backends",
		CLICommand: cmd,
	})
}

func addIssue(r *Result, issue Issue) {
	r.Issues = append(r.Issues, issue)
}

func buildBackendChecks(registered []runtimetypes.Backend, states []statetype.BackendRuntimeState, defaultProvider string) []BackendCheck {
	if len(registered) == 0 && len(states) > 0 {
		registered = make([]runtimetypes.Backend, 0, len(states))
		for _, state := range states {
			registered = append(registered, state.Backend)
		}
	}
	if len(registered) == 0 {
		return nil
	}

	registered = append([]runtimetypes.Backend(nil), registered...)
	sort.SliceStable(registered, func(i, j int) bool {
		ni := strings.ToLower(strings.TrimSpace(registered[i].Name))
		nj := strings.ToLower(strings.TrimSpace(registered[j].Name))
		if ni != nj {
			return ni < nj
		}
		return registered[i].ID < registered[j].ID
	})

	stateByID := make(map[string]statetype.BackendRuntimeState, len(states))
	for _, state := range states {
		stateByID[state.Backend.ID] = state
	}

	checks := make([]BackendCheck, 0, len(registered))
	for _, backend := range registered {
		check := BackendCheck{
			ID:              backend.ID,
			Name:            backend.Name,
			Type:            backend.Type,
			BaseURL:         backend.BaseURL,
			DefaultProvider: strings.EqualFold(strings.TrimSpace(backend.Type), strings.TrimSpace(defaultProvider)),
			Status:          "pending",
			Hint:            pendingBackendHint(backend),
		}

		state, ok := stateByID[backend.ID]
		if !ok {
			checks = append(checks, check)
			continue
		}

		check.ModelCount = len(state.PulledModels)
		check.ChatModelCount = countChatModelsOnState(state)
		check.ChatModels = chatModelNamesOnState(state)

		if strings.TrimSpace(state.Error) == "" {
			check.Status = "ready"
			check.Reachable = true
			check.Hint = ""
			checks = append(checks, check)
			continue
		}

		check.Status = "error"
		check.Error = strings.TrimSpace(state.Error)
		check.Hint = backendHint(backend, classifyBackendError(check.Error))
		checks = append(checks, check)
	}

	return checks
}

func countReachableChecks(checks []BackendCheck) int {
	n := 0
	for _, check := range checks {
		if check.Reachable {
			n++
		}
	}
	return n
}

func countChatModelsOnState(state statetype.BackendRuntimeState) int {
	n := 0
	for _, model := range state.PulledModels {
		if model.CanChat {
			n++
		}
	}
	return n
}

func chatModelNamesOnState(state statetype.BackendRuntimeState) []string {
	seen := map[string]struct{}{}
	var names []string
	for _, model := range state.PulledModels {
		if !model.CanChat {
			continue
		}
		name := strings.TrimSpace(model.Model)
		if name == "" {
			name = strings.TrimSpace(model.Name)
		}
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func filterBackendChecks(checks []BackendCheck, keep func(BackendCheck) bool) []BackendCheck {
	var out []BackendCheck
	for _, check := range checks {
		if keep(check) {
			out = append(out, check)
		}
	}
	return out
}

func filterByKinds(checks []BackendCheck, kinds ...backendErrorKind) []BackendCheck {
	allowed := make(map[backendErrorKind]struct{}, len(kinds))
	for _, kind := range kinds {
		allowed[kind] = struct{}{}
	}
	return filterBackendChecks(checks, func(check BackendCheck) bool {
		_, ok := allowed[classifyCheck(check)]
		return ok
	})
}

func anyBackendKind(checks []BackendCheck, kind backendErrorKind) bool {
	for _, check := range checks {
		if classifyCheck(check) == kind {
			return true
		}
	}
	return false
}

func firstBackendWithKind(checks []BackendCheck, kind backendErrorKind) *BackendCheck {
	for i := range checks {
		if classifyCheck(checks[i]) == kind {
			return &checks[i]
		}
	}
	return nil
}

func classifyCheck(check BackendCheck) backendErrorKind {
	switch check.Status {
	case "pending":
		return backendErrorPending
	case "ready":
		return backendErrorNone
	default:
		return classifyBackendError(check.Error)
	}
}

func classifyBackendError(err string) backendErrorKind {
	msg := strings.ToLower(strings.TrimSpace(err))
	switch {
	case msg == "":
		return backendErrorNone
	case strings.Contains(msg, "api key not configured"),
		strings.Contains(msg, "failed to retrieve api key configuration"):
		return backendErrorAPIKeyMissing
	case strings.Contains(msg, "401"),
		strings.Contains(msg, "403"),
		strings.Contains(msg, "unauthorized"),
		strings.Contains(msg, "forbidden"),
		strings.Contains(msg, "invalid api key"),
		strings.Contains(msg, "incorrect api key"),
		strings.Contains(msg, "authentication"):
		return backendErrorAuth
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "context deadline exceeded"),
		strings.Contains(msg, "timeout"),
		strings.Contains(msg, "dial tcp"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "server misbehaving"),
		strings.Contains(msg, "tls"),
		strings.Contains(msg, "eof"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "network is unreachable"):
		return backendErrorUnreachable
	default:
		return backendErrorOther
	}
}

func backendHint(backend runtimetypes.Backend, kind backendErrorKind) string {
	switch kind {
	case backendErrorAPIKeyMissing:
		switch strings.ToLower(strings.TrimSpace(backend.Type)) {
		case "openai", "gemini":
			return fmt.Sprintf("Save credentials on Cloud providers, or re-add backend %q after exporting the provider API key.", backend.Name)
		case "ollama":
			if isHostedOllamaBackend(backend) {
				return fmt.Sprintf("Save the Ollama Cloud API key on Cloud providers, or re-add backend %q after exporting OLLAMA_API_KEY.", backend.Name)
			}
			return fmt.Sprintf("Store credentials for backend %q, then rerun the backend cycle.", backend.Name)
		default:
			return fmt.Sprintf("Store credentials for backend %q, then rerun the backend cycle.", backend.Name)
		}
	case backendErrorAuth:
		switch strings.ToLower(strings.TrimSpace(backend.Type)) {
		case "openai", "gemini":
			return fmt.Sprintf("The stored API key for backend %q was rejected. Update the key on Cloud providers.", backend.Name)
		case "ollama":
			if isHostedOllamaBackend(backend) {
				return fmt.Sprintf("The stored Ollama Cloud API key for backend %q was rejected. Update the key on Cloud providers.", backend.Name)
			}
			return fmt.Sprintf("Check credentials or auth headers for backend %q.", backend.Name)
		default:
			return fmt.Sprintf("Check credentials or auth headers for backend %q.", backend.Name)
		}
	case backendErrorUnreachable:
		switch strings.ToLower(strings.TrimSpace(backend.Type)) {
		case "ollama":
			if isHostedOllamaBackend(backend) {
				return fmt.Sprintf("Check connectivity to Ollama Cloud and confirm the stored API key for backend %q.", backend.Name)
			}
			return fmt.Sprintf("Verify that %s is running at %s.", providerDisplayName(backend.Type), backend.BaseURL)
		case "vllm":
			return fmt.Sprintf("Verify that %s is running at %s.", providerDisplayName(backend.Type), backend.BaseURL)
		default:
			return fmt.Sprintf("Check connectivity and base URL for backend %q (%s).", backend.Name, backend.BaseURL)
		}
	case backendErrorPending:
		return fmt.Sprintf("Backend %q is registered, but runtime state has not synced it yet.", backend.Name)
	default:
		return fmt.Sprintf("Inspect backend %q on the Backends page for the full runtime error.", backend.Name)
	}
}

func pendingBackendHint(backend runtimetypes.Backend) string {
	return fmt.Sprintf("Backend %q is registered, but runtime state has not reported a status yet.", backend.Name)
}

func joinBackendNames(checks []BackendCheck) string {
	names := make([]string, 0, len(checks))
	for _, check := range checks {
		names = append(names, backendLabel(check))
	}
	return strings.Join(names, ", ")
}

func summarizeBackendFailures(checks []BackendCheck) string {
	parts := make([]string, 0, len(checks))
	for _, check := range checks {
		switch classifyCheck(check) {
		case backendErrorPending:
			parts = append(parts, fmt.Sprintf("%s: runtime state not synced yet", backendLabel(check)))
		default:
			if strings.TrimSpace(check.Error) != "" {
				parts = append(parts, fmt.Sprintf("%s: %s", backendLabel(check), check.Error))
			}
		}
		if len(parts) == 2 {
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ")
}

func backendLabel(check BackendCheck) string {
	if name := strings.TrimSpace(check.Name); name != "" {
		return name
	}
	if typ := strings.TrimSpace(check.Type); typ != "" {
		return typ
	}
	if baseURL := strings.TrimSpace(check.BaseURL); baseURL != "" {
		return baseURL
	}
	if id := strings.TrimSpace(check.ID); id != "" {
		return id
	}
	return "backend"
}

func collectChatModelNames(checks []BackendCheck) []string {
	seen := map[string]struct{}{}
	var models []string
	for _, check := range checks {
		for _, model := range check.ChatModels {
			if _, ok := seen[model]; ok {
				continue
			}
			seen[model] = struct{}{}
			models = append(models, model)
		}
	}
	sort.Strings(models)
	return models
}

func modelNamePresent(available []string, wanted string) bool {
	normalizedWanted := llmresolver.NormalizeModelName(wanted)
	for _, candidate := range available {
		if candidate == wanted || llmresolver.NormalizeModelName(candidate) == normalizedWanted {
			return true
		}
	}
	return false
}

func providerFixPath(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai", "gemini":
		return "/backends?tab=cloud-providers"
	default:
		return "/backends?tab=backends"
	}
}

func providerFixPathForChecks(provider string, checks []BackendCheck) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai", "gemini":
		return "/backends?tab=cloud-providers"
	case "ollama":
		if anyHostedOllamaCheck(checks) {
			return "/backends?tab=cloud-providers"
		}
	}
	return "/backends?tab=backends"
}

func providerAddCommand(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai":
		return "contenox backend add openai --type openai --api-key-env OPENAI_API_KEY"
	case "gemini":
		return "contenox backend add gemini --type gemini --api-key-env GEMINI_API_KEY"
	default:
		return "contenox backend add local --type ollama  # or: contenox backend add ollama-cloud --type ollama --url https://ollama.com/api --api-key-env OLLAMA_API_KEY"
	}
}

func noChatModelsCommand(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai", "gemini":
		return "contenox model list   # confirm which chat models the provider exposes"
	default:
		return "contenox model list   # if empty, pull a chat model (e.g. ollama pull " + DefaultOllamaSuggestModel + ")"
	}
}

func primaryDiagnosticCommand(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai", "gemini":
		return "contenox doctor --json   # inspect backendChecks.error for the provider backend"
	default:
		return "contenox backend list   # verify URL, then inspect runtime errors on the backend"
	}
}

func repairBackendCommand(check *BackendCheck) string {
	if check == nil {
		return ""
	}

	backendType := strings.ToLower(strings.TrimSpace(check.Type))
	switch backendType {
	case "ollama":
		if isHostedOllamaCheck(*check) {
			return fmt.Sprintf("export OLLAMA_API_KEY=... && contenox backend remove %q && contenox backend add %q --type ollama --url %q --api-key-env OLLAMA_API_KEY", check.Name, check.Name, chooseBaseURL(check.BaseURL, "https://ollama.com/api"))
		}
		return ""
	case "openai":
		return fmt.Sprintf("export OPENAI_API_KEY=... && contenox backend remove %q && contenox backend add %q --type openai --url %q --api-key-env OPENAI_API_KEY", check.Name, check.Name, chooseBaseURL(check.BaseURL, "https://api.openai.com/v1"))
	case "gemini":
		return fmt.Sprintf("export GEMINI_API_KEY=... && contenox backend remove %q && contenox backend add %q --type gemini --url %q --api-key-env GEMINI_API_KEY", check.Name, check.Name, chooseBaseURL(check.BaseURL, "https://generativelanguage.googleapis.com"))
	default:
		return ""
	}
}

func anyHostedOllamaCheck(checks []BackendCheck) bool {
	return slices.ContainsFunc(checks, isHostedOllamaCheck)
}

func isHostedOllamaCheck(check BackendCheck) bool {
	return strings.EqualFold(strings.TrimSpace(check.Type), "ollama") && isHostedOllamaBaseURL(check.BaseURL)
}

func isHostedOllamaBackend(backend runtimetypes.Backend) bool {
	return strings.EqualFold(strings.TrimSpace(backend.Type), "ollama") && isHostedOllamaBaseURL(backend.BaseURL)
}

func isHostedOllamaBaseURL(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), "ollama.com")
}

func chooseBaseURL(baseURL, fallback string) string {
	if strings.TrimSpace(baseURL) == "" {
		return fallback
	}
	return baseURL
}

func providerDisplayName(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "ollama":
		return "Ollama"
	case "openai":
		return "OpenAI"
	case "gemini":
		return "Gemini"
	case "vllm":
		return "vLLM"
	default:
		return "backend"
	}
}
