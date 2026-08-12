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
	"github.com/contenox/contenox/internal/models/llmrepo"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/chatservice"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/shellsession"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
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
	// (e.g. chain-compact-default.json for /compact).
	ContenoxDir string

	// WorkspaceRoots is the machine's allowlist of directories a client may
	// root a session in. The launch directory is always its default root;
	// configured roots extend it. The sentinel "/" and "" resolve to that
	// default, so a client that proposes nothing lands in the launch directory
	// rather than at the filesystem root.
	//
	// Nil and empty are different states and must stay so: nil means no
	// allowlist is configured, and the workspace-root config option is then
	// absent from session/new and from the initialize `_meta` snapshot, so a
	// client hides its picker instead of erroring. That is the stdio path,
	// where the editor owns the filesystem and any absolute cwd is accepted.
	//
	// Set it on any surface reachable through the relay. A remote client holds
	// only a session cookie, so its cwd is untrusted input and the machine
	// stays authoritative: a cwd outside the allowlist is refused, not adopted.
	// See Transport.resolveWorkspaceCwd, the single enforcement point.
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
	// registry a shared engine routes HITL approvals through. Required
	// wherever one engine serves more than one connection — serve's
	// WebSockets, and the ACP profile once relay attachments can arrive —
	// because a shared engine has no other way to tell which client is
	// driving the session an approval was raised on.
	//
	// Nil is legal and means "one transport, bound directly": a caller that
	// serves exactly one connection may route through that connection and
	// never consult a registry. Every method here is nil-safe, so an unwired
	// router costs nothing rather than needing a guard at each call site.
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

	// MissionEnvelopes lists and resolves the HITL policy files /mission may
	// fire under (`--policy`), over the host's own policy search path. Nil
	// drops the listing and the pre-dispatch existence check; /mission still
	// fires under whatever name it is given.
	MissionEnvelopes MissionEnvelopeSource

	// Asks is the durable ask inbox `/answer` records an answer through: the
	// process's OWN hitlservice.Service, the instance the engine gates on and
	// the resume hook is registered against. Nil (with Supervision) drops
	// /answer from the advertised menu.
	Asks AskInbox

	// Supervision resolves which missions a session fired, the ownership check
	// /answer applies. Asks and Supervision together gate whether `/answer` is
	// advertised and usable.
	Supervision MissionSupervision

	// OptInBeta mirrors the CLI's opt-in-beta gate, so a beta lever exposed by
	// a slash command (today: /mission --oracle) is hidden exactly where its
	// CLI flag is.
	OptInBeta bool
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

	// nativeViewMu guards nativeViewing: sessions whose in-flight native
	// turn this connection watches via an attached viewer; the mirror skips
	// them (one delivery path per connection).
	nativeViewMu  sync.Mutex
	nativeViewing map[libacp.SessionID]int

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

	// mirrorOnce starts the mirror pump on first use and mirrorCh is its
	// bounded queue: updates produced by another connection on a session this
	// one also holds. Separate from the turn's own write path so a screen that
	// stopped reading cannot stall the turn feeding it. See mirror.go.
	mirrorOnce sync.Once
	mirrorCh   chan mirrorItem

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

// claimPermissionCard reserves this connection's permission-card slot for
// (sid, toolCallID), reporting false when one is already open here.
//
// It is markPermissionPending made conditional, and it is where the re-offer
// path's idempotency lives (see reoffer.go). The slot is per connection
// because that is what "a second card" means: a session is held by every
// attached connection and SessionRouter.AskApproval already asks all of them,
// so the same approval showing on a phone and a desk is the intended state,
// not a duplicate. What must never happen is one connection being asked twice
// about one approval — a live ask and a re-offer racing, or a client loading
// the same session twice — and both of those collide on this key.
func (t *Transport) claimPermissionCard(sid libacp.SessionID, toolCallID string) bool {
	t.permMu.Lock()
	defer t.permMu.Unlock()
	if t.permPending == nil {
		t.permPending = make(map[string]struct{})
	}
	key := permKey(sid, toolCallID)
	if _, open := t.permPending[key]; open {
		return false
	}
	t.permPending[key] = struct{}{}
	return true
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

func (t *Transport) markNativeViewing(sid libacp.SessionID) {
	t.nativeViewMu.Lock()
	if t.nativeViewing == nil {
		t.nativeViewing = make(map[libacp.SessionID]int)
	}
	t.nativeViewing[sid]++
	t.nativeViewMu.Unlock()
}

func (t *Transport) unmarkNativeViewing(sid libacp.SessionID) {
	t.nativeViewMu.Lock()
	if n := t.nativeViewing[sid]; n <= 1 {
		delete(t.nativeViewing, sid)
	} else {
		t.nativeViewing[sid] = n - 1
	}
	t.nativeViewMu.Unlock()
}

func (t *Transport) isNativeViewing(sid libacp.SessionID) bool {
	t.nativeViewMu.Lock()
	defer t.nativeViewMu.Unlock()
	return t.nativeViewing[sid] > 0
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
			t.releaseSessionRouting()
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

// contenoxSessionForACPID is the inverse, over this connection's own sessions:
// the mirror addresses holders by contenox session id, which is the identity
// the router keys on and the one thing two connections agree about.
func (t *Transport) contenoxSessionForACPID(sid libacp.SessionID) (string, bool) {
	t.sessionMu.Lock()
	defer t.sessionMu.Unlock()
	entry, ok := t.sessions[sid]
	if !ok || entry == nil || entry.InternalSessionID == "" {
		return "", false
	}
	return entry.InternalSessionID, true
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

// claimSessionRouting records this transport as one of sess's holders: it
// receives the session's updates and is asked its approvals, alongside every
// other connection holding the same session.
//
// It is called at the top of every turn, not only when a session is created or
// loaded. Joining is idempotent — [SessionRouter.bind] moves an existing holder
// to the front rather than adding it twice — so the per-turn call costs
// nothing and only refreshes the recency order [SessionRouter.transportFor]
// reads.
//
// It does not evict anyone. Two clients on one session is the ordinary case,
// not a conflict to resolve: a phone and a desk attached to one runtime are one
// person looking at one session from two places, and both are addressed.
//
// A nil router, an unwired session and a session with no contenox id are all
// no-ops, so the single-connection path pays nothing for this.
func (t *Transport) claimSessionRouting(sess *sessionEntry) {
	if t.deps.SessionRouter == nil || sess == nil {
		return
	}
	t.deps.SessionRouter.bind(sess.InternalSessionID, t)
}

// releaseSessionRouting deregisters every session this transport holds from the
// shared router. It rides the connection's Closed signal, which is the only
// teardown hook every caller reaches — serve and the relay tunnel never call
// [Transport.Close] — so a client that detached, whether a phone that changed
// network, a relay attachment torn down or a WebSocket dropped, stops being
// asked and stops being mirrored to.
//
// Without it the router keeps naming a dead connection: every approval raised
// on one of its sessions waits on a screen that is gone, and every update is
// queued for a socket that will never drain. Deregistered, the remaining
// holders carry on, and ErrNoBoundSession is reported only once the last one
// leaves.
//
// It deliberately does NOT touch the connection-local session state, terminals
// or downstream agents: a bare connection drop is not a session close (see
// [Transport.Close]), and it is the router entry alone that must not outlive
// the connection. The router's unbind is identity-guarded, so releasing a
// session another connection has since claimed leaves that live claim standing.
func (t *Transport) releaseSessionRouting() {
	if t.deps.SessionRouter == nil {
		return
	}
	t.sessionMu.Lock()
	defer t.sessionMu.Unlock()
	for _, e := range t.sessions {
		t.deps.SessionRouter.unbind(e.InternalSessionID, t)
	}
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

// sendUpdate writes notif to this connection and mirrors it to every other
// connection holding the same session.
//
// The mirror is what makes a session one thing several surfaces watch rather
// than a private stream per connection: session/load already replays the
// transcript to a late attacher, and this extends the same view to the live
// tail, so a turn driven from a phone renders at the desk as it happens.
//
// Normalization runs once, here, and the mirror carries the result verbatim —
// see [Transport.mirrorUpdate] for why re-normalizing per connection would
// renumber tool cards.
func (t *Transport) sendUpdate(ctx context.Context, notif libacp.SessionNotification) {
	if t.conn == nil {
		return
	}
	notif = t.normalizeToolCallNotification(notif)
	t.writeUpdate(ctx, notif)
	if t.deps.SessionRouter != nil {
		if contenoxSessionID, ok := t.contenoxSessionForACPID(notif.SessionID); ok {
			t.deps.SessionRouter.mirror(t, contenoxSessionID, notif)
		}
	}
}

// sendUpdateLocal writes to this connection only, never the mirror: re-sync
// traffic (replay, usage gauges) is addressed to one connection.
func (t *Transport) sendUpdateLocal(ctx context.Context, notif libacp.SessionNotification) {
	if t.conn == nil {
		return
	}
	t.writeUpdate(ctx, t.normalizeToolCallNotification(notif))
}

// writeUpdate writes one already-normalized notification to this connection.
// It is the only place a session update reaches a socket, reached both by the
// turn that produced it and by the mirror pump carrying another connection's.
func (t *Transport) writeUpdate(ctx context.Context, notif libacp.SessionNotification) {
	if t.conn == nil {
		return
	}
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
		t.sendUpdateLocal(ctx, libacp.SessionNotification{
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
	t.sendUpdateLocal(ctx, libacp.SessionNotification{
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
