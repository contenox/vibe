package acpsvc

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/contenox/contenox/internal/kernel/reasoning"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
)

const (
	configIDModel      = "model"
	configIDHITLPolicy = "hitl-policy"
	configIDThink      = "think"
	configIDTokenLimit = "token-limit"
	configIDAgent      = "agent"

	configCategoryModel      = "model"
	configCategoryHITLPolicy = "_hitl_policy"
	configCategoryThink      = "thought_level"
	configCategoryAgent      = "agent"

	configTypeSelect = "select"

	hitlPolicyDefaultValue = "__contenox_default__"

	// agentNativeValue is the agent select's value for the runtime's own chain
	// engine. It is the empty string, which is already what contenox.agent means
	// by "no agent" everywhere else, so the value round-trips by construction.
	agentNativeValue = ""

	agentCatalogLimit = 100

	// modelConfigDefaultGroup is a placeholder group for "no provider", never a
	// provider name.
	modelConfigDefaultGroup = "default"

	// WorkspaceConfigOptionsMetaKey is the initialize-response `_meta` key
	// advertising session-less config options, so a client can render controls
	// before a session exists.
	WorkspaceConfigOptionsMetaKey = "contenox.workspaceConfigOptions"
)

// workspaceConfigOptions builds the session-less config options advertised at
// initialize time, reusing the per-session builders.
func (t *Transport) workspaceConfigOptions(ctx context.Context) []libacp.SessionConfigOption {
	seed := &sessionEntry{
		Provider: t.provider(),
		Model:    t.model(),
		Think:    t.thinkDefault(),
	}
	seed.driver = &nativeDriver{t: t}
	return t.sessionConfigOptions(ctx, seed)
}

// sessionConfigOptions returns the config options advertised for a session,
// dispatching to its driver.
func (t *Transport) sessionConfigOptions(ctx context.Context, sess *sessionEntry) []libacp.SessionConfigOption {
	return sess.driver.ConfigOptions(ctx, sess)
}

// agentConfigOption advertises the machine's registered agents a client may bind a
// session to. It is present only when there is
// something to choose and is fixed at session/new. Only enabled agents are
// offered, plus the session's own bound agent so a session disabled underneath it
// still renders what it is running as.
func (t *Transport) agentConfigOption(ctx context.Context, sess *sessionEntry) (libacp.SessionConfigOption, bool) {
	reg := t.agentRegistry()
	if reg == nil {
		return libacp.SessionConfigOption{}, false
	}
	agents, err := reg.List(ctx, nil, agentCatalogLimit)
	if err != nil {
		return libacp.SessionConfigOption{}, false
	}

	current := agentNativeValue
	if sess != nil && sess.driver != nil {
		if bound := strings.TrimSpace(sess.driver.AgentName()); bound != "" {
			current = bound
		}
	}

	offered := make([]libacp.SessionConfigValue, 0, len(agents))
	seen := map[string]struct{}{agentNativeValue: {}}
	for _, agent := range agents {
		if agent == nil || !agent.Enabled {
			continue
		}
		name := strings.TrimSpace(agent.Name)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		offered = append(offered, libacp.SessionConfigValue{
			Value:       name,
			Name:        name,
			Description: agentConfigDescription(agent),
		})
	}
	if len(offered) == 0 && current == agentNativeValue {
		return libacp.SessionConfigOption{}, false
	}
	sort.SliceStable(offered, func(i, j int) bool {
		return strings.ToLower(offered[i].Name) < strings.ToLower(offered[j].Name)
	})

	values := make([]libacp.SessionConfigValue, 0, len(offered)+2)
	values = append(values, libacp.SessionConfigValue{
		Value:       agentNativeValue,
		Name:        "Contenox",
		Description: "The runtime's own coding chain",
	})
	values = append(values, offered...)
	if _, ok := seen[current]; !ok {
		values = append(values, libacp.SessionConfigValue{
			Value:       current,
			Name:        current,
			Description: "This session's agent; no longer offered for new sessions",
		})
	}

	return libacp.SessionConfigOption{
		ID:           configIDAgent,
		Name:         "Agent",
		Description:  "Which agent answers this session. Chosen when the session starts and fixed for its lifetime.",
		Category:     configCategoryAgent,
		Type:         configTypeSelect,
		CurrentValue: current,
		Options:      libacp.NewSessionConfigValues(values),
	}, true
}

func agentConfigDescription(agent *runtimetypes.Agent) string {
	switch agent.Kind {
	case runtimetypes.AgentKindChain:
		return "A task chain of this runtime, run as an agent"
	case runtimetypes.AgentKindExternalACP:
		return "An external ACP agent this runtime drives"
	}
	return ""
}

func (t *Transport) modelConfigOption(ctx context.Context, sess *sessionEntry) libacp.SessionConfigOption {
	currentProvider := sess.providerOrDefault(t.provider())
	currentModel := sess.modelOrDefault(t.model())
	current := modelConfigValue(currentProvider, currentModel)
	return libacp.SessionConfigOption{
		ID:           configIDModel,
		Name:         "Model",
		Category:     configCategoryModel,
		Type:         configTypeSelect,
		CurrentValue: current,
		Options:      t.modelConfigValues(ctx, currentProvider, currentModel),
	}
}

func (t *Transport) hitlPolicyConfigOption(sess *sessionEntry) libacp.SessionConfigOption {
	return libacp.SessionConfigOption{
		ID:           configIDHITLPolicy,
		Name:         "HITL Policy",
		Description:  "Approval policy used for gated tool calls",
		Category:     configCategoryHITLPolicy,
		Type:         configTypeSelect,
		CurrentValue: sess.hitlPolicy(),
		Options:      t.hitlPolicyConfigValues(sess),
	}
}

func (t *Transport) thinkConfigOption(sess *sessionEntry) libacp.SessionConfigOption {
	return libacp.SessionConfigOption{
		ID:           configIDThink,
		Name:         "Think",
		Description:  "Reasoning level for this session",
		Category:     configCategoryThink,
		Type:         configTypeSelect,
		CurrentValue: sess.think(),
		Options: libacp.NewSessionConfigValues([]libacp.SessionConfigValue{
			{Value: reasoning.Auto, Name: "Auto"},
			{Value: reasoning.Off, Name: "Off"},
			{Value: reasoning.Minimal, Name: "Minimal"},
			{Value: reasoning.Low, Name: "Low"},
			{Value: reasoning.Medium, Name: "Medium"},
			{Value: reasoning.High, Name: "High"},
			{Value: reasoning.XHigh, Name: "XHigh"},
		}),
	}
}

func (t *Transport) tokenLimitConfigOption(ctx context.Context, sess *sessionEntry) libacp.SessionConfigOption {
	limit := sess.effectiveTokenLimit()
	current := "0"
	if limit > 0 {
		current = strconv.Itoa(limit)
	}
	cap := t.modelContextCap(ctx, sess)
	desc := "Session context budget (token limit for history). Controls shifting and usage indicator size. 0 = chain default / unlimited."
	if cap > 0 {
		desc += fmt.Sprintf(" Capped to model max %d if larger.", cap)
	}
	// ACP v1 has only "select" and "boolean", so offer a ladder of budgets clamped
	// to the model cap.
	return libacp.SessionConfigOption{
		ID:           configIDTokenLimit,
		Name:         "Token Limit",
		Description:  desc,
		Category:     "context",
		Type:         configTypeSelect,
		CurrentValue: current,
		Options:      tokenLimitConfigValues(cap, limit),
	}
}

func tokenLimitConfigValues(cap, current int) libacp.SessionConfigValues {
	values := []libacp.SessionConfigValue{{Value: "0", Name: "Chain default", Description: "Use the chain's token limit (or unlimited)"}}
	seen := map[int]struct{}{0: {}}
	add := func(n int, name, desc string) {
		if n <= 0 {
			return
		}
		if _, ok := seen[n]; ok {
			return
		}
		seen[n] = struct{}{}
		values = append(values, libacp.SessionConfigValue{Value: strconv.Itoa(n), Name: name, Description: desc})
	}
	for _, n := range []int{4096, 8192, 16384, 32768, 65536, 131072, 262144} {
		if cap > 0 && n >= cap {
			break
		}
		add(n, formatTokenCount(n), "")
	}
	if cap > 0 {
		add(cap, formatTokenCount(cap)+" (model max)", "The model's reported context length")
	}
	add(current, formatTokenCount(current)+" (current)", "Session's current custom budget")
	return libacp.NewSessionConfigValues(values)
}

func formatTokenCount(n int) string {
	if n >= 1024 && n%1024 == 0 {
		return strconv.Itoa(n/1024) + "k tokens"
	}
	return strconv.Itoa(n) + " tokens"
}

func (t *Transport) modelContextCap(ctx context.Context, sess *sessionEntry) int {
	if sess == nil {
		return 0
	}
	prov := sess.providerOrDefault(t.provider())
	mod := sess.modelOrDefault(t.model())
	for _, st := range t.runtimeStates(ctx) {
		for _, pm := range st.PulledModels {
			if (pm.Model == mod || pm.Name == mod) && (prov == "" || strings.Contains(strings.ToLower(st.Backend.Type), strings.ToLower(prov)) || prov == "") {
				if pm.ContextLength > 0 {
					return pm.ContextLength
				}
			}
		}
	}
	for _, st := range t.runtimeStates(ctx) {
		for _, pm := range st.PulledModels {
			if pm.Model == mod || pm.Name == mod {
				if pm.ContextLength > 0 {
					return pm.ContextLength
				}
			}
		}
	}
	return 0
}

func (t *Transport) modelConfigValues(ctx context.Context, currentProvider, currentModel string) libacp.SessionConfigValues {
	type modelValue struct {
		provider    string
		value       string
		name        string
		description string
		current     bool
	}

	seen := map[string]modelValue{}
	add := func(provider, model, description string, current bool) {
		value := modelConfigValue(provider, model)
		if strings.TrimSpace(value) == "" {
			return
		}
		name := modelConfigDisplayName(model)
		if existing, ok := seen[value]; ok {
			existing.current = existing.current || current
			if existing.description == "" {
				existing.description = description
			}
			seen[value] = existing
			return
		}
		seen[value] = modelValue{
			provider:    strings.TrimSpace(provider),
			value:       value,
			name:        name,
			description: description,
			current:     current,
		}
	}

	add(currentProvider, currentModel, "Current default model", true)
	if altModel := t.altModel(); altModel != "" {
		altProvider := t.altProvider()
		if altProvider == "" {
			altProvider = currentProvider
		}
		add(altProvider, altModel, "Configured alternate model", false)
	}

	for _, state := range t.runtimeStates(ctx) {
		if strings.TrimSpace(state.Error) != "" {
			continue
		}
		provider := strings.TrimSpace(state.Backend.Type)
		if provider == "" {
			continue
		}
		for _, pulled := range state.PulledModels {
			if !pulled.CanChat && !pulled.CanPrompt {
				continue
			}
			model := strings.TrimSpace(pulled.Model)
			if model == "" {
				model = strings.TrimSpace(pulled.Name)
			}
			add(provider, model, describePulledModel(pulled), false)
		}
	}

	groupsByProvider := map[string][]modelValue{}
	groupNames := map[string]string{}
	for _, value := range seen {
		groupID := value.provider
		if groupID == "" {
			groupID = modelConfigDefaultGroup
		}
		groupsByProvider[groupID] = append(groupsByProvider[groupID], value)
		groupNames[groupID] = groupID
	}
	if len(groupsByProvider) == 0 {
		return libacp.NewSessionConfigValues(nil)
	}

	groupIDs := make([]string, 0, len(groupsByProvider))
	for groupID := range groupsByProvider {
		groupIDs = append(groupIDs, groupID)
	}
	sort.SliceStable(groupIDs, func(i, j int) bool {
		leftCurrent := groupIDs[i] == currentProvider
		rightCurrent := groupIDs[j] == currentProvider
		if leftCurrent != rightCurrent {
			return leftCurrent
		}
		return strings.ToLower(groupIDs[i]) < strings.ToLower(groupIDs[j])
	})

	groups := make([]libacp.SessionConfigGroup, 0, len(groupIDs))
	for _, groupID := range groupIDs {
		values := groupsByProvider[groupID]
		sort.SliceStable(values, func(i, j int) bool {
			if values[i].current != values[j].current {
				return values[i].current
			}
			return strings.ToLower(values[i].name) < strings.ToLower(values[j].name)
		})
		options := make([]libacp.SessionConfigValue, 0, len(values))
		for _, value := range values {
			options = append(options, libacp.SessionConfigValue{
				Value:       value.value,
				Name:        value.name,
				Description: value.description,
			})
		}
		groups = append(groups, libacp.SessionConfigGroup{
			Group:   groupID,
			Name:    groupNames[groupID],
			Options: options,
		})
	}
	return libacp.NewGroupedSessionConfigValues(groups)
}

func (t *Transport) hitlPolicyConfigValues(sess *sessionEntry) libacp.SessionConfigValues {
	defaultName := "Default"
	defaultDescription := "Use Contenox's configured fallback policy"
	if name := strings.TrimSpace(t.deps.HITLDefaultPolicyName); name != "" {
		defaultDescription = "Use " + name
	}
	values := []libacp.SessionConfigValue{{
		Value:       hitlPolicyDefaultValue,
		Name:        defaultName,
		Description: defaultDescription,
	}}

	seen := map[string]struct{}{hitlPolicyDefaultValue: {}}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		values = append(values, libacp.SessionConfigValue{Value: name, Name: hitlPolicyDisplayName(name)})
	}
	for _, name := range t.deps.KnownPolicies {
		add(name)
	}
	add(sess.hitlPolicy())
	return libacp.NewSessionConfigValues(values)
}

// resolveSessionHITLPolicy returns the HITL policy name to enforce for this
// session, or "" to defer to the runtime's configured default.
func (t *Transport) resolveSessionHITLPolicy(sess *sessionEntry) string {
	name := sess.hitlPolicy()
	if name == "" || name == hitlPolicyDefaultValue {
		return strings.TrimSpace(t.deps.HITLDefaultPolicyName)
	}
	return name
}

func (t *Transport) runtimeStates(ctx context.Context) []runtimestate.BackendRuntimeState {
	if t.deps.Engine == nil || t.deps.Engine.State == nil {
		return nil
	}
	// Without this, a backend restarted after startup stays invisible to the model
	// dropdown until another read path triggers a reconcile.
	_ = t.deps.Engine.State.ReconcileIfStale(ctx)
	states := t.deps.Engine.State.Get(ctx)
	out := make([]runtimestate.BackendRuntimeState, 0, len(states))
	for _, state := range states {
		out = append(out, state)
	}
	sort.SliceStable(out, func(i, j int) bool {
		left := strings.ToLower(out[i].Backend.Type + "/" + out[i].Name)
		right := strings.ToLower(out[j].Backend.Type + "/" + out[j].Name)
		return left < right
	})
	return out
}

func (t *Transport) SetSessionConfigOption(ctx context.Context, req libacp.SetSessionConfigOptionRequest) (libacp.SetSessionConfigOptionResponse, error) {
	reportErr, reportChange, end := t.tracker().Start(ctx, "set_config_option", "acp_session", "session_id", string(req.SessionID), "config_id", req.ConfigID)
	defer end()

	sess, ok := t.sessionFor(req.SessionID)
	if !ok {
		err := libacp.NewErrorf(libacp.ErrInvalidParams, "unknown session %q", req.SessionID)
		reportErr(err)
		return libacp.SetSessionConfigOptionResponse{}, err
	}

	if err := sess.driver.SetConfigOption(ctx, sess, req.ConfigID, req.Value); err != nil {
		reportErr(err)
		return libacp.SetSessionConfigOptionResponse{}, err
	}

	reportChange(req.ConfigID, req.Value.AsString())
	return libacp.SetSessionConfigOptionResponse{
		ConfigOptions: t.sessionConfigOptions(ctx, sess),
	}, nil
}

func (t *Transport) setSessionConfigOption(ctx context.Context, sess *sessionEntry, configID, value string) error {
	switch configID {
	case configIDModel:
		if !configOptionHasValue(t.modelConfigOption(ctx, sess), value) {
			return libacp.NewErrorf(libacp.ErrInvalidParams, "unknown model option %q", value)
		}
		provider, model := splitModelConfigValue(value)
		if strings.TrimSpace(model) == "" {
			return libacp.NewErrorf(libacp.ErrInvalidParams, "model option %q has empty model", value)
		}
		sess.setModelSelection(provider, model)
		return nil

	case configIDHITLPolicy:
		if !configOptionHasValue(t.hitlPolicyConfigOption(sess), value) {
			return libacp.NewErrorf(libacp.ErrInvalidParams, "unknown HITL policy option %q", value)
		}
		// Session-scoped: never the global KV, so concurrent sessions behind one
		// shared engine gate independently.
		sess.setHITLPolicy(value)
		return nil

	case configIDThink:
		level, err := reasoning.Normalize(value)
		if err != nil {
			return libacp.NewError(libacp.ErrInvalidParams, err.Error())
		}
		if !configOptionHasValue(t.thinkConfigOption(sess), level) {
			return libacp.NewErrorf(libacp.ErrInvalidParams, "unknown think option %q", value)
		}
		sess.setThink(level)
		return nil

	case configIDTokenLimit:
		requested := 0
		if strings.TrimSpace(value) != "" && value != "0" {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || n < 0 {
				return libacp.NewErrorf(libacp.ErrInvalidParams, "token-limit must be a non-negative integer, got %q", value)
			}
			requested = n
		}
		cap := t.modelContextCap(ctx, sess)
		eff := requested
		if cap > 0 && (eff == 0 || eff > cap) {
			eff = cap
		}
		sess.setEffectiveTokenLimit(eff)
		return nil

	case configIDAgent:
		// Fixed at session/new, like the workspace root.
		return libacp.NewErrorf(libacp.ErrInvalidParams, "the agent cannot be changed after the session starts; start a new session to use a different one")

	default:
		return libacp.NewErrorf(libacp.ErrInvalidParams, "unknown config option %q", configID)
	}
}

func (t *Transport) sendConfigOptionUpdate(ctx context.Context, sid libacp.SessionID, sess *sessionEntry) {
	if t.conn == nil || sess == nil {
		return
	}
	t.sendUpdate(ctx, libacp.SessionNotification{
		SessionID: sid,
		Update: libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateConfigOption,
			ConfigOptions: t.sessionConfigOptions(ctx, sess),
		},
	})
}

const (
	CommandModel    = "model"
	CommandProvider = "provider"
	CommandThink    = "think"
	CommandPolicy   = "policy"
)

// CommandValueDomains projects a client's session config options onto the
// argument domains of /model, /provider, /think and /policy. It is a completion
// aid, not a gate; an absent key means anything is fine.
func CommandValueDomains(options []libacp.SessionConfigOption) map[string][]string {
	out := map[string][]string{}
	for _, option := range options {
		switch option.ID {
		case configIDModel:
			models, providers := modelCommandDomains(option)
			addCommandValues(out, CommandModel, models...)
			addCommandValues(out, CommandProvider, providers...)
		case configIDThink:
			for _, value := range option.Options.AllValues() {
				addCommandValues(out, CommandThink, value.Value)
			}
		case configIDHITLPolicy:
			for _, value := range option.Options.AllValues() {
				if value.Value == hitlPolicyDefaultValue {
					continue
				}
				addCommandValues(out, CommandPolicy, value.Value)
			}
		}
	}
	return out
}

func modelCommandDomains(option libacp.SessionConfigOption) (models, providers []string) {
	for _, group := range option.Options.Groups {
		provider := strings.TrimSpace(group.Group)
		if provider != "" && provider != modelConfigDefaultGroup {
			providers = append(providers, provider)
		}
		for _, value := range group.Options {
			models = append(models, modelFromConfigValue(value.Value, provider))
		}
	}
	// An external session may forward an ungrouped select verbatim.
	for _, value := range option.Options.Values {
		provider, model := splitModelConfigValue(value.Value)
		if provider != "" {
			providers = append(providers, provider)
		}
		models = append(models, model)
	}
	return models, providers
}

func modelFromConfigValue(value, provider string) string {
	value = strings.TrimSpace(value)
	if provider == "" || provider == modelConfigDefaultGroup {
		return value
	}
	return strings.TrimPrefix(value, provider+"/")
}

func addCommandValues(domains map[string][]string, command string, values ...string) {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		existing := domains[command]
		duplicate := false
		for _, seen := range existing {
			if seen == value {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		domains[command] = append(existing, value)
	}
}

func configOptionHasValue(option libacp.SessionConfigOption, value string) bool {
	for _, candidate := range option.Options.AllValues() {
		if candidate.Value == value {
			return true
		}
	}
	return false
}

func modelConfigValue(provider, model string) string {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" {
		return model
	}
	if model == "" {
		return provider
	}
	return provider + "/" + model
}

func modelConfigDisplayName(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return "(default)"
	}
	return strings.TrimPrefix(model, "models/")
}

func splitModelConfigValue(value string) (provider, model string) {
	value = strings.TrimSpace(value)
	if before, after, ok := strings.Cut(value, "/"); ok {
		return strings.TrimSpace(before), strings.TrimSpace(after)
	}
	return "", value
}

func hitlPolicyDisplayName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimSuffix(name, ".json")
	name = strings.TrimPrefix(name, "hitl-policy-")
	if name == "" {
		return "Default"
	}
	return name
}

func describePulledModel(model runtimestate.ModelPullStatus) string {
	var parts []string
	if model.ContextLength > 0 {
		parts = append(parts, "context "+strconv.Itoa(model.ContextLength))
	}
	if model.MaxOutputTokens > 0 {
		parts = append(parts, "output ceiling "+strconv.Itoa(model.MaxOutputTokens))
	}
	if model.CanThink {
		parts = append(parts, "thinking")
	}
	return strings.Join(parts, ", ")
}
