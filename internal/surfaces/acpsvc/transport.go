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
	// FIMChainRegistry supplies the fill-in-the-middle chain `_contenox/autocomplete`
	// runs. Nil disables autocomplete.
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

	// WorkspaceRoots is the one workspace a host serves, fixed at launch; set
	// only by the host profile. Nil is the editor-driven shape: the client's
	// cwd on session/new is authoritative and only control-plane paths are
	// refused. Enforced in Transport.resolveWorkspaceCwd.
	WorkspaceRoots *vfs.Factory

	// KnownPolicies are the HITL policy preset names /policy lists. Display only.
	KnownPolicies []string
	// HITLDefaultPolicyName is the engine's fallback policy, shown by /policy.
	HITLDefaultPolicyName string

	// UpdateBanner is an optional one-shot agent_message_chunk sent on the
	// first session created or loaded. Empty = no banner.
	UpdateBanner string

	// EnvSetup enables the env_var auth method (Vars advertised at
	// initialize, Complete finishes setup non-interactively). Nil disables it.
	EnvSetup *EnvSetupSpec

	// SessionRouter is the process-shared (contenox session -> transport) registry
	// a shared engine routes HITL approvals through. Nil means one transport bound
	// directly; every router method is nil-safe.
	SessionRouter *SessionRouter

	// Instances owns external-agent instances off any single connection, so the
	// agent's process survives a client disconnect. Nil falls back to a
	// connCtx-bound spawn.
	Instances agentinstance.Manager

	// NativeTurns is the survival layer for native turns: prompts run on this
	// serve-rooted Registry, so a client drop no longer cancels the chain. Nil
	// falls back to a connection-bound turn.
	NativeTurns *nativeturn.Registry

	// Fleet, when set, is what `/mission` fires through. Nil for a dispatched unit
	// or a setup-only editor.
	Fleet MissionDispatcher

	// Agents resolves a declared agent by name for /mission's grammar. With Fleet
	// it gates whether `/mission` is advertised.
	Agents MissionAgentResolver

	// MissionEnvelopes lists and resolves the HITL policy files /mission may fire
	// under. Nil drops the listing and the pre-dispatch existence check.
	MissionEnvelopes MissionEnvelopeSource

	// Asks is the durable ask inbox /answer records through; it must be the
	// process's own hitlservice.Service.
	Asks AskInbox

	// Supervision resolves which missions a session fired. With Asks it gates
	// whether /answer is advertised.
	Supervision MissionSupervision

	// OptInBeta mirrors the CLI's opt-in-beta gate.
	OptInBeta bool
}

// EnvSetupSpec describes environment-variable-based setup.
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
	// EffectiveTokenLimit is the context budget; 0 means chain default.
	EffectiveTokenLimit int
	// HITLPolicy is the per-session HITL policy ("" = the default sentinel). Must
	// never touch the global cli.hitl-policy-name KV.
	HITLPolicy string

	// MissionID is the mission this session is a dispatched unit of; "" for chat.
	MissionID string

	// ModelAllowlist and BackendAllowlist are the mission envelope's compute bound.
	ModelAllowlist   []string
	BackendAllowlist []string

	FiredMissions bool

	driver sessionDriver
}

// sessionDriver is the per-session execution backend a sessionEntry delegates to:
// the native chain engine or an external downstream agent.
type sessionDriver interface {
	Prompt(ctx context.Context, req libacp.PromptRequest, sess *sessionEntry) (libacp.PromptResponse, error)
	ConfigOptions(ctx context.Context, sess *sessionEntry) []libacp.SessionConfigOption
	SetConfigOption(ctx context.Context, sess *sessionEntry, configID string, value libacp.SessionConfigOptionValue) error
	AvailableCommands() []libacp.AvailableCommand
	AgentName() string
	Close() error
}

type Transport struct {
	deps Deps
	conn *libacp.AgentSideConnection
	// connectionID scopes client-supplied MCP servers to this ACP connection.
	connectionID string

	// connCtx binds spawned external agents to this connection; connCancel fires on
	// the connection's Closed signal, since serve never calls Transport.Close.
	connCtx    context.Context
	connCancel context.CancelFunc

	initMu     sync.Mutex
	clientInfo *libacp.Implementation
	clientCaps libacp.ClientCapabilities

	sessionMu       sync.Mutex
	sessions        map[libacp.SessionID]*sessionEntry
	contenoxToACPID map[string]libacp.SessionID

	// cfgMu guards the live model/provider, mutated by /model and /provider while
	// concurrent prompts read them.
	cfgMu              sync.Mutex
	defaultModel       string
	defaultProvider    string
	defaultAltModel    string
	defaultAltProvider string
	defaultMaxTokens   string
	defaultThink       string

	permMu      sync.Mutex
	permPending map[string]struct{}

	// nativeViewMu guards nativeViewing: sessions whose in-flight native turn this
	// connection watches via an attached viewer; the mirror skips them.
	nativeViewMu  sync.Mutex
	nativeViewing map[libacp.SessionID]int

	toolCallMu     sync.Mutex
	toolCallStatus map[string]libacp.ToolCallStatus
	// toolCallSeq and toolCallOpen disambiguate repeated invocations of a tool with
	// no engine-minted ApprovalID, which would otherwise share one wire id.
	toolCallSeq  map[string]int
	toolCallOpen map[string]int

	bannerMu      sync.Mutex
	pendingBanner string

	// termSubMu guards termSubs, the per-session cancel funcs for live
	// terminal-output subscriptions.
	termSubMu sync.Mutex
	termSubs  map[libacp.SessionID]func()

	// promptCancelMu guards promptCancels, the per-session canceller for the
	// in-flight turn. One turn per session; a superseding registration cancels the
	// stale one.
	promptCancelMu sync.Mutex
	promptCancels  map[libacp.SessionID]*inflightPrompt

	// mirrorOnce starts the mirror pump on first use; mirrorCh is its bounded queue,
	// separate from the turn's write path so a stalled screen cannot stall the turn.
	mirrorOnce sync.Once
	mirrorCh   chan mirrorItem

	// acAgent is a test seam; nil in production.
	acAgent agentservice.Agent
}

// inflightPrompt is a running turn's cancellation registration. Pointer identity
// makes unregister symmetric.
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
// (sid, toolCallID), reporting false when one is already open here. The slot is
// per connection, so one connection is never asked twice about one approval while
// other holders are still asked once each.
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
// (sid, toolCallID).
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
		conn.SetExtRequestHandler(t.handleExtRequest)
		// Cancelling connCtx tears down every external-agent subprocess spawned on
		// it, including serve's WS, which never calls Transport.Close.
		go func() {
			<-conn.Closed()
			connCancel()
			t.releaseSessionRouting()
		}()
		return t
	}
}

// takeBanner atomically reads and clears the pending update banner.
func (t *Transport) takeBanner() string {
	t.bannerMu.Lock()
	defer t.bannerMu.Unlock()
	b := t.pendingBanner
	t.pendingBanner = ""
	return b
}

func (t *Transport) model() string {
	t.cfgMu.Lock()
	defer t.cfgMu.Unlock()
	return t.defaultModel
}

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

func (t *Transport) chainTemplateVars(sess *sessionEntry) map[string]string {
	vars := map[string]string{
		"model":    sess.modelOrDefault(t.model()),
		"provider": sess.providerOrDefault(t.provider()),
	}
	// default_model/default_provider must be the session-effective selection, or
	// recovery tasks resolve a stale provider.
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

// resolutionBounds is this session's envelope allowlist. Both turn paths must
// bind through this one accessor, or a unit could escape its compute envelope.
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
// transport with the shared router. Callers hold sessionMu.
func (t *Transport) bindContenoxSession(contenoxSessionID string, sid libacp.SessionID) {
	t.contenoxToACPID[contenoxSessionID] = sid
	t.deps.SessionRouter.bind(contenoxSessionID, t)
}

// unbindContenoxSession drops the mapping and deregisters this transport from the
// router if it is still the registered owner. Callers hold sessionMu.
func (t *Transport) unbindContenoxSession(contenoxSessionID string) {
	delete(t.contenoxToACPID, contenoxSessionID)
	t.deps.SessionRouter.unbind(contenoxSessionID, t)
}

// claimSessionRouting records this transport as one of sess's holders. Idempotent
// and called at the top of every turn; it evicts no one, since two clients on one
// session is the ordinary case.
func (t *Transport) claimSessionRouting(sess *sessionEntry) {
	if t.deps.SessionRouter == nil || sess == nil {
		return
	}
	t.deps.SessionRouter.bind(sess.InternalSessionID, t)
}

// releaseSessionRouting deregisters every session this transport holds from the
// shared router, riding the connection's Closed signal — the only teardown hook
// serve and the relay tunnel reach. It deliberately leaves connection-local
// session state, terminals and downstream agents alone: a bare connection drop is
// not a session close.
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
// superseding and cancelling any prior registration.
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

// unregisterPromptCancel drops reg's registration only if it is still the current
// one for sid, so an ended turn never clears a newer turn's registration.
func (t *Transport) unregisterPromptCancel(sid libacp.SessionID, reg *inflightPrompt) {
	t.promptCancelMu.Lock()
	if cur, ok := t.promptCancels[sid]; ok && cur == reg {
		delete(t.promptCancels, sid)
	}
	t.promptCancelMu.Unlock()
}

// cancelInflightPrompt cancels sid's in-flight turn, reporting whether it did.
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

// Cancel handles session/cancel, aborting the in-flight turn so Prompt resolves
// it with stopReason "cancelled" rather than a JSON-RPC error.
func (t *Transport) Cancel(ctx context.Context, req libacp.CancelNotification) error {
	_, reportChange, end := t.tracker().Start(ctx, "cancel", "acp_session", "session_id", string(req.SessionID))
	defer end()
	cancelled := t.cancelInflightPrompt(req.SessionID)
	// A survival turn's canceller is not in promptCancels; reach it through the
	// Registry. A connection drop deliberately does not cancel it.
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
// connection holding the same session. Normalization runs once, here, and the
// mirror carries the result verbatim.
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

// sendUpdateLocal writes to this connection only, never the mirror.
func (t *Transport) sendUpdateLocal(ctx context.Context, notif libacp.SessionNotification) {
	if t.conn == nil {
		return
	}
	t.writeUpdate(ctx, t.normalizeToolCallNotification(notif))
}

// writeUpdate writes one already-normalized notification to this connection. It is
// the only place a session update reaches a socket.
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
// connection's seq/open maps.
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

// resolveToolCallWireID is the invocation-counter logic shared by Transport and
// the native-turn translator over their own caller-locked seq/open maps.
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

// sendInitialUsageUpdate sends a brand-new session's usage_update; sessions with
// history use sendUsageUpdate instead.
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

// sendUsageUpdate emits the gauge for a session with history, whenever either
// half is known.
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

// sendResumedUsageUpdate is sendUsageUpdate for session/resume, which never reads
// the transcript, so history is fetched here.
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

// sessionTokenSize resolves the "size" half of a usage_update. It mirrors the
// arithmetic taskengine uses to pick a turn's ctxLength so the gauge's
// denominator matches the next turn's first token_usage event.
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
