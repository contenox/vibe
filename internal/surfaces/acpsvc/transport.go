package acpsvc

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/nativeturn"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/models/llmrepo"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/chatservice"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/shellsession"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
)

type Deps struct {
	Engine        *enginesvc.Engine
	DB            libdb.DBManager
	ChainRegistry *ChainRegistry
	// FIMChainRegistry supplies the fill-in-the-middle chain the
	// `_contenox/autocomplete` extension method runs. Nil disables
	// autocomplete: the method reports a clean method-not-found error rather
	// than a panic. Independent of ChainRegistry so the completion model can
	// differ from the chat model (see LoadFIMChainRegistry).
	FIMChainRegistry   *ChainRegistry
	DefaultModel       string
	DefaultProvider    string
	DefaultAltModel    string
	DefaultAltProvider string
	DefaultMaxTokens   string
	DefaultThink       string
	WorkspaceID        string
	// ContenoxDir is the active .contenox directory for auxiliary chains
	// (e.g. chain-compact.json for /compact).
	ContenoxDir string

	// WorkspaceRoots allowlists session cwd roots. Nil accepts any absolute
	// cwd (the stdio path); the sentinel "/" and "" resolve to the default root.
	WorkspaceRoots *vfs.Factory

	// ShellSessions manages per-chat-session PTY shells. Nil disables shell
	// tooling: extension methods report method-not-found rather than erroring.
	ShellSessions shellsession.Manager

	// KnownPolicies are the HITL policy preset names /policy lists. Display only.
	KnownPolicies []string
	// HITLDefaultPolicyName is the engine's fallback policy, shown by /policy.
	// Display only.
	HITLDefaultPolicyName string

	// UpdateBanner is an optional one-shot agent_message_chunk sent on the
	// first session created or loaded. Empty = no banner.
	UpdateBanner string

	// EnvSetup enables the env_var auth method (Vars advertised at
	// initialize, Complete finishes setup non-interactively). Nil disables it.
	EnvSetup *EnvSetupSpec

	// SessionRouter is the process-shared (contenox session -> transport)
	// registry a shared engine routes HITL approvals through. Nil on the
	// stdio path (one transport, bound directly).
	SessionRouter *SessionRouter

	// Instances owns external-agent instances off any single connection, so
	// the agent's process survives a client disconnect/reload and a reload
	// re-attaches. Nil falls back to a connCtx-bound spawn (stdio behavior).
	Instances agentinstance.Manager

	// NativeTurns is the survival layer for native turns: prompts run on this
	// serve-rooted Registry, so a client drop no longer cancels the chain —
	// the Transport attaches as a viewer, and only session/cancel or delete
	// cancels the turn. Nil falls back to a connection-bound turn.
	NativeTurns *nativeturn.Registry

	// Fleet, when set, is what `/mission` fires through (fleetservice.Dispatch,
	// narrowed to Dispatch only). Nil for a dispatched unit or a setup-only editor.
	Fleet MissionDispatcher

	// Agents resolves a declared agent by name for /mission's grammar. Fleet
	// and Agents together gate whether `/mission` is advertised and usable.
	Agents MissionAgentResolver
}

// EnvSetupSpec describes environment-variable-based setup (the non-interactive
// sibling of the terminal setup wizard).
type EnvSetupSpec struct {
	Vars     []libacp.AuthEnvVar
	Complete func(ctx context.Context) error
}

type sessionEntry struct {
	mu                sync.Mutex
	WorkspaceID       string
	Cwd               string
	InternalSessionID string
	McpServerNames    []string
	Provider          string
	Model             string
	Think             string
	// EffectiveTokenLimit is the user-chosen (or chain default) context
	// budget, clamped to the model's ContextLength when known. 0 means chain
	// default / unlimited; shown in usage indicators as "size".
	EffectiveTokenLimit int
	// HITLPolicy is the per-session HITL policy ("" = the default sentinel),
	// injected into the prompt context so a shared engine gates each session
	// independently. Must never touch the global cli.hitl-policy-name KV.
	HITLPolicy string

	// MissionID is the mission this session is a dispatched unit of (from
	// session/new `_meta`), scoping its mission tools; "" for ordinary chat.
	MissionID string

	// ModelAllowlist / BackendAllowlist are the mission envelope's compute
	// bound, applied on every turn context where a model is actually chosen.
	// Nil (no binding) for ordinary chat or an unbounded mission.
	ModelAllowlist   []string
	BackendAllowlist []string

	// FiredMissions marks that this session dispatched missions of its own,
	// unlocking the supervisor tools.
	FiredMissions bool

	// driver is the execution backend (native chain or external downstream
	// agent), chosen once at construction; all paths dispatch through it.
	driver sessionDriver
}

// sessionDriver is the per-session execution backend a sessionEntry delegates
// to. Implementations share the sessionEntry passed to each call; only the
// execution mechanism differs.
type sessionDriver interface {
	// Prompt runs one full turn for sess, owning update relay, cancellation
	// registration, and history persistence.
	Prompt(ctx context.Context, req libacp.PromptRequest, sess *sessionEntry) (libacp.PromptResponse, error)
	// ConfigOptions returns the options advertised for sess: native selects,
	// or the downstream agent's own options for external.
	ConfigOptions(ctx context.Context, sess *sessionEntry) []libacp.SessionConfigOption
	// SetConfigOption applies a change: native mutates the session's own
	// selection; external forwards downstream and adopts the confirmed set.
	// value carries the wire union (string or boolean).
	SetConfigOption(ctx context.Context, sess *sessionEntry, configID string, value libacp.SessionConfigOptionValue) error
	// AvailableCommands returns the slash-command menu, or nil for external
	// sessions (whose menu is relayed live via available_commands_update).
	AvailableCommands() []libacp.AvailableCommand
	// AgentName is the registered external agent name, or "" for native.
	AgentName() string
	// Close releases connection-local resources. Idempotent: native is a
	// no-op, external closes the downstream Handle.
	Close() error
}

type Transport struct {
	deps Deps
	conn *libacp.AgentSideConnection
	// connectionID scopes client-supplied MCP servers to this ACP connection so
	// two clients loading the same session cannot overwrite each other's tools.
	connectionID string

	// connCtx binds spawned external agents to this connection; connCancel
	// fires on the connection's Closed signal (the reliable teardown hook —
	// serve never calls Transport.Close).
	connCtx    context.Context
	connCancel context.CancelFunc

	initMu     sync.Mutex
	clientInfo *libacp.Implementation
	clientCaps libacp.ClientCapabilities

	sessionMu       sync.Mutex
	sessions        map[libacp.SessionID]*sessionEntry
	contenoxToACPID map[string]libacp.SessionID

	// cfgMu guards the live model/provider, mutated by /model and /provider
	// while concurrent prompts read them. Seeded from Deps once at construction.
	cfgMu              sync.Mutex
	defaultModel       string
	defaultProvider    string
	defaultAltModel    string
	defaultAltProvider string
	defaultMaxTokens   string
	defaultThink       string

	permMu      sync.Mutex
	permPending map[string]struct{}

	toolCallMu     sync.Mutex
	toolCallStatus map[string]libacp.ToolCallStatus
	// toolCallSeq / toolCallOpen disambiguate repeated invocations of a tool
	// with no engine-minted ApprovalID: the name alone would reuse one wire id
	// per run, merging cards and pinning status at the first completion's rank.
	toolCallSeq  map[string]int
	toolCallOpen map[string]int

	bannerMu      sync.Mutex
	pendingBanner string

	// termSubMu guards termSubs, the per-session cancel funcs for live
	// terminal-output subscriptions. Re-subscribing cancels the prior one.
	termSubMu sync.Mutex
	termSubs  map[libacp.SessionID]func()

	// promptCancelMu guards promptCancels, the per-session canceller for the
	// in-flight turn: session/cancel, Close/Delete, or a connection drop abort
	// through it rather than relying solely on libacp's promptCtx
	// substitution. One turn per session; a superseding registration cancels
	// the stale one.
	promptCancelMu sync.Mutex
	promptCancels  map[libacp.SessionID]*inflightPrompt

	// acAgent is a test seam: when set, _contenox/autocomplete uses it instead
	// of building a fresh agentservice.Agent from deps. Nil in production.
	acAgent agentservice.Agent
}

// inflightPrompt is a running turn's cancellation registration. Pointer
// identity is used for symmetric unregister so an ended turn never removes a
// newer turn's registration.
type inflightPrompt struct {
	cancel context.CancelFunc
}

func permKey(sid libacp.SessionID, toolCallID string) string {
	return string(sid) + "\x00" + toolCallID
}

func (t *Transport) markPermissionPending(sid libacp.SessionID, toolCallID string) {
	t.permMu.Lock()
	if t.permPending == nil {
		t.permPending = make(map[string]struct{})
	}
	t.permPending[permKey(sid, toolCallID)] = struct{}{}
	t.permMu.Unlock()
}

func (t *Transport) clearPermissionPending(sid libacp.SessionID, toolCallID string) {
	t.permMu.Lock()
	delete(t.permPending, permKey(sid, toolCallID))
	t.permMu.Unlock()
}

func (t *Transport) sendToolCallUpdateGuarded(ctx context.Context, sid libacp.SessionID, toolCallID string, notif libacp.SessionNotification) {
	t.permMu.Lock()
	defer t.permMu.Unlock()
	if _, pending := t.permPending[permKey(sid, toolCallID)]; pending {
		return
	}
	t.sendUpdate(ctx, notif)
}

// isPermissionPending reports whether a permission dialog is open here for
// (sid, toolCallID). The native-turn viewer consults it at delivery time —
// survival-path translation runs off-connection, so it cannot rely on
// sendToolCallUpdateGuarded's inline check.
func (t *Transport) isPermissionPending(sid libacp.SessionID, toolCallID string) bool {
	t.permMu.Lock()
	defer t.permMu.Unlock()
	_, pending := t.permPending[permKey(sid, toolCallID)]
	return pending
}

func New(deps Deps) libacp.AgentFactory {
	return func(conn *libacp.AgentSideConnection) libacp.Agent {
		connCtx, connCancel := context.WithCancel(context.Background())
		t := &Transport{
			deps:               deps,
			conn:               conn,
			connectionID:       newSessionID("conn"),
			connCtx:            connCtx,
			connCancel:         connCancel,
			sessions:           make(map[libacp.SessionID]*sessionEntry),
			contenoxToACPID:    make(map[string]libacp.SessionID),
			toolCallStatus:     make(map[string]libacp.ToolCallStatus),
			defaultModel:       deps.DefaultModel,
			defaultProvider:    deps.DefaultProvider,
			defaultAltModel:    deps.DefaultAltModel,
			defaultAltProvider: deps.DefaultAltProvider,
			defaultMaxTokens:   deps.DefaultMaxTokens,
			defaultThink:       deps.DefaultThink,
			pendingBanner:      deps.UpdateBanner,
			termSubs:           make(map[libacp.SessionID]func()),
			promptCancels:      make(map[libacp.SessionID]*inflightPrompt),
		}
		// The `!` shell passthrough and contenox-namespaced requests arrive as
		// ACP extension methods (see terminal.go); unknown ones answer
		// MethodNotFound since this handler only claims the contenox namespace.
		conn.SetExtRequestHandler(t.handleExtRequest)
		// Cancelling connCtx tears down every external-agent subprocess spawned
		// on it, for both stdio and serve WS (which never calls Transport.Close).
		go func() {
			<-conn.Closed()
			connCancel()
		}()
		return t
	}
}

// takeBanner atomically reads and clears the pending update banner.
// Returns "" after the first call, ensuring the banner is sent at most once.
func (t *Transport) takeBanner() string {
	t.bannerMu.Lock()
	defer t.bannerMu.Unlock()
	b := t.pendingBanner
	t.pendingBanner = ""
	return b
}

// model returns the live default model, which /model may have changed since
// startup. Safe for concurrent reads/writes against the command handlers.
func (t *Transport) model() string {
	t.cfgMu.Lock()
	defer t.cfgMu.Unlock()
	return t.defaultModel
}

// provider returns the live default provider, which /provider may have changed.
func (t *Transport) provider() string {
	t.cfgMu.Lock()
	defer t.cfgMu.Unlock()
	return t.defaultProvider
}

func (t *Transport) altModel() string {
	t.cfgMu.Lock()
	defer t.cfgMu.Unlock()
	return t.defaultAltModel
}

func (t *Transport) altProvider() string {
	t.cfgMu.Lock()
	defer t.cfgMu.Unlock()
	return t.defaultAltProvider
}

func (t *Transport) maxTokens() string {
	t.cfgMu.Lock()
	defer t.cfgMu.Unlock()
	return t.defaultMaxTokens
}

// chainTemplateVars seeds the template vars every chain execution needs, so
// default_model/default_provider ({{var:alt_model|var:default_model}} and the
// provider equivalent) are always set when a model is known.
func (t *Transport) chainTemplateVars(sess *sessionEntry) map[string]string {
	vars := map[string]string{
		"model":    sess.modelOrDefault(t.model()),
		"provider": sess.providerOrDefault(t.provider()),
	}
	// default_model/default_provider must be the session-effective selection,
	// not the transport default, or recovery tasks resolve a stale provider
	// while the main tasks use the session's working one.
	if vars["model"] != "" {
		vars["default_model"] = vars["model"]
	}
	if vars["provider"] != "" {
		vars["default_provider"] = vars["provider"]
	}
	if altModel := t.altModel(); altModel != "" {
		vars["alt_model"] = altModel
	}
	if altProvider := t.altProvider(); altProvider != "" {
		vars["alt_provider"] = altProvider
	}
	if maxTokens := t.maxTokens(); maxTokens != "" {
		vars["max_tokens"] = maxTokens
	}
	return vars
}

func (t *Transport) setModel(v string) {
	t.cfgMu.Lock()
	t.defaultModel = v
	t.cfgMu.Unlock()
}

func (t *Transport) setProvider(v string) {
	t.cfgMu.Lock()
	t.defaultProvider = v
	t.cfgMu.Unlock()
}

func (t *Transport) setMaxTokens(v string) {
	t.cfgMu.Lock()
	t.defaultMaxTokens = v
	t.cfgMu.Unlock()
}

func (t *Transport) thinkDefault() string {
	t.cfgMu.Lock()
	defer t.cfgMu.Unlock()
	if t.defaultThink == "" {
		return "high"
	}
	return t.defaultThink
}

func (s *sessionEntry) think() string {
	if s == nil {
		return "high"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Think == "" {
		return "high"
	}
	return s.Think
}

func (s *sessionEntry) setThink(v string) {
	s.mu.Lock()
	s.Think = v
	s.mu.Unlock()
}

// hitlPolicy returns the session's HITL policy selection, defaulting to the
// "use configured default" sentinel when unset (nil-safe, mirroring think()).
func (s *sessionEntry) hitlPolicy() string {
	if s == nil {
		return hitlPolicyDefaultValue
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.HITLPolicy == "" {
		return hitlPolicyDefaultValue
	}
	return s.HITLPolicy
}

func (s *sessionEntry) setHITLPolicy(v string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.HITLPolicy = v
	s.mu.Unlock()
}

func (s *sessionEntry) providerOrDefault(defaultProvider string) string {
	if s == nil {
		return defaultProvider
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Provider == "" {
		return defaultProvider
	}
	return s.Provider
}

func (s *sessionEntry) modelOrDefault(defaultModel string) string {
	if s == nil {
		return defaultModel
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Model == "" {
		return defaultModel
	}
	return s.Model
}

// resolutionBounds is this session's envelope allowlist as
// llmrepo.ResolutionBounds. Both turn paths must bind through this one
// accessor, or a unit could escape its compute envelope depending on which
// path ran it. Zero (binds nothing) for chat or an unbounded mission.
func (s *sessionEntry) resolutionBounds() llmrepo.ResolutionBounds {
	if s == nil {
		return llmrepo.ResolutionBounds{}
	}
	return llmrepo.ResolutionBounds{
		Models:   s.ModelAllowlist,
		Backends: s.BackendAllowlist,
	}
}

func (s *sessionEntry) effectiveTokenLimit() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.EffectiveTokenLimit
}

func (s *sessionEntry) setEffectiveTokenLimit(v int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.EffectiveTokenLimit = v
}

func (s *sessionEntry) setModelSelection(provider, model string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.Provider = provider
	s.Model = model
	s.mu.Unlock()
}

func (t *Transport) acpSessionForContenoxID(contenoxSessionID string) (libacp.SessionID, bool) {
	t.sessionMu.Lock()
	defer t.sessionMu.Unlock()
	sid, ok := t.contenoxToACPID[contenoxSessionID]
	return sid, ok
}

// bindContenoxSession records the contenox<->ACP mapping and registers this
// transport with the shared router for HITL routing. Callers hold sessionMu;
// the router takes its own lock and never calls back under it, so there is
// no lock-ordering hazard.
func (t *Transport) bindContenoxSession(contenoxSessionID string, sid libacp.SessionID) {
	t.contenoxToACPID[contenoxSessionID] = sid
	t.deps.SessionRouter.bind(contenoxSessionID, t)
}

// unbindContenoxSession is the inverse: it drops the mapping and deregisters
// this transport from the router (only if it is still the registered owner).
// Callers hold sessionMu.
func (t *Transport) unbindContenoxSession(contenoxSessionID string) {
	delete(t.contenoxToACPID, contenoxSessionID)
	t.deps.SessionRouter.unbind(contenoxSessionID, t)
}

// registerPromptCancel records cancel as sid's in-flight turn canceller,
// superseding and cancelling any prior registration (one turn per session).
// Returns the token for a symmetric unregisterPromptCancel.
func (t *Transport) registerPromptCancel(sid libacp.SessionID, cancel context.CancelFunc) *inflightPrompt {
	reg := &inflightPrompt{cancel: cancel}
	t.promptCancelMu.Lock()
	if t.promptCancels == nil {
		t.promptCancels = make(map[libacp.SessionID]*inflightPrompt)
	}
	prev := t.promptCancels[sid]
	t.promptCancels[sid] = reg
	t.promptCancelMu.Unlock()
	if prev != nil {
		prev.cancel()
	}
	return reg
}

// unregisterPromptCancel drops reg's registration if — and only if — it is
// still the current one for sid (pointer identity), so a turn that already
// ended never clears a newer turn's registration.
func (t *Transport) unregisterPromptCancel(sid libacp.SessionID, reg *inflightPrompt) {
	t.promptCancelMu.Lock()
	if cur, ok := t.promptCancels[sid]; ok && cur == reg {
		delete(t.promptCancels, sid)
	}
	t.promptCancelMu.Unlock()
}

// cancelInflightPrompt cancels sid's in-flight turn, reporting whether it
// did; a no-op without one (the spec allows session/cancel at any time). The
// registration stays; the turn's deferred unregisterPromptCancel removes it.
func (t *Transport) cancelInflightPrompt(sid libacp.SessionID) bool {
	t.promptCancelMu.Lock()
	reg, ok := t.promptCancels[sid]
	t.promptCancelMu.Unlock()
	if !ok {
		return false
	}
	reg.cancel()
	return true
}

// Cancel handles session/cancel, aborting the in-flight turn with
// context.Canceled semantics: Prompt resolves it with stopReason "cancelled",
// never a JSON-RPC error, per the ACP contract. A no-op with no running turn.
func (t *Transport) Cancel(ctx context.Context, req libacp.CancelNotification) error {
	_, reportChange, end := t.tracker().Start(ctx, "cancel", "acp_session", "session_id", string(req.SessionID))
	defer end()
	cancelled := t.cancelInflightPrompt(req.SessionID)
	// A survival turn's canceller is not in promptCancels; reach it through
	// the Registry. A connection drop deliberately does not cancel it.
	if t.deps.NativeTurns != nil && t.deps.NativeTurns.Cancel(req.SessionID) {
		cancelled = true
	}
	reportChange(string(req.SessionID), map[string]any{"cancelled_inflight": cancelled})
	return nil
}

func (t *Transport) clientIdentity() *libacp.Implementation {
	t.initMu.Lock()
	defer t.initMu.Unlock()
	return t.clientInfo
}

func (t *Transport) getClientCaps() libacp.ClientCapabilities {
	t.initMu.Lock()
	defer t.initMu.Unlock()
	return t.clientCaps
}

func (t *Transport) workspaceID() string {
	return t.deps.WorkspaceID
}

func (t *Transport) sendUpdate(ctx context.Context, notif libacp.SessionNotification) {
	if t.conn == nil {
		return
	}
	notif = t.normalizeToolCallNotification(notif)
	kind := string(notif.Update.SessionUpdate)
	kv := []any{"kind", kind, "session_id", string(notif.SessionID)}
	if notif.Update.ToolCallID != "" {
		kv = append(kv, "tool_call_id", notif.Update.ToolCallID)
	}
	if notif.Update.Status != "" {
		kv = append(kv, "status", string(notif.Update.Status))
	}
	reportErr, _, end := t.tracker().Start(ctx, "send", "acp_session_update", kv...)
	defer end()
	if err := t.conn.SessionUpdate(notif); err != nil {
		reportErr(err)
	}
}

func (t *Transport) normalizeToolCallNotification(notif libacp.SessionNotification) libacp.SessionNotification {
	upd := &notif.Update
	if upd.ToolCallID == "" {
		return notif
	}
	if upd.SessionUpdate != libacp.SessionUpdateToolCall && upd.SessionUpdate != libacp.SessionUpdateToolCallUpdate {
		return notif
	}

	t.toolCallMu.Lock()
	defer t.toolCallMu.Unlock()
	if t.toolCallStatus == nil {
		t.toolCallStatus = make(map[string]libacp.ToolCallStatus)
	}

	key := permKey(notif.SessionID, upd.ToolCallID)
	previousStatus, seen := t.toolCallStatus[key]
	if upd.SessionUpdate == libacp.SessionUpdateToolCallUpdate && !seen {
		upd.SessionUpdate = libacp.SessionUpdateToolCall
		if upd.Title == "" {
			upd.Title = upd.ToolCallID
		}
		if upd.Kind == "" {
			upd.Kind = libacp.ToolKindOther
		}
	}

	if seen && toolCallStatusRank(upd.Status) < toolCallStatusRank(previousStatus) {
		upd.Status = previousStatus
	}
	t.toolCallStatus[key] = upd.Status
	return notif
}

func (t *Transport) clearToolCallState(sid libacp.SessionID) {
	t.toolCallMu.Lock()
	defer t.toolCallMu.Unlock()
	prefix := string(sid) + "\x00"
	for key := range t.toolCallStatus {
		if strings.HasPrefix(key, prefix) {
			delete(t.toolCallStatus, key)
		}
	}
	for key := range t.toolCallSeq {
		if strings.HasPrefix(key, prefix) {
			delete(t.toolCallSeq, key)
		}
	}
	for key := range t.toolCallOpen {
		if strings.HasPrefix(key, prefix) {
			delete(t.toolCallOpen, key)
		}
	}
}

// toolCallWireID resolves the ACP tool-call id for an event using this
// connection's seq/open maps. See resolveToolCallWireID for the algorithm.
func (t *Transport) toolCallWireID(sid libacp.SessionID, ev taskengine.TaskEvent, closes bool) string {
	t.toolCallMu.Lock()
	defer t.toolCallMu.Unlock()
	if t.toolCallSeq == nil {
		t.toolCallSeq = make(map[string]int)
	}
	if t.toolCallOpen == nil {
		t.toolCallOpen = make(map[string]int)
	}
	return resolveToolCallWireID(t.toolCallSeq, t.toolCallOpen, sid, ev, closes)
}

// resolveToolCallWireID is the invocation-counter logic shared by Transport
// and the native-turn translator over their own seq/open maps (caller-locked,
// non-nil). An ApprovalID is used verbatim; otherwise the name-derived base
// gets a counter so repeated runs of one tool stay distinct cards.
func resolveToolCallWireID(seq, open map[string]int, sid libacp.SessionID, ev taskengine.TaskEvent, closes bool) string {
	if ev.ApprovalID != "" {
		return ev.ApprovalID
	}
	base := fallbackToolCallID(ev)
	if base == "" {
		return ""
	}
	key := permKey(sid, ev.TaskID+"\x1f"+base)
	if closes {
		if n, ok := open[key]; ok {
			delete(open, key)
			return invocationToolCallID(base, n)
		}
		seq[key]++
		return invocationToolCallID(base, seq[key])
	}
	seq[key]++
	open[key] = seq[key]
	return invocationToolCallID(base, seq[key])
}

func invocationToolCallID(base string, n int) string {
	if n <= 1 {
		return base
	}
	return base + "#" + strconv.Itoa(n)
}

func toolCallStatusRank(status libacp.ToolCallStatus) int {
	switch status {
	case libacp.ToolCallStatusPending:
		return 1
	case libacp.ToolCallStatusInProgress:
		return 2
	case libacp.ToolCallStatusCompleted, libacp.ToolCallStatusFailed:
		return 3
	default:
		return 0
	}
}

func (t *Transport) tracker() libtracker.ActivityTracker {
	if t.deps.Engine != nil && t.deps.Engine.Tracker != nil {
		return t.deps.Engine.Tracker
	}
	return libtracker.NoopTracker{}
}

func ReadConfigValue(ctx context.Context, db libdb.DBManager, key string) string {
	store := runtimetypes.New(db.WithoutTransaction())
	return strings.TrimSpace(clikv.Read(ctx, store, key))
}

func resolveACPSessionID(ctx context.Context, t *Transport) libacp.SessionID {
	contenoxSessionID := sessionIDFromCtx(ctx)
	if contenoxSessionID == "" {
		return ""
	}
	acpSID, _ := t.acpSessionForContenoxID(contenoxSessionID)
	return acpSID
}

func sessionIDFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(runtimetypes.SessionIDContextKey).(string)
	return v
}

// sendInitialUsageUpdate sends a brand-new session's usage_update: size is
// the effective token budget, used is 0. Sessions with history use
// sendUsageUpdate instead.
func (t *Transport) sendInitialUsageUpdate(ctx context.Context, sid libacp.SessionID) {
	if size := t.sessionTokenSize(ctx, sid); size > 0 {
		t.sendUpdate(ctx, libacp.SessionNotification{
			SessionID: sid,
			Update: libacp.SessionUpdate{
				SessionUpdate: libacp.SessionUpdateUsageUpdate,
				Size:          size,
			},
		})
	}
}

// sendUsageUpdate emits the gauge for a session with history, used half
// filled in. Emitted whenever either half is known, mirroring the live
// token_usage translation in events.go.
func (t *Transport) sendUsageUpdate(ctx context.Context, sid libacp.SessionID, used int) {
	size := t.sessionTokenSize(ctx, sid)
	if size <= 0 && used <= 0 {
		return
	}
	t.sendUpdate(ctx, libacp.SessionNotification{
		SessionID: sid,
		Update: libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateUsageUpdate,
			Used:          used,
			Size:          size,
		},
	})
}

// sendResumedUsageUpdate is sendUsageUpdate for session/resume, which unlike
// session/load never reads the transcript, so history is fetched here. A read
// failure degrades to a size-only update rather than failing the resume.
func (t *Transport) sendResumedUsageUpdate(ctx context.Context, sid libacp.SessionID, entry *sessionEntry) {
	used := 0
	if t.deps.DB != nil && entry != nil && entry.InternalSessionID != "" {
		mgr := chatservice.NewManager(entry.WorkspaceID)
		if msgs, err := mgr.ListMessages(ctx, t.deps.DB.WithoutTransaction(), entry.InternalSessionID); err == nil {
			used = estimateHistoryTokens(msgs)
		}
	}
	t.sendUsageUpdate(ctx, sid, used)
}

// sessionTokenSize resolves the "size" half of a usage_update. It mirrors
// the arithmetic taskengine uses to pick a turn's ctxLength (chain
// token_limit as base, session override winning only when smaller) so the
// gauge's denominator matches the next turn's first token_usage event. The
// model-context-length scan is the last resort; 0 means no budget resolved.
func (t *Transport) sessionTokenSize(ctx context.Context, sid libacp.SessionID) int {
	t.sessionMu.Lock()
	sess, hasSess := t.sessions[sid]
	t.sessionMu.Unlock()

	limit := 0
	if t.deps.ChainRegistry != nil {
		if chain := t.deps.ChainRegistry.Default(); chain != nil {
			limit = int(chain.TokenLimit)
		}
	}
	if hasSess && sess != nil {
		if eff := sess.effectiveTokenLimit(); eff > 0 && (limit <= 0 || eff < limit) {
			limit = eff
		}
	}
	if limit > 0 {
		return limit
	}

	// Fallback to model cap (for cases where no explicit budget set yet)
	preferredModel := t.model()
	t.sessionMu.Lock()
	if entry, ok := t.sessions[sid]; ok && entry != nil {
		preferredModel = entry.modelOrDefault(t.model())
	}
	t.sessionMu.Unlock()

	for _, state := range t.runtimeStates(ctx) {
		for _, pulled := range state.PulledModels {
			if preferredModel != "" && pulled.Model == preferredModel && pulled.ContextLength > 0 {
				return pulled.ContextLength
			}
		}
	}
	for _, state := range t.runtimeStates(ctx) {
		for _, pulled := range state.PulledModels {
			if pulled.ContextLength > 0 && (pulled.CanChat || pulled.CanPrompt) {
				return pulled.ContextLength
			}
		}
	}
	return 0
}
