package acpsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/agenthost"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/chatservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/version"
	libacp "github.com/contenox/contenox/libacp"
)

// AgentMetaKey is the session/new (and session/list) `_meta` key a client sets
// to bind a session to a registered external ACP agent instead of the native
// task-chain engine: `{"contenox.agent": "<name>"}`. Absent means the native
// chain path. Lives in the spec's reserved `_meta` namespace, so conformant
// clients that don't recognize it simply ignore it.
const AgentMetaKey = "contenox.agent"

// AgentModeConfigOptionID is the reserved SessionConfigOption id under which
// an external session surfaces the downstream agent's session Modes as one
// synthetic "select" option (label "Mode", each availableMode a value,
// currentValue = currentModeId) — ACP keeps modes and config options
// separate, and contenox has no first-class mode toggle. A set translates to
// session/set_mode; a current_mode_update relays back as a
// config_option_update on this id. Reserved namespace avoids id collisions.
const AgentModeConfigOptionID = "contenox.agent-mode"

// syntheticModeOption maps a downstream SessionModeState onto the synthetic
// "Mode" select (see AgentModeConfigOptionID). ok=false when there are no
// modes to surface (nil state or empty availableModes).
func syntheticModeOption(m *libacp.SessionModeState) (libacp.SessionConfigOption, bool) {
	if m == nil || len(m.AvailableModes) == 0 {
		return libacp.SessionConfigOption{}, false
	}
	values := make([]libacp.SessionConfigValue, 0, len(m.AvailableModes))
	for _, mode := range m.AvailableModes {
		values = append(values, libacp.SessionConfigValue{
			Value:       mode.ID,
			Name:        mode.Name,
			Description: mode.Description,
		})
	}
	return libacp.SessionConfigOption{
		ID:           AgentModeConfigOptionID,
		Name:         "Mode",
		Type:         libacp.SessionConfigOptionTypeSelect,
		CurrentValue: m.CurrentModeID,
		Options:      libacp.NewSessionConfigValues(values),
	}, true
}

// AgentModelConfigOptionID is the reserved SessionConfigOption id under which
// an external session surfaces the downstream agent's unstable model-picker
// state as one synthetic "select" option — the model parallel of
// AgentModeConfigOptionID. A set translates to the unstable session/set_model;
// its stateless response is adopted directly (no model-update notification
// exists to relay). Placed after the mode option, before the downstream's own.
const AgentModelConfigOptionID = "contenox.agent-model"

// syntheticModelOption maps a downstream SessionModelState onto the synthetic
// "Model" select (see AgentModelConfigOptionID). ok=false when there are no
// models to surface, mirroring syntheticModeOption.
func syntheticModelOption(m *libacp.SessionModelState) (libacp.SessionConfigOption, bool) {
	if m == nil || len(m.AvailableModels) == 0 {
		return libacp.SessionConfigOption{}, false
	}
	values := make([]libacp.SessionConfigValue, 0, len(m.AvailableModels))
	for _, model := range m.AvailableModels {
		values = append(values, libacp.SessionConfigValue{
			Value:       model.ID,
			Name:        model.Name,
			Description: model.Description,
		})
	}
	return libacp.SessionConfigOption{
		ID:           AgentModelConfigOptionID,
		Name:         "Model",
		Type:         libacp.SessionConfigOptionTypeSelect,
		CurrentValue: m.CurrentModelID,
		Options:      libacp.NewSessionConfigValues(values),
	}, true
}

// externalKillGrace bounds how long a spawned downstream agent's teardown waits
// for it to exit on stdin-close before killing it. Persistent agents rarely
// exit on stdin-close, so a short grace avoids stalling teardown on the
// default (see agenthost.KillGrace).
const externalKillGrace = 2 * time.Second

// parseAgentMeta extracts the AgentMetaKey value from a request `_meta`.
// Missing key, malformed json, or a non-string value all read as "" (native path).
func parseAgentMeta(meta json.RawMessage) string {
	if len(meta) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(meta, &m) != nil {
		return ""
	}
	raw, ok := m[AgentMetaKey]
	if !ok {
		return ""
	}
	var name string
	if json.Unmarshal(raw, &name) != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

// agentMetaJSON builds the `{"contenox.agent": name}` object echoed on the
// session/new response and each external session/list entry.
func agentMetaJSON(name string) json.RawMessage {
	return mustJSON(map[string]any{AgentMetaKey: name})
}

// sessionAgentRecord is the durable KV shape for an external session's agent
// name, so session/list attribution and the prompt path survive reconnects
// (the in-memory session map is empty after a restart). The KV records below
// follow the same pattern: keyed by upstream session id, best-effort
// persisted, deleted with the agent-name key on session delete, tolerant of
// a stale or missing value (falls back to a fresh bring-up/default).
type sessionAgentRecord struct {
	Agent string `json:"agent"`
}

const acpSessionAgentKVPrefix = "acp:session_agent:"

// acpSessionAgentCommandsKVPrefix and acpSessionAgentConfigOptionsKVPrefix
// store the downstream's advertised slash-command menu and config pickers
// (synthetic mode/model selects folded into the latter), so a reopened
// session restores its toolbar before ensureAttached lazily respawns it.
const (
	acpSessionAgentCommandsKVPrefix      = "acp:session_agent_commands:"
	acpSessionAgentConfigOptionsKVPrefix = "acp:session_agent_configoptions:"
)

func (t *Transport) persistSessionAgent(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID, agentName string) {
	if agentName == "" {
		return
	}
	raw, err := json.Marshal(sessionAgentRecord{Agent: agentName})
	if err != nil {
		return
	}
	if err := store.SetKV(ctx, acpSessionAgentKVPrefix+string(sid), raw); err != nil {
		reportErr, _, end := t.tracker().Start(ctx, "persist_agent", "acp_session", "session_id", string(sid))
		reportErr(err)
		end()
	}
}

func (t *Transport) readSessionAgent(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID) string {
	var rec sessionAgentRecord
	if err := store.GetKV(ctx, acpSessionAgentKVPrefix+string(sid), &rec); err != nil {
		return ""
	}
	return rec.Agent
}

// acpSessionInstanceKVPrefix stores the Manager-owned instance id backing an
// external session, so a session/load can re-attach (agentinstance.Attach) to
// the still-running instance instead of a fresh bring-up. Only the Instances
// path writes it.
const acpSessionInstanceKVPrefix = "acp:session_instance:"

type sessionInstanceRecord struct {
	InstanceID string `json:"instanceId"`
}

// persistSessionInstance builds its own store so the driver, which holds
// none, can call it.
func (t *Transport) persistSessionInstance(ctx context.Context, sid libacp.SessionID, instanceID string) {
	if t.deps.DB == nil || instanceID == "" {
		return
	}
	raw, err := json.Marshal(sessionInstanceRecord{InstanceID: instanceID})
	if err != nil {
		return
	}
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())
	if err := store.SetKV(ctx, acpSessionInstanceKVPrefix+string(sid), raw); err != nil {
		reportErr, _, end := t.tracker().Start(ctx, "persist_instance", "acp_session", "session_id", string(sid))
		reportErr(err)
		end()
	}
}

func (t *Transport) readSessionInstance(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID) string {
	var rec sessionInstanceRecord
	if err := store.GetKV(ctx, acpSessionInstanceKVPrefix+string(sid), &rec); err != nil {
		return ""
	}
	return rec.InstanceID
}

// acpSessionDownstreamKVPrefix stores the downstream agent's own session id
// (minted at downstream session/new). Since the externalBridge is a
// per-attachment viewer, not surviving state on the instance, a reconnecting
// Transport needs this to recover the downstream id and re-attach to the same
// downstream session, preserving the agent's context, instead of a fresh
// session/new.
const acpSessionDownstreamKVPrefix = "acp:session_downstream:"

type sessionDownstreamRecord struct {
	DownstreamID string `json:"downstreamId"`
}

// persistSessionDownstream mirrors persistSessionInstance.
func (t *Transport) persistSessionDownstream(ctx context.Context, sid libacp.SessionID, downstreamID libacp.SessionID) {
	if t.deps.DB == nil || downstreamID == "" {
		return
	}
	raw, err := json.Marshal(sessionDownstreamRecord{DownstreamID: string(downstreamID)})
	if err != nil {
		return
	}
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())
	if err := store.SetKV(ctx, acpSessionDownstreamKVPrefix+string(sid), raw); err != nil {
		reportErr, _, end := t.tracker().Start(ctx, "persist_downstream", "acp_session", "session_id", string(sid))
		reportErr(err)
		end()
	}
}

func (t *Transport) readSessionDownstream(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID) libacp.SessionID {
	var rec sessionDownstreamRecord
	if err := store.GetKV(ctx, acpSessionDownstreamKVPrefix+string(sid), &rec); err != nil {
		return ""
	}
	return libacp.SessionID(rec.DownstreamID)
}

// persistSessionAgentCommands builds its own store so the bridge, which holds
// none, can call it.
func (t *Transport) persistSessionAgentCommands(ctx context.Context, sid libacp.SessionID, cmds []libacp.AvailableCommand) {
	if t.deps.DB == nil {
		return
	}
	raw, err := json.Marshal(cmds)
	if err != nil {
		return
	}
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())
	if err := store.SetKV(ctx, acpSessionAgentCommandsKVPrefix+string(sid), raw); err != nil {
		reportErr, _, end := t.tracker().Start(ctx, "persist_agent_commands", "acp_session", "session_id", string(sid))
		reportErr(err)
		end()
	}
}

// persistSessionAgentConfigOptions mirrors persistSessionAgentCommands.
func (t *Transport) persistSessionAgentConfigOptions(ctx context.Context, sid libacp.SessionID, opts []libacp.SessionConfigOption) {
	if t.deps.DB == nil {
		return
	}
	raw, err := json.Marshal(opts)
	if err != nil {
		return
	}
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())
	if err := store.SetKV(ctx, acpSessionAgentConfigOptionsKVPrefix+string(sid), raw); err != nil {
		reportErr, _, end := t.tracker().Start(ctx, "persist_agent_config_options", "acp_session", "session_id", string(sid))
		reportErr(err)
		end()
	}
}

func (t *Transport) readSessionAgentCommands(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID) []libacp.AvailableCommand {
	var cmds []libacp.AvailableCommand
	if err := store.GetKV(ctx, acpSessionAgentCommandsKVPrefix+string(sid), &cmds); err != nil {
		return nil
	}
	return cmds
}

func (t *Transport) readSessionAgentConfigOptions(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID) []libacp.SessionConfigOption {
	var opts []libacp.SessionConfigOption
	if err := store.GetKV(ctx, acpSessionAgentConfigOptionsKVPrefix+string(sid), &opts); err != nil {
		return nil
	}
	return opts
}

// acpSessionHITLPolicyKVPrefix stores an external session's contenox-native
// HITL policy selection (the per-session approval policy gating its
// runtime-mediated actions, and driving beam's file-explorer HITL labels).
// The native chain path keeps this only in-memory; an external session
// persists it so its toolbar restores on session/load before any respawn.
const acpSessionHITLPolicyKVPrefix = "acp:session_hitl_policy:"

type sessionHITLPolicyRecord struct {
	Policy string `json:"policy"`
}

func (t *Transport) persistSessionHITLPolicy(ctx context.Context, sid libacp.SessionID, policy string) {
	if t.deps.DB == nil {
		return
	}
	raw, err := json.Marshal(sessionHITLPolicyRecord{Policy: policy})
	if err != nil {
		return
	}
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())
	if err := store.SetKV(ctx, acpSessionHITLPolicyKVPrefix+string(sid), raw); err != nil {
		reportErr, _, end := t.tracker().Start(ctx, "persist_hitl_policy", "acp_session", "session_id", string(sid))
		reportErr(err)
		end()
	}
}

func (t *Transport) readSessionHITLPolicy(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID) string {
	var rec sessionHITLPolicyRecord
	if err := store.GetKV(ctx, acpSessionHITLPolicyKVPrefix+string(sid), &rec); err != nil {
		return ""
	}
	return rec.Policy
}

// externalBridge bridges one external-agent-backed session's downstream stream
// to the connected upstream client, relaying session/update (remapping only
// the session id) and forwarding session/request_permission. Two ownership
// modes: on the connCtx path (Deps.Instances nil) the bridge IS the wired
// libacp.Client; on the Instances path it is an agentinstance.Viewer attached
// to a Manager-owned instance, which owns the real client and fans out to the
// bridge's Deliver (per-attachment: a reconnect builds a fresh bridge rather
// than inheriting one). terminal/* (external_terminal.go) is serviced only on
// the connCtx path; fs/* is refused on both via UnimplementedClient.
type externalBridge struct {
	libacp.UnimplementedClient

	// upstreamID is the stable upstream ACP session id this bridge serves (minted
	// at session/new, resupplied on every session/load). Set once at construction.
	upstreamID libacp.SessionID

	// viewerID is this bridge's per-attachment agentinstance.Viewer id (ID()), a
	// fresh uuid so Detach names exactly this bridge. Immutable.
	viewerID string

	// relayMu guards relayT (the currently-attached upstream Transport), relaySuppressed
	// (re-attach backlog gate), relayHeld/relayQueue, and the Manager viewer identity
	// (mgr + instanceID) used for self-detach. relayT clears — detaching the Manager
	// viewer too, if any — when its connection ends; a bare WebSocket drop fires
	// connCtx without calling Transport.Close, so attach() watches connCtx directly.
	// While relayT is nil, relays are dropped.
	relayMu sync.Mutex
	relayT  *Transport
	// relaySuppressed drops the upstream relay while set. It gates the journal replay on
	// a re-attach: Attach synchronously replays the instance's journal backlog, which the
	// durable chatservice transcript already replayed at session/load, so suppressing
	// during that replay avoids double-emitting the pre-drop turn. resumeRelay (called
	// right after Attach, before any prompt) re-enables live relay. Unset on a fresh
	// bring-up, whose journal is empty.
	relaySuppressed bool
	// relayHeld + relayQueue buffer the relay instead of dropping it (adopt.go): an
	// adopted session's journal replay is its only history, but Attach replays
	// synchronously — before the session/new response reaches the client — and a client
	// drops updates for a session id it hasn't learned yet (see bound). The replay is
	// queued in arrival order and flushed by releaseRelay once the response is on the
	// wire; live updates during the hold queue behind it, preserving order.
	relayHeld  bool
	relayQueue []libacp.SessionUpdate
	// mgr + instanceID name the Manager-owned instance this bridge views (Instances
	// path); both zero on the connCtx path. Set by bindInstance before Attach.
	mgr        agentinstance.Manager
	instanceID string
	// detachOnce makes viewer Detach idempotent across its two triggers: the connCtx
	// watcher (bare WS drop) and driver.Close.
	detachOnce sync.Once

	// mu guards capture, bound, cachedCommands, configOptions and related fields,
	// modeState/modelState, and downstreamID. capture is the per-turn transcript
	// accumulator (text, reasoning, tool calls): externalDriver.Prompt sets a fresh
	// capture before the downstream session/prompt and reads it back after, so the
	// whole turn can be persisted for session/load replay. SessionUpdate runs on the
	// downstream read-loop goroutine; externalDriver.Prompt runs on the upstream
	// request goroutine.
	mu      sync.Mutex
	capture *externalTurnCapture

	// downstreamID is the downstream agent's own session id, held on the bridge
	// (not just the per-connection externalDriver) so a re-attaching Transport
	// recovers it and drives the same downstream session without a fresh session/new.
	downstreamID libacp.SessionID

	// bound reports whether the upstream client can resolve upstreamID yet, i.e.
	// whether the session/new response has reached it. An available_commands_update
	// relayed before that point references an unlearned session and is silently
	// dropped by the client. While unbound the menu is cached, not relayed; markBound
	// (scheduled via libacp.AfterResponse from the session/new handler) flips this
	// and flushes the cache. A bridge spawned lazily after a session/load starts bound.
	bound bool
	// cachedCommands is the latest available_commands_update payload
	// (full-replacement per spec), kept for (re)emission once resolvable.
	cachedCommands []libacp.AvailableCommand

	// configOptions is the downstream agent's own advertised config-option set
	// (full-replacement per spec), seeded from session/new and replaced by each
	// config_option_update or confirmed session/set_config_option.
	// externalDriver.ConfigOptions returns it verbatim.
	configOptions []libacp.SessionConfigOption
	// configReceived records that a live config-option payload has superseded the
	// session/new seed, so a late seed write never clobbers it.
	configReceived bool
	// configOptionsPending mirrors cachedCommands' pre-bind gating for
	// config_option_update, including a current_mode_update surfaced as one
	// (over the synthetic mode id).
	configOptionsPending bool

	// modeState is the downstream's session Modes, seeded from session/new and kept
	// current by current_mode_update / confirmed session/set_mode. Folded into the
	// config-option output as the synthetic AgentModeConfigOptionID select (first,
	// ahead of configOptions). nil when the downstream advertises no modes.
	modeState *libacp.SessionModeState
	// modeReceived mirrors configReceived: a live mode change has landed, so a late
	// seed cannot clobber it.
	modeReceived bool
	// pendingModeID holds a currentModeId from a current_mode_update that raced ahead
	// of the session/new seed; seedModes applies it once availableModes arrive.
	pendingModeID string

	// modelState is the downstream's unstable model-picker state, seeded from
	// session/new and updated only by a confirmed session/set_model (applyModel).
	// Folded into the config-option output as the synthetic AgentModelConfigOptionID
	// select, after the mode option. nil when the downstream advertises no models.
	// Unlike modeState there is no received/pending race: the update stream carries
	// no model-update kind, so applyModel always runs strictly after the seed.
	modelState *libacp.SessionModelState

	// termMu guards terminals, the live downstream-created terminals for this
	// session keyed by bridge-minted id. Independent of mu so terminal lifecycle
	// never contends with the update-relay stream. Each bridgeTerminal owns a
	// scrollback watcher, torn down on terminal/release, terminal/kill, or
	// connection/session teardown (closeAllTerminals). See external_terminal.go.
	termMu    sync.Mutex
	terminals map[string]*bridgeTerminal
}

// newExternalBridge builds a bridge for upstreamID relaying to t, watching t's
// connection so it self-detaches when that connection ends. bound seeds the
// live-relay readiness (see the bound field).
func newExternalBridge(t *Transport, upstreamID libacp.SessionID, bound bool) *externalBridge {
	b := &externalBridge{upstreamID: upstreamID, bound: bound, viewerID: "acp-bridge-" + uuid.NewString()}
	b.attach(t)
	return b
}

// ID is the agentinstance.Viewer id this bridge registers and Detaches under.
func (b *externalBridge) ID() string { return b.viewerID }

// bindInstance records the Manager instance this bridge views, so the connCtx
// watcher and driver.Close can self-detach it. Called before Attach on the
// Instances path; never on the connCtx path.
func (b *externalBridge) bindInstance(mgr agentinstance.Manager, instanceID string) {
	b.relayMu.Lock()
	b.mgr = mgr
	b.instanceID = instanceID
	b.relayMu.Unlock()
}

// suppressReplay silences the upstream relay so the journal backlog Attach is
// about to replay is dropped (already replayed via chatservice at session/load).
// Paired with resumeRelay, called right after Attach returns.
func (b *externalBridge) suppressReplay() {
	b.relayMu.Lock()
	b.relaySuppressed = true
	b.relayMu.Unlock()
}

// resumeRelay re-enables the upstream relay after a suppressed re-attach replay,
// so subsequent live downstream updates reach the reconnected client.
func (b *externalBridge) resumeRelay() {
	b.relayMu.Lock()
	b.relaySuppressed = false
	b.relayMu.Unlock()
}

// holdRelay queues upstream relays instead of sending them, so an Attach that
// replays a journal before this connection's session/new response is on the
// wire loses nothing (see relayHeld). Must be paired with releaseRelay (from
// libacp.AfterResponse) or the queued backlog never reaches the client.
func (b *externalBridge) holdRelay() {
	b.relayMu.Lock()
	b.relayHeld = true
	b.relayMu.Unlock()
}

// releaseRelay flushes the held backlog in arrival order and returns the bridge
// to live relay. It drains under repeated locking rather than clearing the flag
// first, so a concurrently-produced update either joins the tail of the queue or
// relays live strictly after it — held and live streams never interleave out of
// order. A no-op when nothing was held.
func (b *externalBridge) releaseRelay(ctx context.Context) {
	for {
		b.relayMu.Lock()
		if len(b.relayQueue) == 0 {
			b.relayHeld = false
			b.relayMu.Unlock()
			return
		}
		batch := b.relayQueue
		b.relayQueue = nil
		t := b.relayT
		suppressed := b.relaySuppressed
		b.relayMu.Unlock()
		if t == nil || suppressed {
			continue // nothing to relay to; keep draining until the queue is empty
		}
		for _, upd := range batch {
			t.relayExternalUpdate(ctx, b.upstreamID, upd)
		}
	}
}

// attach binds the bridge's relay target to t and arms a watcher that detaches
// when t's connection ends. Wired to connCtx cancellation, not Transport.Close,
// because a bare WebSocket drop fires connCtx without calling Transport.Close
// (see transport.go). detachFrom is pointer-identity guarded, so a stale
// connection ending never clears a newer target.
func (b *externalBridge) attach(t *Transport) {
	b.relayMu.Lock()
	b.relayT = t
	b.relayMu.Unlock()
	go func() {
		<-t.connCtx.Done()
		b.detachFrom(t)
		b.detachViewer()
	}()
}

// detachFrom clears the relay target iff it is still t, so a re-attach to a
// newer Transport is never clobbered by an older one's later teardown.
func (b *externalBridge) detachFrom(t *Transport) {
	b.relayMu.Lock()
	if b.relayT == t {
		b.relayT = nil
	}
	b.relayMu.Unlock()
}

// detachViewer removes this bridge from its Manager instance's session fan-out.
// A no-op on the connCtx path and idempotent across its triggers (connCtx
// watcher, driver.Close). Never stops the instance — a viewer leaving keeps the
// agent running for reconnect; only DeleteSession / Manager.Close stop it.
func (b *externalBridge) detachViewer() {
	b.detachOnce.Do(func() {
		b.relayMu.Lock()
		mgr, instanceID := b.mgr, b.instanceID
		b.relayMu.Unlock()
		if mgr == nil || instanceID == "" {
			return
		}
		_ = mgr.Detach(instanceID, b.downstream(), b.viewerID)
	})
}

// transport returns the currently-attached upstream Transport, or nil when
// detached (updates are then dropped).
func (b *externalBridge) transport() *Transport {
	b.relayMu.Lock()
	defer b.relayMu.Unlock()
	return b.relayT
}

// relayUpstream forwards a downstream update to the currently-attached upstream
// client, remapping onto upstreamID. A no-op when detached or while the
// re-attach replay is suppressed; while held (adopt) it is queued instead, and
// releaseRelay sends it.
func (b *externalBridge) relayUpstream(ctx context.Context, upd libacp.SessionUpdate) {
	b.relayMu.Lock()
	if b.relayHeld && !b.relaySuppressed {
		b.relayQueue = append(b.relayQueue, upd)
		b.relayMu.Unlock()
		return
	}
	t := b.relayT
	suppressed := b.relaySuppressed
	b.relayMu.Unlock()
	if t == nil || suppressed {
		return
	}
	t.relayExternalUpdate(ctx, b.upstreamID, upd)
}

// Deliver is the agentinstance.Viewer fan-out entry point (Instances path): the
// instance's journaling harness calls it for each replayed and live
// session/update. Shares logic with SessionUpdate (the connCtx-path entry
// point) — both remap the downstream session id onto upstreamID and relay.
func (b *externalBridge) Deliver(ctx context.Context, n libacp.SessionNotification) error {
	return b.SessionUpdate(ctx, n)
}

// setDownstreamID records the downstream agent's session id after the handshake, so
// a re-attaching Transport recovers it via the surviving bridge.
func (b *externalBridge) setDownstreamID(id libacp.SessionID) {
	b.mu.Lock()
	b.downstreamID = id
	b.mu.Unlock()
}

// downstream returns the recorded downstream agent session id.
func (b *externalBridge) downstream() libacp.SessionID {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.downstreamID
}

// SessionUpdate relays a downstream session/update to the upstream client and,
// when a turn's capture is active, accumulates agent_message_chunk text for
// history persistence.
func (b *externalBridge) SessionUpdate(ctx context.Context, n libacp.SessionNotification) error {
	// available_commands_update: full-replacement slash-command menu. Cache it and
	// relay only once bound — a menu relayed before session/new is on the wire
	// references an unresolvable session and is dropped by the client; markBound
	// flushes the cache once bound.
	if n.Update.SessionUpdate == libacp.SessionUpdateAvailableCommands {
		b.mu.Lock()
		b.cachedCommands = n.Update.AvailableCommands
		relay := b.bound
		b.mu.Unlock()
		b.persistCommands(ctx, n.Update.AvailableCommands)
		if relay {
			b.relayUpstream(ctx, n.Update)
		}
		return nil
	}
	// config_option_update: full-replacement config pickers. Same pre-bind gating
	// as the command menu.
	if n.Update.SessionUpdate == libacp.SessionUpdateConfigOption {
		b.mu.Lock()
		b.configReceived = true
		b.configOptions = n.Update.ConfigOptions
		relay := b.bound
		if !relay {
			b.configOptionsPending = true
		}
		b.mu.Unlock()
		// configOptionsSurface has exactly one owner per path: the local fold above on
		// connCtx, the kernel's already-merged capture on Instances. Read outside b.mu.
		merged := b.configOptionsSurface()
		b.persistConfigOptions(ctx, merged)
		if relay {
			b.relayUpstream(ctx, libacp.SessionUpdate{
				SessionUpdate: libacp.SessionUpdateConfigOption,
				ConfigOptions: merged,
			})
		}
		return nil
	}
	// current_mode_update: contenox surfaces modes as the synthetic
	// AgentModeConfigOptionID option, so this is translated into a
	// config_option_update carrying the refreshed set; the raw update is not
	// forwarded. Same pre-bind gating.
	if n.Update.SessionUpdate == libacp.SessionUpdateCurrentMode {
		b.mu.Lock()
		b.modeReceived = true
		if b.modeState != nil {
			b.modeState.CurrentModeID = n.Update.CurrentModeID
		} else {
			// Raced ahead of the session/new seed; seedModes applies this later.
			b.pendingModeID = n.Update.CurrentModeID
		}
		relay := b.bound
		if !relay {
			b.configOptionsPending = true
		}
		b.mu.Unlock()
		merged := b.configOptionsSurface()
		b.persistConfigOptions(ctx, merged)
		if relay {
			b.relayUpstream(ctx, libacp.SessionUpdate{
				SessionUpdate: libacp.SessionUpdateConfigOption,
				ConfigOptions: merged,
			})
		}
		return nil
	}
	b.relayUpstream(ctx, n.Update)
	b.captureForHistory(n.Update)
	return nil
}

// captureForHistory records the transcript-bearing part of a downstream
// session/update into the active per-turn capture (a no-op outside a turn):
// assistant text, reasoning, and tool calls, so session/load replay
// reconstructs the full transcript rather than just the final text. Tool
// calls are merged by id across their tool_call/tool_call_update frames.
func (b *externalBridge) captureForHistory(u libacp.SessionUpdate) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.capture == nil {
		return
	}
	switch u.SessionUpdate {
	case libacp.SessionUpdateAgentMessageChunk:
		if c := u.Content; c != nil && c.Type == string(libacp.ContentKindText) {
			b.capture.addText(c.Text)
		}
	case libacp.SessionUpdateAgentThoughtChunk:
		if c := u.Content; c != nil && c.Type == string(libacp.ContentKindText) {
			b.capture.addThinking(c.Text)
		}
	case libacp.SessionUpdateToolCall, libacp.SessionUpdateToolCallUpdate:
		b.capture.addToolUpdate(u)
	}
}

// markBound records that the upstream client can now resolve this session
// (its session/new response is on the wire) and flushes the latest cached
// downstream available_commands_update. Scheduled via libacp.AfterResponse so
// the menu reaches the client strictly after the result, mirroring
// sendAvailableCommands' ordering contract for the native menu. Idempotent.
func (b *externalBridge) markBound(ctx context.Context) {
	b.mu.Lock()
	if b.bound {
		b.mu.Unlock()
		return
	}
	b.bound = true
	cmds := b.cachedCommands
	flushConfig := b.configOptionsPending
	b.mu.Unlock()
	var configOpts []libacp.SessionConfigOption
	if flushConfig {
		configOpts = b.configOptionsSurface()
	}
	if cmds != nil {
		b.relayUpstream(ctx, libacp.SessionUpdate{
			SessionUpdate:     libacp.SessionUpdateAvailableCommands,
			AvailableCommands: cmds,
		})
	}
	// A config/mode update that raced ahead of the session/new response is flushed
	// now that the client can resolve this session.
	if flushConfig {
		b.relayUpstream(ctx, libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateConfigOption,
			ConfigOptions: configOpts,
		})
	}
}

// seedConfigOptions records the downstream session/new response's advertised
// config options as the initial set, unless a live downstream update has
// already superseded the seed (the read-loop can deliver a config_option_update
// concurrently with initExternalConn capturing the session/new response).
func (b *externalBridge) seedConfigOptions(opts []libacp.SessionConfigOption) {
	b.mu.Lock()
	if !b.configReceived {
		b.configOptions = opts
	}
	b.mu.Unlock()
}

// applyConfigOptions adopts a downstream-confirmed option set (from a
// session/set_config_option response), marking the live set as received so a
// later seed cannot clobber it.
func (b *externalBridge) applyConfigOptions(opts []libacp.SessionConfigOption) {
	b.mu.Lock()
	b.configReceived = true
	b.configOptions = opts
	b.mu.Unlock()
}

// seedModes records the downstream session/new response's SessionModeState as
// the initial mode set (copied, so later mutations stay local). Folds in a
// pendingModeID left by a current_mode_update that raced ahead of the seed.
// Never overwrites an already-established modeState.
func (b *externalBridge) seedModes(m *libacp.SessionModeState) {
	if m == nil {
		return
	}
	b.mu.Lock()
	if b.modeState == nil {
		cp := *m
		if b.modeReceived && b.pendingModeID != "" {
			cp.CurrentModeID = b.pendingModeID
		}
		b.modeState = &cp
	}
	b.mu.Unlock()
}

// applyMode adopts an upstream-confirmed session mode into the synthetic
// option's currentValue. The set_mode response carries no state, so the
// requested modeId is authoritative; a downstream current_mode_update, if also
// emitted, reconfirms it.
func (b *externalBridge) applyMode(modeID string) {
	b.mu.Lock()
	b.modeReceived = true
	if b.modeState != nil {
		b.modeState.CurrentModeID = modeID
	} else {
		b.pendingModeID = modeID
	}
	b.mu.Unlock()
}

// seedModels records the downstream session/new response's SessionModelState as
// the initial model set (copied, so later mutation stays local). Mirrors
// seedModes but without the pending/received race fold-in: no model-update
// kind exists, so no live update can race this seed.
func (b *externalBridge) seedModels(m *libacp.SessionModelState) {
	if m == nil {
		return
	}
	b.mu.Lock()
	if b.modelState == nil {
		cp := *m
		b.modelState = &cp
	}
	b.mu.Unlock()
}

// applyModel adopts an upstream-confirmed model into the synthetic option's
// currentValue. The set_model response carries no state and no model-update
// notification exists, so the requested modelId is authoritative. No-op when
// the downstream advertised no models.
func (b *externalBridge) applyModel(modelID string) {
	b.mu.Lock()
	if b.modelState != nil {
		b.modelState.CurrentModelID = modelID
	}
	b.mu.Unlock()
}

// buildConfigOptionsLocked assembles the driver's full upstream config-option
// set: synthetic mode select (if any), then synthetic model select (if any),
// then the downstream's own config options. Caller holds b.mu.
func (b *externalBridge) buildConfigOptionsLocked() []libacp.SessionConfigOption {
	modeOpt, hasMode := syntheticModeOption(b.modeState)
	modelOpt, hasModel := syntheticModelOption(b.modelState)
	if !hasMode && !hasModel {
		return b.configOptions
	}
	out := make([]libacp.SessionConfigOption, 0, len(b.configOptions)+2)
	if hasMode {
		out = append(out, modeOpt)
	}
	if hasModel {
		out = append(out, modelOpt)
	}
	out = append(out, b.configOptions...)
	return out
}

// snapshotConfigOptions returns the driver's current upstream option set (see
// buildConfigOptionsLocked). It is the connCtx path's builder only — on the
// Instances path the surface belongs to the kernel, so read
// configOptionsSurface instead.
func (b *externalBridge) snapshotConfigOptions() []libacp.SessionConfigOption {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buildConfigOptionsLocked()
}

// configOptionsSurface returns the downstream-derived config-option surface
// this session advertises upstream: on the connCtx path the bridge owns it
// (snapshotConfigOptions); on the Instances path the kernel owns it
// (Manager.SessionConfigOptions already folds in the synthetic selects). Live
// updates keep both current, since the kernel captures them before fanning
// out to viewers. An unknown/stopped instance yields nil.
func (b *externalBridge) configOptionsSurface() []libacp.SessionConfigOption {
	b.relayMu.Lock()
	mgr, instanceID := b.mgr, b.instanceID
	b.relayMu.Unlock()
	if mgr == nil || instanceID == "" {
		return b.snapshotConfigOptions()
	}
	opts, err := mgr.SessionConfigOptions(instanceID, b.downstream())
	if err != nil {
		return nil
	}
	return opts
}

func (b *externalBridge) persistCommands(ctx context.Context, cmds []libacp.AvailableCommand) {
	if t := b.transport(); t != nil {
		t.persistSessionAgentCommands(ctx, b.upstreamID, cmds)
	}
}

func (b *externalBridge) persistConfigOptions(ctx context.Context, opts []libacp.SessionConfigOption) {
	if t := b.transport(); t != nil {
		t.persistSessionAgentConfigOptions(ctx, b.upstreamID, opts)
	}
}

// RequestPermission forwards the downstream agent's permission request to the
// upstream client, remapping the session id, reusing the same path
// AskApproval uses for the native engine.
func (b *externalBridge) RequestPermission(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	t := b.transport()
	if t == nil || t.conn == nil {
		return libacp.RequestPermissionResponse{}, libacp.InternalError("acpsvc: no upstream connection to relay permission to")
	}
	req.SessionID = b.upstreamID
	return t.conn.RequestPermission(ctx, req)
}

func (b *externalBridge) beginCapture() {
	b.mu.Lock()
	b.capture = &externalTurnCapture{}
	b.mu.Unlock()
}

func (b *externalBridge) finishCapture() []externalCaptureSegment {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.capture == nil {
		return nil
	}
	segs := b.capture.segments
	b.capture = nil
	return segs
}

// relayExternalUpdate forwards a downstream agent's session/update to the
// connected upstream client, remapping only the session id. Unlike sendUpdate
// it applies no tool-call normalization: the downstream agent owns its own
// tool-call framing. The one enrichment is on config_option_update: contenox's
// own per-session HITL policy select is appended (see externalConfigOptionsForRelay).
func (t *Transport) relayExternalUpdate(ctx context.Context, upstreamID libacp.SessionID, upd libacp.SessionUpdate) {
	if t.conn == nil {
		return
	}
	if upd.SessionUpdate == libacp.SessionUpdateConfigOption {
		upd.ConfigOptions = t.externalConfigOptionsForRelay(upstreamID, upd.ConfigOptions)
	}
	reportErr, _, end := t.tracker().Start(ctx, "relay", "acp_session_update",
		"session_id", string(upstreamID), "kind", string(upd.SessionUpdate))
	defer end()
	if err := t.conn.SessionUpdate(libacp.SessionNotification{SessionID: upstreamID, Update: upd}); err != nil {
		reportErr(err)
	}
}

// externalConfigOptionsForRelay appends contenox's own per-session HITL policy
// select to a downstream/merged config-option set bound for a relayed
// config_option_update. The upstream client applies a config_option_update as
// a wholesale replacement of the session's options, so a relay carrying only
// the downstream surface would blank the HITL picker (and its file-explorer
// labels) until the next set_config_option response restored it. Falls back
// to the bare set when the session is not resolvable.
func (t *Transport) externalConfigOptionsForRelay(sid libacp.SessionID, downstream []libacp.SessionConfigOption) []libacp.SessionConfigOption {
	sess, ok := t.sessionFor(sid)
	if !ok {
		return downstream
	}
	out := make([]libacp.SessionConfigOption, 0, len(downstream)+1)
	out = append(out, downstream...)
	out = append(out, t.hitlPolicyConfigOption(sess))
	return out
}

// externalDriver drives a session against a registered downstream ACP agent
// instead of the native chain engine. The downstream connection is acquired
// lazily via ensureAttached (session/new acquires eagerly; the first prompt
// after a session/load re-attaches or freshly brings one up) and released on Close.
//
// Two ownership modes:
//   - Manager-owned (Deps.Instances set): the connection belongs to an
//     agentinstance.Manager instance (instanceID) that outlives this connection.
//     handle is nil; Close detaches the bridge but leaves the instance running.
//   - connCtx-owned (Deps.Instances nil): the driver spawns and owns the
//     subprocess (handle) bound to the connection's connCtx; Close closes it.
type externalDriver struct {
	t         *Transport
	agentName string

	// upstreamID is the ACP session id this driver serves. Kept so a config
	// change routed through the native HITL policy path can persist under the
	// session's key even with no downstream spawned yet. Set once, never mutated.
	upstreamID libacp.SessionID

	// mu guards the live downstream state below. The attached/not-attached
	// sentinel is bridge (nil = not attached), not conn, which is nil on the
	// Instances path by design.
	mu sync.Mutex
	// conn is the raw downstream connection, set only on the connCtx-owned path.
	// On the Instances path it stays nil: the Manager owns the connection and
	// every operation is driven through its session API.
	conn *libacp.ClientSideConnection
	// handle is the connCtx-owned subprocess handle, set only on that path.
	handle *agenthost.Handle
	// instanceID names the Manager-owned instance backing this session, set
	// only on the Instances path. What a reconnect re-attaches to
	// (agentinstance.Attach) and what DeleteSession stops.
	instanceID   string
	downstreamID libacp.SessionID
	bridge       *externalBridge
}

// AgentName returns the registered downstream agent name, echoed in session/new's
// `_meta` and used for session/list attribution.
func (d *externalDriver) AgentName() string { return d.agentName }

// AvailableCommands returns nil: an external session relays the downstream
// agent's own menu live instead of contenox's slash commands.
func (d *externalDriver) AvailableCommands() []libacp.AvailableCommand { return nil }

// ConfigOptions returns the downstream agent's own advertised config options —
// prefixed by the synthetic "Mode" select (AgentModeConfigOptionID) and
// "Model" select (AgentModelConfigOptionID) when advertised, kept current by
// update relays and confirmed set_config_option/set_mode/set_model calls — and
// suffixed by contenox's own per-session HITL policy select. The HITL policy
// is a real per-session capability (it gates the foreign agent's
// runtime-mediated actions and drives beam's file-explorer labels), so it is
// always appended even before the downstream is spawned (nil bridge on a
// loaded session), when the lazy respawn later pushes an update to restore
// the downstream pickers.
func (d *externalDriver) ConfigOptions(_ context.Context, sess *sessionEntry) []libacp.SessionConfigOption {
	d.mu.Lock()
	bridge := d.bridge
	d.mu.Unlock()
	var base []libacp.SessionConfigOption
	if bridge != nil {
		base = bridge.configOptionsSurface()
	}
	// Fresh slice avoids aliasing the bridge's backing array.
	out := make([]libacp.SessionConfigOption, 0, len(base)+1)
	out = append(out, base...)
	out = append(out, d.t.hitlPolicyConfigOption(sess))
	return out
}

// SetConfigOption forwards an upstream config-option change to the downstream
// agent's session/set_config_option and adopts the option set it confirms, so
// the upstream response reflects the downstream's authoritative value. The
// value union (string/boolean) is forwarded intact. Contenox performs no
// upstream validation: the downstream owns its option semantics.
func (d *externalDriver) SetConfigOption(ctx context.Context, sess *sessionEntry, configID string, value libacp.SessionConfigOptionValue) error {
	// contenox's own per-session HITL policy is enforced by the runtime, not the
	// downstream agent: route through the native per-session path and persist it,
	// never forwarding it downstream. Works even with a nil bridge (loaded
	// session, downstream not yet spawned) since it needs no connection.
	if configID == configIDHITLPolicy {
		if err := d.t.setSessionConfigOption(ctx, sess, configID, value.AsString()); err != nil {
			return err
		}
		d.t.persistSessionHITLPolicy(ctx, d.upstreamID, sess.hitlPolicy())
		return nil
	}

	d.mu.Lock()
	conn, instanceID, downstreamID, bridge := d.conn, d.instanceID, d.downstreamID, d.bridge
	d.mu.Unlock()
	if bridge == nil {
		return libacp.NewError(libacp.ErrInvalidParams, "external agent session is not active")
	}
	// Instances path: the kernel performs this same mapping (synthetic mode/model
	// ids to session/set_mode / session/set_model, everything else to
	// session/set_config_option) and owns the confirmed state (see
	// configOptionsSurface), so only the transport-side DB persistence stays here.
	if instanceID != "" {
		if err := d.t.deps.Instances.SetConfigOption(ctx, instanceID, downstreamID, configID, value); err != nil {
			return err
		}
		bridge.persistConfigOptions(ctx, bridge.configOptionsSurface())
		return nil
	}
	if conn == nil {
		return libacp.NewError(libacp.ErrInvalidParams, "external agent session is not active")
	}
	// The synthetic mode option is not a real downstream option: a set on its
	// reserved id translates to session/set_mode, adopted into currentValue.
	// Every other id forwards to session/set_config_option unchanged.
	if configID == AgentModeConfigOptionID {
		if _, err := conn.SetSessionMode(ctx, libacp.SetSessionModeRequest{
			SessionID: downstreamID,
			ModeID:    value.AsString(),
		}); err != nil {
			return err
		}
		bridge.applyMode(value.AsString())
		bridge.persistConfigOptions(ctx, bridge.snapshotConfigOptions())
		return nil
	}
	// The synthetic model option likewise translates to the unstable
	// session/set_model. The response is stateless and no model-update
	// notification exists, so the requested id is authoritative — nothing to
	// relay, unlike the mode path's current_mode_update.
	if configID == AgentModelConfigOptionID {
		if _, err := conn.SetSessionModel(ctx, libacp.SetSessionModelRequest{
			SessionID: downstreamID,
			ModelID:   value.AsString(),
		}); err != nil {
			return err
		}
		bridge.applyModel(value.AsString())
		bridge.persistConfigOptions(ctx, bridge.snapshotConfigOptions())
		return nil
	}
	resp, err := conn.SetSessionConfigOption(ctx, libacp.SetSessionConfigOptionRequest{
		SessionID: downstreamID,
		ConfigID:  configID,
		Value:     value,
	})
	if err != nil {
		return err
	}
	bridge.applyConfigOptions(resp.ConfigOptions)
	bridge.persistConfigOptions(ctx, bridge.snapshotConfigOptions())
	return nil
}

// Close releases this connection's hold on the downstream agent. Its meaning
// depends on ownership (see externalDriver):
//
//   - Manager-owned instance (instanceID set): detach only. The instance is
//     left running for reconnect; only DeleteSession / Manager.Close stop it.
//   - connCtx-owned subprocess (handle set): closes the handle, tearing down
//     the subprocess the driver owns.
//
// Idempotent and safe when nothing was attached.
func (d *externalDriver) Close() error {
	d.mu.Lock()
	handle := d.handle
	bridge := d.bridge
	instanceID := d.instanceID
	d.handle = nil
	d.conn = nil
	d.bridge = nil
	d.downstreamID = ""
	d.mu.Unlock()
	// Tear down any live downstream-created terminals before dropping the
	// connection, so no watcher goroutine leaks. The serve WebSocket path
	// (which never calls Transport.Close) is covered independently: each
	// terminal's owner goroutine watches connCtx itself.
	if bridge != nil {
		bridge.closeAllTerminals()
	}
	// Manager-owned: detach the viewer and clear its relay target, but leave
	// the instance running. detachViewer is idempotent with the connCtx
	// watcher's detach; detachFrom is pointer-identity guarded.
	if instanceID != "" {
		if bridge != nil {
			bridge.detachViewer()
			bridge.detachFrom(d.t)
		}
		return nil
	}
	if handle != nil {
		return handle.Close()
	}
	return nil
}

// storeMcpResolver adapts a runtimetypes.Store to agenthost.McpServerResolver so
// an external agent's mcp_servers allowlist can be resolved to ACP session/new
// wire shapes without acpsvc depending on mcpserverservice.
type storeMcpResolver struct {
	store runtimetypes.Store
}

func (r storeMcpResolver) GetByName(ctx context.Context, name string) (*runtimetypes.MCPServer, error) {
	return r.store.GetMCPServerByName(ctx, name)
}

// resolveExternalAgent resolves a registered agent by name and returns both
// the declared record and its external_acp config, rejecting an unknown or
// disabled agent with a clear JSON-RPC error.
//
// The record is returned, not just the config, so the Manager-owned branch of
// bringUpExternal can spawn from this same read (Instances.StartResolved)
// instead of the kernel re-reading the row: the Enabled check and the spawn
// must be made against the same bytes, or an agent disabled in between still
// spawns.
func (t *Transport) resolveExternalAgent(ctx context.Context, name string) (*runtimetypes.Agent, *runtimetypes.ExternalACPConfig, error) {
	if t.deps.DB == nil {
		return nil, nil, libacp.InternalError("acpsvc: no database configured for external agents")
	}
	reg := agentregistryservice.New(t.deps.DB)
	// ResolveForSpawn is the one shared disabled-agent check every spawn path
	// uses (fleetservice.Dispatch too), so the judgment can't drift between
	// the REST dispatch path and this chat path.
	agent, err := agentregistryservice.ResolveForSpawn(ctx, reg, name)
	if err != nil {
		if errors.Is(err, agentregistryservice.ErrAgentDisabled) {
			return nil, nil, libacp.NewErrorf(libacp.ErrInvalidParams, "%v", err)
		}
		if errors.Is(err, libdb.ErrNotFound) {
			return nil, nil, libacp.NewErrorf(libacp.ErrInvalidParams, "unknown contenox.agent %q", name)
		}
		return nil, nil, libacp.InternalError(fmt.Sprintf("acpsvc: resolve agent %q: %v", name, err))
	}
	// A chain-kind agent has no external_acp config to read — its config names
	// a chain file, and the Manager builds the spawn from it — so asking for
	// one would wrongly refuse a runnable agent. The zero config returned here
	// is the truthful answer for its one consumer, the mcp_servers allowlist
	// (resolveMcpAllowlist): a chain unit runs this runtime's own tools and
	// forwards none. bringUpExternal's connCtx branch refuses chain kind
	// itself rather than spawning from these zero bytes.
	if agent.Kind == runtimetypes.AgentKindChain {
		return agent, &runtimetypes.ExternalACPConfig{}, nil
	}
	cfg, err := agent.ExternalACPConfig()
	if err != nil {
		return nil, nil, libacp.NewErrorf(libacp.ErrInvalidParams, "contenox.agent %q: %v", name, err)
	}
	return agent, cfg, nil
}

// filterMcpForCaps mirrors agenthost's filter semantics: stdio is the protocol
// baseline and always passes; http and sse are gated on the downstream agent's
// initialize-advertised mcpCapabilities. Servers it cannot consume are dropped
// rather than forwarded to be silently ignored.
func filterMcpForCaps(servers []libacp.McpServer, caps libacp.McpCapabilities) ([]libacp.McpServer, []string) {
	kept := make([]libacp.McpServer, 0, len(servers))
	var dropped []string
	for _, srv := range servers {
		switch srv.Kind() {
		case libacp.McpServerKindHTTP:
			if !caps.HTTP {
				dropped = append(dropped, srv.Name)
				continue
			}
		case libacp.McpServerKindSSE:
			if !caps.SSE {
				dropped = append(dropped, srv.Name)
				continue
			}
		}
		kept = append(kept, srv)
	}
	return kept, dropped
}

// externalAttach is the result of bringing up a downstream connection for an
// external session: whichever ownership token applies — handle + conn
// (connCtx-owned) xor instanceID (Manager-owned, conn stays nil). teardown
// reverses exactly the one that was set.
type externalAttach struct {
	conn         *libacp.ClientSideConnection // set on the nil-Instances path only
	handle       *agenthost.Handle            // set on the nil-Instances path
	instanceID   string                       // set on the Instances path
	downstreamID libacp.SessionID
	bridge       *externalBridge
}

// teardown reverses a bring-up: closes the connCtx-owned subprocess, or stops
// the Manager-owned instance. Used when a caller discards this attach (a
// failed session/new, or a lost lazy-attach race).
func (ea *externalAttach) teardown(t *Transport) {
	if ea.handle != nil {
		_ = ea.handle.Close()
	}
	if ea.instanceID != "" && t.deps.Instances != nil {
		_ = t.deps.Instances.Stop(ea.instanceID)
	}
}

// resolveMcpAllowlist resolves a declared external agent's mcp_servers
// allowlist (names) against the store into ACP session/new wire shapes. It
// stays on the transport on both paths: resolution needs the DB, which the
// kernel deliberately does not have (agentinstance.SessionSpec.McpServers
// takes an already-resolved set).
func (t *Transport) resolveMcpAllowlist(ctx context.Context, cfg *runtimetypes.ExternalACPConfig, agentName string) ([]libacp.McpServer, error) {
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())
	servers, err := agenthost.ResolveForwardedMcpServers(ctx, storeMcpResolver{store: store}, cfg.McpServers)
	if err != nil {
		return nil, libacp.InternalError(fmt.Sprintf("acpsvc: resolve mcp allowlist for agent %q: %v", agentName, err))
	}
	return servers, nil
}

// openInstanceSession drives the downstream handshake for a Manager-owned
// instance, the Instances-path counterpart of initExternalConn. It holds no
// connection: the kernel owns the handshake and capture of the advertised
// surface, returning only the downstream session id; this transport resolves
// the mcp allowlist and persists the advertised options. SessionSpec.Terminal
// is deliberately false: the instance's own harness answers terminal/* with
// MethodNotFound, so advertising it would route commands to a dead surface.
// The caller owns teardown on failure (Stop the instance).
func (t *Transport) openInstanceSession(ctx context.Context, instanceID string, bridge *externalBridge, cfg *runtimetypes.ExternalACPConfig, cwd, agentName string) (libacp.SessionID, error) {
	mcpServers, err := t.resolveMcpAllowlist(ctx, cfg, agentName)
	if err != nil {
		return "", err
	}
	downstreamID, err := t.deps.Instances.OpenSession(ctx, instanceID, agentinstance.SessionSpec{
		Cwd:        cwd,
		McpServers: mcpServers,
		Terminal:   false,
	})
	if err != nil {
		return "", libacp.InternalError(fmt.Sprintf("acpsvc: open session on agent %q instance: %v", agentName, err))
	}
	bridge.setDownstreamID(downstreamID)
	// Persist the kernel's captured surface so a session/load before the first
	// prompt can restore the pickers; each (re)open overwrites with fresh truth.
	bridge.persistConfigOptions(ctx, bridge.configOptionsSurface())
	return downstreamID, nil
}

// initExternalConn drives the downstream ACP handshake (initialize +
// session/new) on an already-connected downstream connection wired to bridge,
// seeding its advertised surface and recording the downstream session id on
// the bridge (so a reconnect recovers it). This is the connCtx-spawn path
// only; the Manager-instance path drives the same handshake through the
// kernel (openInstanceSession), holding no connection. On failure the caller
// owns teardown of the connection.
func (t *Transport) initExternalConn(ctx context.Context, conn *libacp.ClientSideConnection, bridge *externalBridge, cfg *runtimetypes.ExternalACPConfig, cwd, agentName string, terminalCapable bool) (libacp.SessionID, error) {
	mcpServers, err := t.resolveMcpAllowlist(ctx, cfg, agentName)
	if err != nil {
		return "", err
	}

	// Advertise the terminal client capability only when this path can service
	// terminal/* (a shell manager is present, and the bridge is the wired
	// client here) — this routes the downstream's shell commands through the
	// runtime's terminals (external_terminal.go). The Instances path withholds
	// it, see openInstanceSession.
	clientCaps := libacp.ClientCapabilities{}
	if terminalCapable {
		clientCaps.Terminal = true
	}
	init, err := conn.Initialize(ctx, libacp.InitializeRequest{
		ProtocolVersion:    libacp.ProtocolVersion,
		ClientCapabilities: clientCaps,
		ClientInfo:         &libacp.Implementation{Name: "contenox", Version: version.Get()},
	})
	if err != nil {
		return "", libacp.InternalError(fmt.Sprintf("acpsvc: initialize agent %q: %v", agentName, err))
	}
	if init.ProtocolVersion != libacp.ProtocolVersion {
		return "", libacp.InternalError(fmt.Sprintf("acpsvc: agent %q negotiated unsupported protocol version %d", agentName, init.ProtocolVersion))
	}

	forwarded, _ := filterMcpForCaps(mcpServers, init.AgentCapabilities.McpCapabilities)
	if forwarded == nil {
		forwarded = []libacp.McpServer{}
	}
	downstream, err := conn.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: forwarded,
	})
	if err != nil {
		return "", libacp.InternalError(fmt.Sprintf("acpsvc: session/new against agent %q: %v", agentName, err))
	}
	// Capture the downstream's config options, session modes, and unstable
	// model-picker state so the upstream session/new response carries them
	// synchronously. Seeding config/modes yields to any concurrent
	// config_option_update / current_mode_update; the model seed has no such
	// race (no model-update kind exists on the stream).
	bridge.seedConfigOptions(downstream.ConfigOptions)
	bridge.seedModes(downstream.Modes)
	bridge.seedModels(downstream.Models)
	bridge.setDownstreamID(downstream.SessionID)
	// Persist the current live option set so a session/load before the first
	// prompt can restore the pickers; every (re)spawn overwrites it.
	bridge.persistConfigOptions(ctx, bridge.snapshotConfigOptions())
	return downstream.SessionID, nil
}

// bringUpExternal establishes a fresh downstream connection for agentName
// under a new bridge and drives its handshake. When Deps.Instances is wired
// the downstream is a Manager-owned instance that survives this connection's
// teardown; otherwise it is a subprocess bound to this connection's connCtx.
// Every failure after connect tears the connection back down.
//
// bound seeds the bridge's readiness to relay the slash-command menu live:
// false for the eager session/new bring-up (menu cached, re-emitted by
// markBound after the response), true for a lazy bring-up after a
// session/load (the upstream session already exists).
func (t *Transport) bringUpExternal(ctx context.Context, upstreamID libacp.SessionID, cwd, agentName string, bound bool) (*externalAttach, error) {
	agent, cfg, err := t.resolveExternalAgent(ctx, agentName)
	if err != nil {
		return nil, err
	}
	bridge := newExternalBridge(t, upstreamID, bound)

	if t.deps.Instances != nil {
		// Manager-owned: bound to the Manager's root ctx, so it outlives this
		// connection. This driver drives the downstream through the Manager's
		// session API and observes it by attaching the bridge as a viewer — it
		// holds no connection. Any failure stops the instance.
		//
		// StartResolved, not Start(agentName): spawning from the record
		// resolveExternalAgent already read (rather than a second lookup)
		// closes the TOCTOU window and saves a query.
		instanceID, err := t.deps.Instances.StartResolved(ctx, agent, cwd)
		if err != nil {
			return nil, libacp.InternalError(fmt.Sprintf("acpsvc: start agent %q instance: %v", agentName, err))
		}
		// Bind before the handshake: openInstanceSession persists the
		// kernel-owned surface, readable only once the bridge knows its instance.
		bridge.bindInstance(t.deps.Instances, instanceID)
		downstreamID, err := t.openInstanceSession(ctx, instanceID, bridge, cfg, cwd, agentName)
		if err != nil {
			_ = t.deps.Instances.Stop(instanceID)
			return nil, err
		}
		// Attach the bridge as a viewer of the downstream session; the first
		// viewer becomes the session's controller and answers permission
		// requests. A fresh instance's journal is empty, so no suppression needed.
		if _, err := t.deps.Instances.Attach(ctx, instanceID, downstreamID, bridge); err != nil {
			_ = t.deps.Instances.Stop(instanceID)
			return nil, libacp.InternalError(fmt.Sprintf("acpsvc: attach to agent %q instance: %v", agentName, err))
		}
		return &externalAttach{instanceID: instanceID, downstreamID: downstreamID, bridge: bridge}, nil
	}

	// connCtx-owned (fallback): the subprocess dies with this connection, and
	// the bridge is the wired libacp.Client, so terminal/* is serviced here
	// when a shell manager is present.
	//
	// A chain agent has no external_acp config (resolveExternalAgent hands
	// back a deliberately zero one), so building a subprocess from it would
	// spawn nothing coherent; refuse and name the remedy. A chain unit needs
	// the Manager, which the bare stdio transport does not wire.
	if agent.Kind == runtimetypes.AgentKindChain {
		return nil, libacp.NewErrorf(libacp.ErrInvalidParams,
			"contenox.agent %q is a chain agent, which this transport cannot run: chain units are spawned by the fleet manager (fire them as a mission from an editor session, e.g. `/mission %s <intent>`)", agentName, agentName)
	}
	// Seed the sandbox workspace from this session's cwd when the agent
	// declares none of its own, mirroring the Instances and one-shot paths. A
	// declared Cwd still wins.
	spawnCfg := *cfg
	if spawnCfg.Cwd == "" {
		spawnCfg.Cwd = cwd
	}
	host := &agenthost.ExternalACPAgent{Config: spawnCfg, KillGrace: externalKillGrace}
	handle, err := host.Connect(t.connCtx, bridge)
	if err != nil {
		return nil, libacp.InternalError(fmt.Sprintf("acpsvc: spawn agent %q: %v", agentName, err))
	}
	downstreamID, err := t.initExternalConn(ctx, handle.Conn, bridge, cfg, cwd, agentName, t.deps.ShellSessions != nil)
	if err != nil {
		_ = handle.Close()
		return nil, err
	}
	return &externalAttach{conn: handle.Conn, handle: handle, downstreamID: downstreamID, bridge: bridge}, nil
}

// externalTarget names the live downstream a driver call drives. It carries
// exactly one of the two ownership modes (see externalDriver): a raw
// connection the driver owns (connCtx path), or a Manager instance id to
// drive through (Instances path, conn nil). Every downstream operation
// branches on instanceID exactly once, in the drive* helpers below.
type externalTarget struct {
	conn         *libacp.ClientSideConnection
	instanceID   string
	downstreamID libacp.SessionID
	bridge       *externalBridge
}

// ensureAttached returns the driver's live downstream target, acquiring it
// lazily on first use. On the Manager path a session/load persisted the
// instanceID, so the first prompt after a load re-attaches to the
// still-running instance, preserving the downstream's context, falling back
// to a fresh bring-up only when that instance is gone. On the connCtx path
// there is no instance to re-attach to: the first prompt after a load
// respawns, restarting the downstream's context (a v1 limit of that path).
//
// The attached/not-attached sentinel is the bridge, not the connection: the
// Instances path holds no connection at all, so a nil conn means
// "kernel-owned", not "detached".
func (d *externalDriver) ensureAttached(ctx context.Context, upstreamID libacp.SessionID, sess *sessionEntry) (*externalTarget, error) {
	d.mu.Lock()
	if d.bridge != nil {
		tgt := &externalTarget{conn: d.conn, instanceID: d.instanceID, downstreamID: d.downstreamID, bridge: d.bridge}
		d.mu.Unlock()
		return tgt, nil
	}
	instanceID, downstreamID := d.instanceID, d.downstreamID
	d.mu.Unlock()

	// Re-attach to a still-running Manager instance first (survives reconnect).
	// The bridge does not survive on the instance, so a reconnect builds a
	// fresh viewer keyed by the persisted downstream session id, driving
	// prompts against the same downstream session — preserving its context.
	// No connection is fetched; the instance is driven through the Manager's
	// session API. The journal replay is suppressed since chatservice already
	// replayed the pre-drop turn at session/load.
	if d.t.deps.Instances != nil && instanceID != "" && downstreamID != "" {
		if st, err := d.t.deps.Instances.Get(instanceID); err == nil && st.State == agentinstance.StateRunning {
			bridge := newExternalBridge(d.t, upstreamID, true)
			bridge.setDownstreamID(downstreamID)
			bridge.suppressReplay()
			bridge.bindInstance(d.t.deps.Instances, instanceID)
			if _, err := d.t.deps.Instances.Attach(ctx, instanceID, downstreamID, bridge); err == nil {
				return d.commitReattach(instanceID, downstreamID, bridge), nil
			}
			// Attach failed: drop this redundant bridge and fall through to a
			// fresh bring-up, which restarts the downstream's context.
			bridge.detachFrom(d.t)
		}
		// Instance gone/stopped/errored: fall through to a fresh bring-up below.
	}

	sess.mu.Lock()
	cwd := sess.Cwd
	sess.mu.Unlock()

	att, err := d.t.bringUpExternal(ctx, upstreamID, cwd, d.agentName, true)
	if err != nil {
		return nil, err
	}
	return d.commitBringUp(ctx, upstreamID, att)
}

// commitReattach adopts a re-attached Manager instance and its fresh viewer
// bridge onto the driver, re-enabling the suppressed relay now that Attach's
// backlog replay has drained. No config push is needed: session/load already
// restored the reopened toolbar from the persisted set (reloadedConfigOptions).
func (d *externalDriver) commitReattach(instanceID string, downstreamID libacp.SessionID, bridge *externalBridge) *externalTarget {
	bridge.resumeRelay()
	d.mu.Lock()
	if d.bridge != nil {
		// Lost a race: another prompt re-attached first. Detach our redundant
		// viewer and use the winner.
		won := &externalTarget{conn: d.conn, instanceID: d.instanceID, downstreamID: d.downstreamID, bridge: d.bridge}
		d.mu.Unlock()
		bridge.detachViewer()
		bridge.detachFrom(d.t)
		return won
	}
	d.bridge = bridge
	d.instanceID = instanceID
	d.downstreamID = downstreamID
	d.mu.Unlock()
	return &externalTarget{instanceID: instanceID, downstreamID: downstreamID, bridge: bridge}
}

// commitBringUp adopts a freshly brought-up downstream onto the driver (winner
// logic for a concurrent prompt), persists the Manager instanceID and
// downstream session id so a later reconnect targets the same session, and
// pushes the config options to restore the reloaded toolbar (lazy bring-up case).
func (d *externalDriver) commitBringUp(ctx context.Context, upstreamID libacp.SessionID, att *externalAttach) (*externalTarget, error) {
	d.mu.Lock()
	if d.bridge != nil {
		// Lost a race: keep the winner, discard ours.
		won := &externalTarget{conn: d.conn, instanceID: d.instanceID, downstreamID: d.downstreamID, bridge: d.bridge}
		d.mu.Unlock()
		att.teardown(d.t)
		return won, nil
	}
	d.conn = att.conn
	d.handle = att.handle
	d.instanceID = att.instanceID
	d.downstreamID = att.downstreamID
	d.bridge = att.bridge
	d.mu.Unlock()
	if att.instanceID != "" {
		d.t.persistSessionInstance(ctx, upstreamID, att.instanceID)
		d.t.persistSessionDownstream(ctx, upstreamID, att.downstreamID)
	}
	if opts := att.bridge.configOptionsSurface(); len(opts) > 0 {
		d.t.relayExternalUpdate(ctx, upstreamID, libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateConfigOption,
			ConfigOptions: opts,
		})
	}
	return &externalTarget{conn: att.conn, instanceID: att.instanceID, downstreamID: att.downstreamID, bridge: att.bridge}, nil
}

// promptDownstream drives one downstream prompt turn against tgt and returns
// its stop reason. On the Instances path the kernel owns the connection, so
// the turn goes through Manager.Prompt, which is itself cancellation-aware
// and resolves a cancelled turn as StopReasonCancelled with a nil error.
func (d *externalDriver) promptDownstream(ctx context.Context, tgt *externalTarget, prompt []libacp.ContentBlock) (libacp.StopReason, error) {
	if tgt.instanceID != "" {
		return d.t.deps.Instances.Prompt(ctx, tgt.instanceID, tgt.downstreamID, prompt)
	}
	resp, err := tgt.conn.Prompt(ctx, libacp.PromptRequest{SessionID: tgt.downstreamID, Prompt: prompt})
	if err != nil {
		return "", err
	}
	return resp.StopReason, nil
}

// cancelDownstream cancels tgt's in-flight downstream turn (session/cancel plus the
// prompt-turn permission auto-resolve). Best-effort on both paths.
func (d *externalDriver) cancelDownstream(tgt *externalTarget) {
	if tgt.instanceID != "" {
		_ = d.t.deps.Instances.Cancel(tgt.instanceID, tgt.downstreamID)
		return
	}
	_ = tgt.conn.CancelPrompt(tgt.downstreamID)
}

// Prompt forwards a prompt to the session's downstream agent, bypassing
// slash-command interception and the native chain engine: the prompt blocks
// go straight to the downstream session/prompt and its stopReason is
// returned. Upstream session/cancel forwards downstream as session/cancel,
// and the turn is persisted so session/list titles and replay work.
func (d *externalDriver) Prompt(ctx context.Context, req libacp.PromptRequest, sess *sessionEntry) (libacp.PromptResponse, error) {
	t := d.t
	reportErr, reportChange, end := t.tracker().Start(ctx, "prompt", "acp_external_session", "session_id", string(req.SessionID))
	defer end()

	tgt, err := d.ensureAttached(ctx, req.SessionID, sess)
	if err != nil {
		reportErr(err)
		return libacp.PromptResponse{}, err
	}

	// Forward an upstream session/cancel to the downstream agent as
	// session/cancel. Registered so Transport.Cancel, Close/Delete, or a
	// connection drop invokes it; sync.Once keeps the deferred unregister from
	// sending a stray cancel after normal completion.
	var cancelOnce sync.Once
	cancel := func() {
		cancelOnce.Do(func() { d.cancelDownstream(tgt) })
	}
	promptReg := t.registerPromptCancel(req.SessionID, cancel)
	defer t.unregisterPromptCancel(req.SessionID, promptReg)

	tgt.bridge.beginCapture()
	stopReason, promptErr := d.promptDownstream(ctx, tgt, req.Prompt)
	captured := tgt.bridge.finishCapture()

	userText, _ := libacp.FlattenContent(req.Prompt)
	t.persistExternalTurn(ctx, sess.InternalSessionID, userText, captured)

	if promptErr != nil {
		// A genuine user cancellation resolves as stopReason "cancelled" with no
		// JSON-RPC error, per the ACP contract.
		if errors.Is(promptErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			reportChange(string(req.SessionID), map[string]any{"stop_reason": string(libacp.StopReasonCancelled)})
			return libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, nil
		}
		reportErr(promptErr)
		return libacp.PromptResponse{}, libacp.InternalError(promptErr.Error())
	}

	// Push the post-turn session_info_update, mirroring the native path, so
	// the client's sidebar label updates without a re-list.
	libacp.AfterResponse(ctx, func() {
		update := libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateSessionInfo,
			UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
		}
		if title := t.sessionInfoTitle(ctx, sess.InternalSessionID); title != "" {
			update.Title = title
		}
		t.sendUpdate(ctx, libacp.SessionNotification{SessionID: req.SessionID, Update: update})
	})

	reportChange(string(req.SessionID), map[string]any{"stop_reason": string(stopReason)})
	return libacp.PromptResponse{StopReason: stopReason}, nil
}

// externalTurnCapture records a downstream agent's turn as an ordered stream
// of display segments (prose, reasoning, tool cards) so the whole turn can be
// persisted and replayed faithfully, not collapsed to its final text. It is
// display-only: an external session's history is never fed to a model (the
// downstream agent keeps its own context). Guarded by externalBridge.mu.
type externalTurnCapture struct {
	segments  []externalCaptureSegment
	toolIndex map[string]int // toolCallId -> index in segments, for cross-frame merge
}

// externalCaptureSegment is one ordered piece of a captured turn: a run of
// assistant text, a run of assistant reasoning, or one tool call.
type externalCaptureSegment struct {
	kind string              // "text", "thinking", or "tool"
	text string              // for "text"/"thinking"
	tool *externalToolRecord // for "tool"
}

// externalToolRecord is the merged, display-complete state of one downstream
// tool call, accumulated across its tool_call/tool_call_update frames.
// Persisted as a "tool"-role message's JSON Content and re-emitted verbatim on
// replay (see externalToolReplayUpdate), preserving fields the native
// ToolCall model can't carry.
type externalToolRecord struct {
	ToolCallID  string                    `json:"toolCallId"`
	Title       string                    `json:"title,omitempty"`
	Kind        libacp.ToolKind           `json:"kind,omitempty"`
	Status      libacp.ToolCallStatus     `json:"status,omitempty"`
	RawInput    json.RawMessage           `json:"rawInput,omitempty"`
	RawOutput   json.RawMessage           `json:"rawOutput,omitempty"`
	ToolContent []libacp.ToolCallContent  `json:"toolContent,omitempty"`
	Locations   []libacp.ToolCallLocation `json:"locations,omitempty"`
}

func (c *externalTurnCapture) addText(s string) {
	if s == "" {
		return
	}
	if n := len(c.segments); n > 0 && c.segments[n-1].kind == "text" {
		c.segments[n-1].text += s
		return
	}
	c.segments = append(c.segments, externalCaptureSegment{kind: "text", text: s})
}

func (c *externalTurnCapture) addThinking(s string) {
	if s == "" {
		return
	}
	if n := len(c.segments); n > 0 && c.segments[n-1].kind == "thinking" {
		c.segments[n-1].text += s
		return
	}
	c.segments = append(c.segments, externalCaptureSegment{kind: "thinking", text: s})
}

func (c *externalTurnCapture) addToolUpdate(u libacp.SessionUpdate) {
	if u.ToolCallID == "" {
		return
	}
	if c.toolIndex == nil {
		c.toolIndex = make(map[string]int)
	}
	idx, ok := c.toolIndex[u.ToolCallID]
	if !ok {
		c.segments = append(c.segments, externalCaptureSegment{kind: "tool", tool: &externalToolRecord{ToolCallID: u.ToolCallID}})
		idx = len(c.segments) - 1
		c.toolIndex[u.ToolCallID] = idx
	}
	rec := c.segments[idx].tool
	// Later frames win per field (last non-empty), mirroring the client
	// reducer's merge so the persisted record matches the rendered card.
	if u.Title != "" {
		rec.Title = u.Title
	}
	if u.Kind != "" {
		rec.Kind = u.Kind
	}
	if u.Status != "" {
		rec.Status = u.Status
	}
	if len(u.RawInput) > 0 {
		rec.RawInput = u.RawInput
	}
	if len(u.RawOutput) > 0 {
		rec.RawOutput = u.RawOutput
	}
	if len(u.ToolContent) > 0 {
		rec.ToolContent = u.ToolContent
	}
	if len(u.Locations) > 0 {
		rec.Locations = u.Locations
	}
}

// externalTurnMessages converts a captured turn into the ordered []Message the
// message store persists. Consecutive text/reasoning coalesces into one
// assistant message, flushed at every tool boundary so the prose/tool-card
// interleaving survives replay. Monotonic timestamps preserve order through
// the store's added_at sort.
func externalTurnMessages(userText string, segments []externalCaptureSegment, base time.Time) []taskengine.Message {
	var msgs []taskengine.Message
	seq := 0
	stamp := func() time.Time {
		ts := base.Add(time.Duration(seq) * time.Millisecond)
		seq++
		return ts
	}
	if strings.TrimSpace(userText) != "" {
		msgs = append(msgs, taskengine.Message{ID: uuid.NewString(), Role: "user", Content: userText, Timestamp: stamp()})
	}

	var pendingText, pendingThinking strings.Builder
	flushAssistant := func() {
		if pendingText.Len() == 0 && pendingThinking.Len() == 0 {
			return
		}
		msgs = append(msgs, taskengine.Message{
			ID:        uuid.NewString(),
			Role:      "assistant",
			Content:   pendingText.String(),
			Thinking:  pendingThinking.String(),
			Timestamp: stamp(),
		})
		pendingText.Reset()
		pendingThinking.Reset()
	}

	for _, seg := range segments {
		switch seg.kind {
		case "text":
			pendingText.WriteString(seg.text)
		case "thinking":
			pendingThinking.WriteString(seg.text)
		case "tool":
			if seg.tool == nil {
				continue
			}
			flushAssistant()
			payload, err := json.Marshal(seg.tool)
			if err != nil {
				continue
			}
			msgs = append(msgs, taskengine.Message{
				ID:         uuid.NewString(),
				Role:       "tool",
				ToolCallID: seg.tool.ToolCallID,
				Content:    string(payload),
				Timestamp:  stamp(),
			})
		}
	}
	flushAssistant()
	return msgs
}

// persistExternalTurn records a downstream agent's turn — user prompt plus
// captured transcript — into the same message store the native path uses, so
// session/list titles and session/load replay work for external sessions too.
// Fresh message IDs make PersistDiff's dedupe append them. Uses a
// cancellation-immune context so a cancelled turn still records what was said.
func (t *Transport) persistExternalTurn(ctx context.Context, internalSessionID, userText string, segments []externalCaptureSegment) {
	if t.deps.DB == nil || internalSessionID == "" {
		return
	}
	msgs := externalTurnMessages(userText, segments, time.Now().UTC())
	if len(msgs) == 0 {
		return
	}
	cleanCtx := context.WithoutCancel(ctx)
	mgr := chatservice.NewManager(t.workspaceID())
	if err := mgr.PersistDiff(cleanCtx, t.deps.DB.WithoutTransaction(), internalSessionID, msgs); err != nil {
		reportErr, _, end := t.tracker().Start(cleanCtx, "persist", "acp_external_history", "session_id", internalSessionID)
		reportErr(err)
		end()
	}
}

// markExternalIfPersisted swaps a rebuilt session entry (session/load or
// session/resume) onto an external driver when a persisted agent name
// exists, so the next prompt routes to the downstream agent instead of the
// native chain. This, with NewSession's `_meta` check, is the sole place the
// native-vs-external driver is chosen.
func (t *Transport) markExternalIfPersisted(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID, entry *sessionEntry) {
	name := t.readSessionAgent(ctx, store, sid)
	if name == "" {
		return
	}
	ed := &externalDriver{t: t, agentName: name, upstreamID: sid}
	// On the Manager path, recover the persisted instanceID and downstream id so
	// the first prompt re-attaches (ensureAttached) instead of a fresh bring-up.
	// Absent/stale falls back to a fresh bring-up.
	if t.deps.Instances != nil {
		ed.instanceID = t.readSessionInstance(ctx, store, sid)
		ed.downstreamID = t.readSessionDownstream(ctx, store, sid)
	}
	entry.driver = ed
	// Restore the persisted per-session HITL policy so the reopened toolbar
	// shows the previously-chosen value; unlike a native session (in-memory
	// only), an external one persists it.
	if policy := t.readSessionHITLPolicy(ctx, store, sid); policy != "" {
		entry.setHITLPolicy(policy)
	}
}

// reloadedConfigOptions returns the config options to advertise on a
// session/load or session/resume response. A native session dispatches to its
// driver. An external session's downstream is not respawned during load, so
// its surface comes from the set persisted at session/new (and later
// updates), with contenox's own HITL policy select appended after it — its
// CurrentValue restored from the persisted selection by markExternalIfPersisted.
func (t *Transport) reloadedConfigOptions(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID, entry *sessionEntry) []libacp.SessionConfigOption {
	if _, ok := entry.driver.(*externalDriver); ok {
		downstream := t.readSessionAgentConfigOptions(ctx, store, sid)
		return append(downstream, t.hitlPolicyConfigOption(entry))
	}
	return t.sessionConfigOptions(ctx, entry)
}

// reemitExternalCommandMenu schedules the persisted downstream slash-command
// menu to relay strictly after the load/resume result is on the wire (via
// libacp.AfterResponse, mirroring sendAvailableCommands' ordering contract),
// so a reopened session shows its menu without a first prompt to respawn the
// downstream. A no-op when the session persisted no menu.
func (t *Transport) reemitExternalCommandMenu(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID) {
	cmds := t.readSessionAgentCommands(ctx, store, sid)
	if len(cmds) == 0 {
		return
	}
	libacp.AfterResponse(ctx, func() {
		t.relayExternalUpdate(ctx, sid, libacp.SessionUpdate{
			SessionUpdate:     libacp.SessionUpdateAvailableCommands,
			AvailableCommands: cmds,
		})
	})
}
