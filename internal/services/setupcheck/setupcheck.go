// Package setupcheck evaluates local runtime readiness (defaults, backends) for the CLI.
package setupcheck

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/kernel/llmresolver"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

const (
	CategoryDefaults     = "defaults"
	CategoryRegistration = "registration"
	CategoryHealth       = "health"
)

// Input is everything needed to compute readiness; callers gather from DB + runtime state.
type Input struct {
	DefaultModel       string
	DefaultProvider    string
	DefaultAltModel    string
	DefaultAltProvider string
	// DefaultEmbedModel/DefaultEmbedProvider gate the optional workspace
	// index; every issue they produce is a warning (see addEmbeddingIssues).
	DefaultEmbedModel    string
	DefaultEmbedProvider string
	DefaultChain         string
	HITLPolicyName       string
	States               []runtimestate.BackendRuntimeState
	// RegisteredBackendCount, if non-nil, overrides len(RegisteredBackends) / len(States)
	// for BackendCount. CLI doctor sets this from ListBackends when runtime state sync is unavailable.
	RegisteredBackendCount *int
	RegisteredBackends     []runtimetypes.Backend
	// ResolvedFrom records where workspace-scoped keys came from: "workspace" or "global".
	// Keys are camelCase JSON names: "defaultChain", "hitlPolicyName".
	ResolvedFrom map[string]string
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
	// EmbedModels lists the backend's embedding-capable models, reported
	// separately since chat and embedding models are usually disjoint sets.
	EmbedModelCount int      `json:"embedModelCount,omitempty"`
	EmbedModels     []string `json:"embedModels,omitempty"`
	Error           string   `json:"error,omitempty"`
	Hint            string   `json:"hint,omitempty"`
}

// Result is returned by GET /setup-status and contenox doctor.
type Result struct {
	DefaultModel           string            `json:"defaultModel"`
	DefaultProvider        string            `json:"defaultProvider"`
	DefaultMaxOutputTokens int               `json:"defaultMaxOutputTokens,omitempty"`
	DefaultEmbedModel      string            `json:"defaultEmbedModel,omitempty"`
	DefaultEmbedProvider   string            `json:"defaultEmbedProvider,omitempty"`
	DefaultChain           string            `json:"defaultChain"`
	HITLPolicyName         string            `json:"hitlPolicyName"`
	BackendCount           int               `json:"backendCount"`
	ReachableBackendCount  int               `json:"reachableBackendCount"`
	Issues                 []Issue           `json:"issues"`
	BackendChecks          []BackendCheck    `json:"backendChecks,omitempty"`
	ResolvedFrom           map[string]string `json:"resolvedFrom,omitempty"`
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

// Evaluate returns readiness from gathered input (no I/O). Embedding-model
// issues are appended last and unconditionally, regardless of which early
// return fired in evaluateCore.
func Evaluate(in Input) Result {
	r := evaluateCore(in)
	addEmbeddingIssues(&r)
	return r
}

func evaluateCore(in Input) Result {
	r := Result{
		DefaultModel:           strings.TrimSpace(in.DefaultModel),
		DefaultProvider:        strings.TrimSpace(in.DefaultProvider),
		DefaultMaxOutputTokens: ResolveMaxOutputTokens(in.States, in.DefaultProvider, in.DefaultModel),
		DefaultEmbedModel:      strings.TrimSpace(in.DefaultEmbedModel),
		DefaultEmbedProvider:   strings.TrimSpace(in.DefaultEmbedProvider),
		DefaultChain:           strings.TrimSpace(in.DefaultChain),
		HITLPolicyName:         strings.TrimSpace(in.HITLPolicyName),
		BackendCount:           len(in.RegisteredBackends),
		ResolvedFrom:           in.ResolvedFrom,
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
			Code:     "missing_default_model",
			Severity: "error",
			Category: CategoryDefaults,
			Message:  "No default model is set. Internal chat and chains using {{var:model}} need it.",
			// Defaults live on the Settings page, not Backends — a
			// registered backend alone never sets a default.
			FixPath:    "/settings",
			CLICommand: "contenox setup   # guided; or: contenox config set default-model <name>",
		})
	}
	if r.DefaultProvider == "" {
		addIssue(&r, Issue{
			Code:       "missing_default_provider",
			Severity:   "error",
			Category:   CategoryDefaults,
			Message:    "No default provider is set. Internal chat and chains using {{var:provider}} need it.",
			FixPath:    "/settings",
			CLICommand: "contenox setup   # guided; or: contenox config set default-provider ollama",
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

// OverlayEffectiveDefaults credits an effective default model/provider
// supplied out-of-band (e.g. CLI flags) but never persisted to KV config:
// for each empty persisted default, a non-empty override fills it and
// clears the corresponding "missing default" issue. Empty overrides are
// ignored; a persisted default is never overwritten.
func OverlayEffectiveDefaults(res Result, model, provider string) Result {
	model = strings.TrimSpace(model)
	provider = strings.TrimSpace(provider)

	dropModel := res.DefaultModel == "" && model != ""
	if dropModel {
		res.DefaultModel = model
	}
	dropProvider := res.DefaultProvider == "" && provider != ""
	if dropProvider {
		res.DefaultProvider = provider
	}
	if !dropModel && !dropProvider {
		return res
	}

	// Rebuild into a fresh slice so the caller's Issues backing array is untouched.
	kept := make([]Issue, 0, len(res.Issues))
	for _, iss := range res.Issues {
		if dropModel && iss.Code == "missing_default_model" {
			continue
		}
		if dropProvider && iss.Code == "missing_default_provider" {
			continue
		}
		kept = append(kept, iss)
	}
	res.Issues = kept
	return res
}

// ResolveMaxOutputTokens returns the known output-token ceiling for the active
// provider/model from already-synced runtime state. It returns 0 when unknown.
func ResolveMaxOutputTokens(states []runtimestate.BackendRuntimeState, provider, model string) int {
	provider = modelrepo.CanonicalBackendType(provider)
	model = strings.TrimSpace(model)
	if provider == "" || model == "" {
		return 0
	}
	normalizedModel := llmresolver.NormalizeModelName(model)
	for _, state := range states {
		if strings.TrimSpace(state.Error) != "" || !providerTypeMatches(provider, state.Backend.Type) {
			continue
		}
		for _, pulled := range state.PulledModels {
			name := strings.TrimSpace(pulled.Model)
			if name == "" {
				name = strings.TrimSpace(pulled.Name)
			}
			if name == "" {
				continue
			}
			if name == model || llmresolver.NormalizeModelName(name) == normalizedModel {
				if pulled.MaxOutputTokens > 0 {
					return pulled.MaxOutputTokens
				}
				return 0
			}
		}
	}
	return 0
}

func providerTypeMatches(provider, backendType string) bool {
	provider = modelrepo.CanonicalBackendType(provider)
	backendType = modelrepo.CanonicalBackendType(backendType)
	if provider == backendType {
		return true
	}
	return provider == "vertex" && strings.HasPrefix(backendType, "vertex-")
}

// blockingIssue reports whether an issue prevents a usable agent: any
// error-severity issue, plus no_backends (emitted as a warning, but chat/run/ACP
// cannot resolve any model without at least one backend).
func blockingIssue(iss Issue) bool {
	return iss.Severity == "error" || iss.Code == "no_backends"
}

// BlockingIssues returns the issues that make the runtime not ready, in the
// order Evaluate produced them. It performs no I/O.
func (r Result) BlockingIssues() []Issue {
	var out []Issue
	for _, iss := range r.Issues {
		if blockingIssue(iss) {
			out = append(out, iss)
		}
	}
	return out
}

// Ready reports whether the runtime has a usable default model and provider with
// a reachable backend. It reads the already-computed Result — no I/O, and never a
// model completion — and is the shared readiness predicate for doctor, chat/run
// preflight, and the setup wizard.
func (r Result) Ready() bool {
	return len(r.BlockingIssues()) == 0
}

// Summary renders a concise, human-readable readiness report for a chat/terminal
// surface (e.g. the ACP /doctor command). It reads the already-computed Result —
// no I/O and no model completion.
func (r Result) Summary() string {
	model := r.DefaultModel
	if model == "" {
		model = "(unset)"
	}
	provider := r.DefaultProvider
	if provider == "" {
		provider = "(unset)"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Default model:    %s\n", model)
	fmt.Fprintf(&b, "Default provider: %s\n", provider)
	fmt.Fprintf(&b, "Backends:         %d/%d reachable", r.ReachableBackendCount, r.BackendCount)

	if r.Ready() {
		b.WriteString("\n\n✓ Ready — provider reachable and a chat model is available.")
		return b.String()
	}

	b.WriteString("\n\n⚠ Not ready:")
	for _, iss := range r.BlockingIssues() {
		fmt.Fprintf(&b, "\n  • %s", iss.Message)
		if iss.CLICommand != "" {
			fmt.Fprintf(&b, "\n    Try: %s", iss.CLICommand)
		}
	}
	b.WriteString("\n\nRun `contenox doctor` for full backend diagnostics.")
	return b.String()
}

func addDefaultProviderIssues(r *Result) {
	defaultProvider := modelrepo.CanonicalBackendType(r.DefaultProvider)
	if defaultProvider == "" {
		return
	}

	defaultChecks := filterBackendChecks(r.BackendChecks, func(check BackendCheck) bool {
		return modelrepo.CanonicalBackendType(check.Type) == defaultProvider
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
		Code:     "default_model_not_available",
		Severity: "error",
		Category: CategoryHealth,
		Message:  fmt.Sprintf("Default model %q is not currently available for provider %q. Available chat models: %s.", r.DefaultModel, r.DefaultProvider, available),
		// The backend is reachable; the misconfiguration is the default
		// model choice, picked on the Settings page, not Backends.
		FixPath:    "/settings",
		CLICommand: cmd,
	})
}

// addEmbeddingIssues reports readiness of the optional retrieval path (the
// workspace index behind `contenox index` / `contenox search`). Every issue
// is severity "warning" and none blocks Ready(): an agent that never
// searches must stay ready with no embedding model at all.
func addEmbeddingIssues(r *Result) {
	if r.DefaultEmbedModel == "" {
		addIssue(r, Issue{
			Code:     "embed_model_unset",
			Severity: "warning",
			Category: CategoryDefaults,
			Message: "No embedding model is set, so workspace indexing falls back to the chat model — which embeds only on some providers. " +
				"Retrieval (contenox index / search) is optional; nothing else depends on this.",
			FixPath:    "/settings",
			CLICommand: "contenox config set default-embed-model <name>",
		})
		return
	}

	// The embedding provider defaults to the chat provider (resolveEmbeddingModel).
	embedProvider := r.DefaultEmbedProvider
	if embedProvider == "" {
		embedProvider = r.DefaultProvider
	}
	canonical := modelrepo.CanonicalBackendType(embedProvider)
	if canonical == "" {
		return
	}
	reachable := filterBackendChecks(r.BackendChecks, func(check BackendCheck) bool {
		return check.Reachable && providerTypeMatches(canonical, check.Type)
	})
	// Nothing observed for this provider yet: the provider-level issues already cover it.
	if len(reachable) == 0 {
		return
	}

	available := collectEmbedModelNames(reachable)
	if len(available) == 0 {
		addIssue(r, Issue{
			Code:     "embed_model_not_available",
			Severity: "warning",
			Category: CategoryHealth,
			Message: fmt.Sprintf("Embedding model %q is set, but provider %q reports no embedding-capable models on backend(s): %s. Workspace indexing will fail until one is available.",
				r.DefaultEmbedModel, embedProvider, joinBackendNames(reachable)),
			FixPath:    providerFixPathForChecks(canonical, reachable),
			CLICommand: embedModelCommand(canonical, r.DefaultEmbedModel),
		})
		return
	}
	if modelNamePresent(available, r.DefaultEmbedModel) {
		return
	}
	addIssue(r, Issue{
		Code:     "embed_model_not_available",
		Severity: "warning",
		Category: CategoryHealth,
		Message: fmt.Sprintf("Embedding model %q is not currently available for provider %q. Available embedding models: %s.",
			r.DefaultEmbedModel, embedProvider, strings.Join(available, ", ")),
		FixPath:    "/settings",
		CLICommand: fmt.Sprintf("contenox config set default-embed-model %q", available[0]),
	})
}

// embedModelCommand suggests how to obtain an embedding model for a provider —
// pulling one locally on Ollama, listing what the account exposes elsewhere.
func embedModelCommand(provider, wanted string) string {
	switch provider {
	case "ollama":
		return fmt.Sprintf("ollama pull %s   # or another embedding model, then: contenox model list", wanted)
	default:
		return "contenox model list   # confirm which embedding models the provider exposes"
	}
}

func addIssue(r *Result, issue Issue) {
	r.Issues = append(r.Issues, issue)
}

func buildBackendChecks(registered []runtimetypes.Backend, states []runtimestate.BackendRuntimeState, defaultProvider string) []BackendCheck {
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

	stateByID := make(map[string]runtimestate.BackendRuntimeState, len(states))
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
			DefaultProvider: modelrepo.CanonicalBackendType(backend.Type) == modelrepo.CanonicalBackendType(defaultProvider),
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
		check.EmbedModels = embedModelNamesOnState(state)
		check.EmbedModelCount = len(check.EmbedModels)

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

func countChatModelsOnState(state runtimestate.BackendRuntimeState) int {
	n := 0
	for _, model := range state.PulledModels {
		if model.CanChat {
			n++
		}
	}
	return n
}

func chatModelNamesOnState(state runtimestate.BackendRuntimeState) []string {
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

// embedModelNamesOnState lists embedding-capable models, deduplicated and sorted.
func embedModelNamesOnState(state runtimestate.BackendRuntimeState) []string {
	seen := map[string]struct{}{}
	var names []string
	for _, model := range state.PulledModels {
		if !model.CanEmbed {
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

func collectEmbedModelNames(checks []BackendCheck) []string {
	seen := map[string]struct{}{}
	var models []string
	for _, check := range checks {
		for _, model := range check.EmbedModels {
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
		switch modelrepo.CanonicalBackendType(backend.Type) {
		case "openai", "anthropic", "gemini":
			return fmt.Sprintf("Save credentials on Cloud providers, or re-add backend %q after exporting the provider API key.", backend.Name)
		case "vertex-google":
			return fmt.Sprintf("Backend %q uses ADC (Application Default Credentials). Run: gcloud auth application-default login", backend.Name)
		case "bedrock":
			return fmt.Sprintf("Backend %q uses the ambient AWS credential chain (env / profile / IAM role). Verify with: aws sts get-caller-identity", backend.Name)
		case "ollama":
			if isHostedOllamaBackend(backend) {
				return fmt.Sprintf("Save the Ollama Cloud API key on Cloud providers, or re-add backend %q after exporting OLLAMA_API_KEY.", backend.Name)
			}
			return fmt.Sprintf("Store credentials for backend %q, then rerun the backend cycle.", backend.Name)
		default:
			return fmt.Sprintf("Store credentials for backend %q, then rerun the backend cycle.", backend.Name)
		}
	case backendErrorAuth:
		switch modelrepo.CanonicalBackendType(backend.Type) {
		case "openai", "anthropic", "gemini":
			return fmt.Sprintf("The stored API key for backend %q was rejected. Update the key on Cloud providers.", backend.Name)
		case "vertex-google":
			return fmt.Sprintf("ADC credentials for backend %q were rejected. Refresh with: gcloud auth application-default login", backend.Name)
		case "bedrock":
			return fmt.Sprintf("AWS credentials for backend %q were rejected, or the model isn't enabled. Check: aws sts get-caller-identity and Bedrock console model access.", backend.Name)
		case "ollama":
			if isHostedOllamaBackend(backend) {
				return fmt.Sprintf("The stored Ollama Cloud API key for backend %q was rejected. Update the key on Cloud providers.", backend.Name)
			}
			return fmt.Sprintf("Check credentials or auth headers for backend %q.", backend.Name)
		default:
			return fmt.Sprintf("Check credentials or auth headers for backend %q.", backend.Name)
		}
	case backendErrorUnreachable:
		switch modelrepo.CanonicalBackendType(backend.Type) {
		case "vertex-google":
			return fmt.Sprintf("Check connectivity to Vertex AI and confirm GOOGLE_CLOUD_PROJECT is set. Backend %q URL: %s", backend.Name, backend.BaseURL)
		case "bedrock":
			return fmt.Sprintf("Check connectivity to Bedrock and that the URL region matches an enabled region. Backend %q URL: %s", backend.Name, backend.BaseURL)
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
	switch modelrepo.CanonicalBackendType(provider) {
	case "openai", "anthropic", "bedrock", "gemini", "vertex-google":
		return "/backends?tab=cloud-providers"
	default:
		return "/backends?tab=backends"
	}
}

func providerFixPathForChecks(provider string, checks []BackendCheck) string {
	switch modelrepo.CanonicalBackendType(provider) {
	case "openai", "anthropic", "bedrock", "gemini", "vertex-google":
		return "/backends?tab=cloud-providers"
	case "ollama":
		if anyHostedOllamaCheck(checks) {
			return "/backends?tab=cloud-providers"
		}
	}
	return "/backends?tab=backends"
}

func providerAddCommand(provider string) string {
	switch modelrepo.CanonicalBackendType(provider) {
	case "openai":
		return "contenox backend add openai --type openai --api-key-env OPENAI_API_KEY"
	case "anthropic":
		return "contenox backend add anthropic --type anthropic --api-key-env ANTHROPIC_API_KEY"
	case "gemini":
		return "contenox backend add gemini --type gemini --api-key-env GEMINI_API_KEY"
	case "vertex-google":
		return fmt.Sprintf("gcloud auth application-default login && contenox backend add %s --type %s --url \"https://aiplatform.googleapis.com/v1/projects/$GOOGLE_CLOUD_PROJECT/locations/global\"   # or regional (data residency): --url \"https://{REGION}-aiplatform.googleapis.com/v1/projects/$GOOGLE_CLOUD_PROJECT/locations/{REGION}\"; models differ per endpoint: contenox model list", provider, provider)
	case "bedrock":
		return "contenox backend add bedrock --type bedrock --url \"https://bedrock-runtime.us-east-1.amazonaws.com\"   # uses the ambient AWS credential chain"
	default:
		return "contenox backend add ollama --type ollama  # or: contenox backend add ollama-cloud --type ollama --url https://ollama.com/api --api-key-env OLLAMA_API_KEY"
	}
}

func noChatModelsCommand(provider string) string {
	switch modelrepo.CanonicalBackendType(provider) {
	case "openai", "anthropic", "gemini":
		return "contenox model list   # confirm which chat models the provider exposes"
	case "vertex-google":
		return "contenox model list   # Gemini models from AI Studio metadata; set default-model to a gemini-* name"
	case "bedrock":
		return "Enable the model in the AWS Bedrock console (Model access), then: contenox model list   # Bedrock returns AccessDeniedException until the model is enabled for your account"
	default:
		return "contenox model list   # if empty, pull a chat model (e.g. ollama pull " + DefaultOllamaSuggestModel + ")"
	}
}

func primaryDiagnosticCommand(provider string) string {
	switch modelrepo.CanonicalBackendType(provider) {
	case "openai", "anthropic", "gemini":
		return "contenox doctor --json   # inspect backendChecks.error for the provider backend"
	case "vertex-google":
		return "gcloud auth application-default print-access-token   # verify ADC is working; also check GOOGLE_CLOUD_PROJECT is set"
	case "bedrock":
		return "aws sts get-caller-identity   # verify AWS creds resolve; then check model access in the Bedrock console"
	default:
		return "contenox backend list   # verify URL, then inspect runtime errors on the backend"
	}
}

func repairBackendCommand(check *BackendCheck) string {
	if check == nil {
		return ""
	}

	backendType := modelrepo.CanonicalBackendType(check.Type)
	switch backendType {
	case "ollama":
		if isHostedOllamaCheck(*check) {
			return fmt.Sprintf("export OLLAMA_API_KEY=... && contenox backend remove %q && contenox backend add %q --type ollama --url %q --api-key-env OLLAMA_API_KEY", check.Name, check.Name, chooseBaseURL(check.BaseURL, "https://ollama.com/api"))
		}
		return ""
	case "openai":
		return fmt.Sprintf("export OPENAI_API_KEY=... && contenox backend remove %q && contenox backend add %q --type openai --url %q --api-key-env OPENAI_API_KEY", check.Name, check.Name, chooseBaseURL(check.BaseURL, "https://api.openai.com/v1"))
	case "anthropic":
		return fmt.Sprintf("export ANTHROPIC_API_KEY=... && contenox backend remove %q && contenox backend add %q --type anthropic --url %q --api-key-env ANTHROPIC_API_KEY", check.Name, check.Name, chooseBaseURL(check.BaseURL, "https://api.anthropic.com"))
	case "gemini":
		return fmt.Sprintf("export GEMINI_API_KEY=... && contenox backend remove %q && contenox backend add %q --type gemini --url %q --api-key-env GEMINI_API_KEY", check.Name, check.Name, chooseBaseURL(check.BaseURL, "https://generativelanguage.googleapis.com"))
	case "vertex-google":
		return fmt.Sprintf("gcloud auth application-default login && contenox backend remove %q && contenox backend add %q --type %s --url %q", check.Name, check.Name, backendType, check.BaseURL)
	case "bedrock":
		return fmt.Sprintf("# ensure AWS creds resolve (aws sts get-caller-identity), then:\ncontenox backend remove %q && contenox backend add %q --type bedrock --url %q", check.Name, check.Name, chooseBaseURL(check.BaseURL, "https://bedrock-runtime.us-east-1.amazonaws.com"))
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
	switch modelrepo.CanonicalBackendType(provider) {
	case "ollama":
		return "Ollama"
	case "openai":
		return "OpenAI"
	case "anthropic":
		return "Anthropic"
	case "bedrock":
		return "AWS Bedrock"
	case "gemini":
		return "Gemini"
	case "vllm":
		return "vLLM"
	case "vertex-google":
		return "Vertex AI (Google)"
	default:
		return "backend"
	}
}
