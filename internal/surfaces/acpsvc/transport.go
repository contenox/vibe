package acpsvc

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	"github.com/contenox/beam/internal/kernel/enginesvc"
	"github.com/contenox/beam/internal/kernel/nativeturn"
	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/chatservice"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/services/shellsession"
	"github.com/contenox/beam/internal/services/vfs"
	"github.com/contenox/beam/internal/store/runtimetypes"
	libacp "github.com/contenox/beam/libacp"
)

type Deps struct {
	Engine             *enginesvc.Engine
	DB                 libdb.DBManager
	ChainRegistry      *ChainRegistry
	DefaultModel       string
	DefaultProvider    string
	DefaultAltModel    string
	DefaultAltProvider string
	DefaultMaxTokens   string
	DefaultThink       string
	WorkspaceID        string
	// ContenoxDir is the active .contenox directory, used to locate auxiliary
	// chains (e.g. chain-compact.json for the /compact command).
	ContenoxDir string

	// WorkspaceRoots is the allowlist of directories a client may choose as a
	// session's workspace (its cwd). When nil, no allowlist is enforced and any
	// absolute cwd is accepted — the historical behavior for the stdio ACP path,
	// where the editor owns the filesystem. serve sets it so a browser client
	// can only root a session inside an operator-approved directory. The sentinel
	// cwd "/" (what beam sends today) and an empty cwd both resolve to the
	// default root, so existing clients keep working.
	WorkspaceRoots *vfs.Factory

	// ShellSessions manages the per-chat-session persistent PTY shells behind the
	// shell-session surface (the terminal panel + shell_session_run/read tools).
	// Nil when shell tooling is disabled: the terminal extension methods report
	// method-not-found and no live output is streamed — the feature is absent,
	// not broken.
	ShellSessions shellsession.Manager

	// KnownPolicies are the HITL policy preset names shown by /policy when
	// listing. Display only — empty just omits the list.
	KnownPolicies []string
	// HITLDefaultPolicyName is the policy the engine falls back to when no
	// override is set, shown by /policy so the status is accurate. Display only.
	HITLDefaultPolicyName string

	// UpdateBanner is an optional one-shot message sent to the client as an
	// agent_message_chunk on the first session created or loaded. Empty = no banner.
	UpdateBanner string

	// EnvSetup enables the env_var auth method: in setup-only mode initialize
	// advertises Vars as the environment the client should collect/set, and
	// authenticate with the env method calls Complete to finish setup
	// non-interactively from the current environment. Nil disables the method.
	EnvSetup *EnvSetupSpec

	// SessionRouter, when set, is a process-shared registry each transport
	// records its live (contenox session -> this transport) bindings into, so a
	// single shared engine can route a HITL approval back to the WS connection
	// whose client raised it. serve sets it (it hosts many ACP WS connections
	// behind one engine); the stdio ACP path leaves it nil — it has one
	// transport, late-bound directly into the engine's AskApproval closure.
	SessionRouter *SessionRouter

	// Instances, when set, owns the lifecycle of external-agent instances OFF any
	// single connection: an external session obtains its downstream agent from this
	// Manager (StartExternal) instead of spawning a subprocess bound to its own
	// connCtx, so the agent's process — and thus its context — SURVIVES a client
	// disconnect/reload, and a reloaded session re-attaches (Attach) to the same
	// still-running instance. serve sets it (a process-owned Manager, Closed at
	// shutdown). When nil (the stdio `contenox acp` path), the external path falls
	// back to today's connCtx-bound spawn — byte-for-byte the historical behavior —
	// so nothing regresses where the Manager is not wired.
	Instances agentinstance.Manager

	// NativeTurns, when set, is the survival layer for NATIVE (task-chain) turns —
	// the native counterpart of Instances. A native session/prompt runs its turn on
	// this serve-rooted Registry instead of on the connection's context, so a client
	// drop no longer cancels the running chain; the Transport attaches as a thin
	// VIEWER that replays the turn journal on (re)connect and joins the live
	// fan-out, and only session/cancel (or delete) actually cancels the turn. serve
	// sets it (a process-owned Registry, Closed at shutdown). When nil (the stdio
	// `contenox acp` path), the native path falls back to today's connection-bound
	// turn — byte-for-byte the historical behavior — so nothing regresses where the
	// Registry is not wired. See runtime/nativeturn and native_turn.go.
	NativeTurns *nativeturn.Registry

	// Fleet, when set, is what the `/mission` slash command fires through —
	// fleetservice.Dispatch, so firing a mission from a chat reimplements
	// nothing. It is the narrow MissionDispatcher slice (Dispatch only), not the
	// whole Service — /mission needs no more. A `contenox acp` editor embeds the
	// fleet IN-PROCESS (the mission is a subagent of THIS process, reporting
	// back into this session; see runtime/contenoxcli/acp_cmd.go). A process
	// that is ITSELF a dispatched unit, or a setup-only editor with no model,
	// leaves both nil.
	Fleet MissionDispatcher

	// Agents, when set, resolves a declared agent by name so /mission can tell its
	// two shape-identical grammar forms apart (`/mission <intent>` vs `/mission
	// <agent-name> <intent>`).
	//
	// Fleet and Agents together are this transport's mission capability
	// (hasMissionCapability, in mission.go): BOTH non-nil is what gates whether
	// `/mission` is advertised to the client at all (acpCommands, in commands.go)
	// and whether handleMission treats an invocation as possible. ACP is
	// advertise-what-works — a process with no dispatcher wired (a dispatched unit,
	// or a setup-only editor) never lists `/mission`. A client that sends it anyway
	// (stale menu state, a remembered command) gets handleMission's teaching error.
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
	// EffectiveTokenLimit is the user-chosen (or chain default) context budget for this session.
	// It is clamped at set time (and on use) to the model's ContextLength when the model reports >0.
	// 0 means "use chain default / unlimited". This is the value shown in usage indicators as "size".
	EffectiveTokenLimit int
	// HITLPolicy is the per-session HITL approval policy selection, session-scoped
	// exactly like Model/Think/EffectiveTokenLimit. It stores either a concrete
	// policy name or the "use configured default" sentinel (hitlPolicyDefaultValue);
	// empty is treated as the sentinel. Changing it must NEVER touch the global
	// cli.hitl-policy-name KV — that key is the single-session CLI/stdio fallback.
	// A concrete selection is injected into the prompt context (see prompt.go) so a
	// shared engine's one hitlservice gates each session's turn under its own policy.
	HITLPolicy string

	// MissionID is set when this session was constructed for a dispatched unit on a
	// mission — parsed from the session/new `_meta` (missionservice.MissionMetaKey)
	// the dispatcher forwards. It is what binds this session's mission tools to
	// exactly one mission: prompt.go injects it into the turn context
	// (missiontools.WithMissionID) so mission_report/mission_ask_attention report
	// against THIS mission and no other. Empty for an ordinary chat-mode session,
	// whose mission tools resolve to nothing (the envelope enforced at construction).
	MissionID string

	// FiredMissions marks the OTHER side of the relationship: this session has
	// dispatched missions of its own, which is what unlocks the supervisor tools
	// (mission_list / mission_answer) for its turns. Set when `/mission` succeeds
	// here and restored from the durable marker on load, so a reloaded session can
	// still see and answer its units. An ordinary chat leaves it false and is
	// offered no mission tools at all.
	FiredMissions bool

	// driver is this session's execution backend: a nativeDriver for the
	// contenox task-chain engine, or an externalDriver for a REGISTERED downstream
	// ACP agent. It is chosen once at construction — NewSession/LoadSession/
	// ResumeSession decide which from the session/new `_meta` or the persisted
	// agent KV — and every prompt/config/menu/teardown path dispatches through it
	// polymorphically, so there is no native-vs-external flag branch anywhere else.
	driver sessionDriver
}

// sessionDriver is the per-session execution backend a sessionEntry delegates
// to. The native and external implementations share the session's data
// (workspace, cwd, ids, model/think, HITL, mcp names) on the sessionEntry passed
// to each call; only the execution mechanism differs.
type sessionDriver interface {
	// Prompt runs one full turn for sess. The implementation owns everything the
	// turn needs: update relay / event translation, cancellation registration,
	// and history persistence all happen inside it.
	Prompt(ctx context.Context, req libacp.PromptRequest, sess *sessionEntry) (libacp.PromptResponse, error)
	// ConfigOptions returns the session config options advertised for sess. The
	// native driver returns the model/think/policy/token selects; the external
	// driver returns the DOWNSTREAM agent's own advertised options (captured from
	// its session/new response and kept current by config_option_update relays) —
	// contenox's native chain selects stay suppressed for an external session.
	ConfigOptions(ctx context.Context, sess *sessionEntry) []libacp.SessionConfigOption
	// SetConfigOption applies a config-option change for sess. The native driver
	// mutates the session's own model/think/policy/token selection; the external
	// driver forwards it to the downstream agent (session/set_config_option) and
	// adopts the option set the agent confirms. value carries the wire union
	// (string or boolean) so a boolean-typed downstream option round-trips intact.
	SetConfigOption(ctx context.Context, sess *sessionEntry, configID string, value libacp.SessionConfigOptionValue) error
	// AvailableCommands returns the slash-command menu emitted for the session, or
	// nil when the session has none — the external driver returns nil because the
	// downstream agent's own menu is relayed live via available_commands_update.
	AvailableCommands() []libacp.AvailableCommand
	// AgentName is the registered external agent name (feeds the session/new
	// `_meta` echo and session/list attribution), or "" for a native session.
	AgentName() string
	// Close releases the driver's connection-local resources. Idempotent: the
	// native driver is a no-op, the external driver closes the downstream Handle.
	Close() error
}

type Transport struct {
	deps Deps
	conn *libacp.AgentSideConnection
	// connectionID scopes client-supplied MCP servers to this ACP connection so
	// two clients loading the same session cannot overwrite each other's tools.
	connectionID string

	// connCtx is a connection-scoped context spawned external agents are bound to
	// (their subprocess dies when it is cancelled). connCancel is fired when the
	// upstream connection ends — the serve WebSocket path never calls
	// Transport.Close, so the connection's own Closed signal (both stdio and WS)
	// is the reliable teardown hook that guarantees no downstream process leaks.
	connCtx    context.Context
	connCancel context.CancelFunc

	initMu     sync.Mutex
	clientInfo *libacp.Implementation
	clientCaps libacp.ClientCapabilities

	sessionMu       sync.Mutex
	sessions        map[libacp.SessionID]*sessionEntry
	contenoxToACPID map[string]libacp.SessionID

	// cfgMu guards the live model/provider, which the /model and /provider
	// commands mutate while concurrent prompts read them. The values seed from
	// Deps at construction; Deps.DefaultModel/DefaultProvider are not read again.
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
	// that has no engine-minted ApprovalID (declarative `tools` tasks): the
	// name alone would reuse one wire id for every run, merging their cards and
	// pinning the status at the never-downgrade rank of the first completion.
	toolCallSeq  map[string]int
	toolCallOpen map[string]int

	bannerMu      sync.Mutex
	pendingBanner string

	// termSubMu guards termSubs, the per-open-session cancel funcs for live
	// terminal-output subscriptions to the shell manager. One subscription per
	// ACP session id; re-subscribing (on reload) cancels the prior one.
	termSubMu sync.Mutex
	termSubs  map[libacp.SessionID]func()

	// promptCancelMu guards promptCancels, the per-session canceller for the
	// in-flight prompt turn. Prompt registers its turn context's cancel here so
	// session/cancel (Cancel), an explicit session Close/Delete, or a connection
	// drop can abort the running chain — the server owning cancellation instead
	// of relying solely on libacp's connection-level promptCtx substitution. One
	// turn per session is the invariant; a superseding registration cancels the
	// stale one so nothing outlives its turn.
	promptCancelMu sync.Mutex
	promptCancels  map[libacp.SessionID]*inflightPrompt
}

// inflightPrompt is a running turn's cancellation registration. The pointer is
// the identity used for symmetric unregister so a turn that already ended never
// removes a newer turn's registration.
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

// isPermissionPending reports whether a permission dialog is currently open on THIS
// connection for (sid, toolCallID). It is the viewer-side half of the guard
// sendToolCallUpdateGuarded applies inline on the connCtx path: on the survival
// path the turn's event translation runs OFF the connection (in the nativeturn
// Registry), so the per-connection suppression moves to the point of delivery — the
// native-turn viewer consults this before writing a tool-call card, so the
// permission request the client is answering is not shadowed by a duplicate card.
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
		// The `!` shell passthrough and any future contenox-namespaced client
		// requests arrive as ACP extension methods (see terminal.go). A conformant
		// foreign client never calls them; unknown extension methods still answer
		// MethodNotFound because this handler only claims the contenox namespace.
		conn.SetExtRequestHandler(t.handleExtRequest)
		// Tear down any external-agent subprocesses when this connection ends.
		// Cancelling connCtx closes every downstream agent spawned on it (Connect
		// binds the subprocess to it); this fires for both the stdio path and the
		// serve WebSocket path, the latter never calling Transport.Close.
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

// chainTemplateVars seeds the template vars every chain execution needs.
// The seeded chains reference {{var:alt_model|var:default_model}} (and the
// provider equivalent), so default_model/default_provider must always be
// set when a model is known — configured default first, falling back to the
// session's effective selection, matching the CLI chat path.
func (t *Transport) chainTemplateVars(sess *sessionEntry) map[string]string {
	vars := map[string]string{
		"model":    sess.modelOrDefault(t.model()),
		"provider": sess.providerOrDefault(t.provider()),
	}
	// default_model/default_provider are the recovery fallback for the seeded
	// chains' {{var:alt_model|var:default_model}}. They must be the
	// session-effective selection (vars["model"]/vars["provider"]), not the
	// transport-configured default: Zed's model dropdown sets a session-only
	// selection that never touches t.model(), so seeding from t.model() here
	// makes recovery/summarise_failure resolve a stale provider that may have no
	// models in runtime state while the main tasks use the working selection.
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

// bindContenoxSession records the contenox<->ACP session mapping on this
// connection and, when serve supplied a shared SessionRouter, registers this
// transport as the owner of the contenox session so the shared engine can route
// HITL approvals here. Callers hold sessionMu; the router takes its own lock and
// never calls back under it, so no lock-ordering hazard exists.
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

// registerPromptCancel records cancel as the in-flight turn's canceller for
// sid, superseding (and cancelling) any prior registration. One turn per
// session is the invariant, but a stale registration must never outlive its
// turn, so a second turn cancels the first. Returns the registration token for
// a symmetric unregisterPromptCancel.
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

// cancelInflightPrompt cancels the in-flight turn for sid, if one is running,
// and reports whether it did. A cancel with no in-flight turn is a clean no-op
// (returns false) — the spec allows session/cancel at any time. The
// registration is left in place; the turn's deferred unregisterPromptCancel
// removes it, and cancelling an already-cancelled context is harmless.
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

// Cancel handles session/cancel: it aborts the session's in-flight prompt turn
// with context.Canceled semantics. Prompt's error path keys the silent-cancel
// on errors.Is(err, context.Canceled) and resolves the prompt with stopReason
// "cancelled" (never a JSON-RPC error), per the ACP contract. A cancel for a
// session with no running turn is a clean no-op.
func (t *Transport) Cancel(ctx context.Context, req libacp.CancelNotification) error {
	_, reportChange, end := t.tracker().Start(ctx, "cancel", "acp_session", "session_id", string(req.SessionID))
	defer end()
	cancelled := t.cancelInflightPrompt(req.SessionID)
	// A native survival turn runs OFF this connection, so its canceller is not in
	// promptCancels — session/cancel must reach it through the Registry. This is the
	// ONLY real user cancel of a survival turn; a connection drop deliberately does
	// not. A no-op for external sessions and the stdio path (nil Registry / no turn).
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

// toolCallWireID resolves the ACP tool-call id for an event. The engine's
// ApprovalID is already per-invocation; the name-derived fallback gets an
// invocation counter so repeated runs of one tool stay distinct cards. A
// pending event opens an invocation, the result event closes it (matching by
// key when one is open, else it is a result without a pending — its own
// invocation). The first invocation keeps the bare name, so single-run flows
// are wire-identical to before.
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

// resolveToolCallWireID is the pure invocation-counter logic behind
// toolCallWireID, factored out so the connection-scoped Transport and the
// turn-scoped native-turn translator (native_turn.go) can share one
// implementation over their own seq/open maps. The caller owns any locking; the
// maps must be non-nil. An event carrying an engine-minted ApprovalID uses it
// verbatim (already per-invocation); otherwise the name-derived base gets an
// invocation counter so repeated runs of one tool stay distinct cards, opening on
// a pending event and closing on the matching result.
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

// sendInitialUsageUpdate sends a usage_update with the session's effective token budget as size
// (the chain token_limit or per-session override, clamped to model cap).
// Used is 0 — this is the BRAND-NEW session path, where zero is the truth.
// A session with a history uses sendUsageUpdate instead. This makes indicators based on the
// user-visible/controllable session budget, not raw model cap or default-max-tokens.
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

// sendUsageUpdate emits the gauge for a session that ALREADY has a history —
// the load/resume counterpart of sendInitialUsageUpdate, same size resolution
// with the used half filled in. It is emitted whenever either half is known, so
// a deployment that cannot resolve a context size still reports what the
// session has consumed (mirroring the live token_usage translation in
// events.go, which publishes on `size > 0 || used > 0` for the same reason).
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

// sendResumedUsageUpdate is sendUsageUpdate for session/resume, which — unlike
// session/load — never reads the transcript, so the history it needs to size
// the used half is fetched here. A read failure degrades to a size-only update
// rather than failing the resume: a gauge is not worth refusing a session over.
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

// sessionTokenSize resolves the "size" half of a usage_update: the context
// budget the gauge is drawn against, BEFORE any turn has reported one.
//
// It is deliberately the same arithmetic taskengine runs to pick a turn's
// ctxLength (see taskenv's tokenLimit resolution): the chain's token_limit is
// the base, and the session's own override wins only when it is SMALLER (or
// when the chain declares none). Deriving it any other way would put a
// different denominator under the gauge than the first token_usage event of the
// next turn, and the indicator would visibly jump for no reason the operator
// can see.
//
// The chain's token_limit was missing from this resolution entirely, which is
// why a reloaded session could produce a usage_update with no size at all
// wherever the backend reports no per-model context length — most hosted
// providers, including every vertex model. The model-context-length scan below
// stays as the last resort for chains that declare no budget.
//
// 0 means no budget could be resolved at all.
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
