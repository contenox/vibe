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

	"github.com/contenox/beam/internal/kernel/agentinstance"
	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/agenthost"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/chatservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/internal/version"
	libacp "github.com/contenox/beam/libacp"
)

// AgentMetaKey is the session/new (and session/list) `_meta` key a client uses
// to bind a session to a REGISTERED external ACP agent instead of the native
// task-chain engine: `{"contenox.agent": "<registered agent name>"}`. Absent =
// native chain path, byte-for-byte the historical behavior. It is a contenox
// extension living in the spec's reserved `_meta` namespace (the same precedent
// as WorkspaceConfigOptionsMetaKey); conformant clients that don't recognize it
// ignore `_meta` entirely.
const AgentMetaKey = "contenox.agent"

// AgentModeConfigOptionID is the reserved SessionConfigOption id under which an
// external session surfaces the DOWNSTREAM agent's session Modes (its
// SessionModeState) as a single synthetic "select" picker in the upstream
// client's toolbar. The ACP spec models session modes (session/set_mode +
// SessionModeState) and config options (session/set_config_option +
// SessionConfigOption) as two distinct surfaces; contenox does not expose a
// first-class mode toggle to its clients, so a downstream agent that advertises
// modes only (claude-code-acp: default/acceptEdits/plan/dontAsk/bypassPermissions,
// zero configOptions) would otherwise render an empty toolbar. Mapping the modes
// onto one synthetic config option — id AgentModeConfigOptionID, label "Mode",
// type "select", each availableMode as a value(id)→label(name), currentValue the
// currentModeId — lets the existing config-option picker render it; a set on this
// id is translated back to session/set_mode (see externalDriver.SetConfigOption),
// and a downstream current_mode_update is relayed as a config_option_update over
// this same id. It lives in contenox's reserved dotted namespace so it never
// collides with a downstream agent's own option ids.
const AgentModeConfigOptionID = "contenox.agent-mode"

// syntheticModeOption maps a downstream SessionModeState onto the single synthetic
// "Mode" select the upstream toolbar renders (see AgentModeConfigOptionID). Returns
// ok=false when there are no modes to surface (nil state or empty availableModes),
// so an agent that advertises no modes yields no synthetic option.
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

// AgentModelConfigOptionID is the reserved SessionConfigOption id under which an
// external session surfaces the DOWNSTREAM agent's UNSTABLE model-picker state (its
// SessionModelState) as a single synthetic "select" picker in the upstream client's
// toolbar — the exact parallel of AgentModeConfigOptionID for session modes. Zed's
// claude-code-acp advertises a `models` state (availableModels + currentModelId) in
// its session/new response and switches models via the unstable `session/set_model`
// method (`unstable_setSessionModel`), a surface distinct from both session modes and
// config options; contenox does not expose a first-class model toggle to its clients,
// so mapping that state onto one synthetic config option — id AgentModelConfigOptionID,
// label "Model", type "select", each availableModel as a value(modelId)→label(name),
// currentValue the currentModelId — lets the existing config-option picker render it.
// A set on this id is translated back to session/set_model (see
// externalDriver.SetConfigOption). Unlike modes, the ACP session/update stream carries
// NO model-update kind, so there is nothing to relay after a switch: the (stateless)
// set_model response is the truth and the confirmed model is adopted locally. The
// model entries carry no effort/fast-mode facet (claude-code-acp's availableModels are
// modelId + name + description only), so this select has no sub-option for reasoning
// effort. It lives in contenox's reserved dotted namespace so it never collides with a
// downstream agent's own option ids, and it is placed after the synthetic mode option
// and before the downstream's own config options.
const AgentModelConfigOptionID = "contenox.agent-model"

// syntheticModelOption maps a downstream SessionModelState onto the single synthetic
// "Model" select the upstream toolbar renders (see AgentModelConfigOptionID). Returns
// ok=false when there are no models to surface (nil state or empty availableModels),
// so an agent that advertises no model picker yields no synthetic option — mirroring
// syntheticModeOption.
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
// for it to exit on stdin-close before killing it. Persistent agents (most
// editor adapters) never exit on stdin-close, so a short grace keeps CloseSession
// / connection teardown from stalling on the default (see agenthost.KillGrace).
const externalKillGrace = 2 * time.Second

// parseAgentMeta extracts the AgentMetaKey value from a request `_meta`. Missing
// key, malformed json, or a non-string value all read as "" (no external agent),
// so a client that ships unrelated `_meta` still lands on the native path.
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
// name, stored alongside the cwd KV so session/list attribution and the external
// prompt path survive reconnects (the in-memory session map is empty after a
// process restart or on a fresh connection).
type sessionAgentRecord struct {
	Agent string `json:"agent"`
}

const acpSessionAgentKVPrefix = "acp:session_agent:"

// acpSessionAgentCommandsKVPrefix and acpSessionAgentConfigOptionsKVPrefix store an
// external session's downstream-advertised surface (its slash-command menu and its
// config-option pickers) durably alongside the agent-name key. A session/load or
// session/resume does NOT drive the downstream during load (that happens lazily on
// the next prompt — see ensureAttached, which re-attaches to a surviving Manager
// instance or freshly brings one up), so the pre-load connection's live bridge cache
// is not consulted; these keys let a reopened external session restore its menu and
// pickers immediately, without waiting for the first prompt. The
// synthetic mode select (mapped from the downstream's session Modes) and the synthetic
// model select (mapped from its UNSTABLE model-picker state) are both folded into the
// persisted config-option set — no separate keys — so a reopened session's toolbar
// keeps its mode and model pickers too. Fresh live values overwrite them on every
// (re)spawn and on each config-option / mode / model change — live truth wins. Both are
// deleted with the agent-name key on session delete.
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

// acpSessionInstanceKVPrefix stores the Manager-owned instance id backing an external
// session, keyed by the upstream session id, so a session/load (even in a fresh
// process or on a new connection) can RE-ATTACH to the same still-running instance
// (agentinstance.Attach) instead of bringing up a fresh one. Only the Instances path
// writes it; it is deleted with the agent-name key on session delete. A stale id
// (Manager restarted / instance stopped) is harmless: Attach fails and the session
// falls back to a fresh bring-up, overwriting the key with the new id.
const acpSessionInstanceKVPrefix = "acp:session_instance:"

type sessionInstanceRecord struct {
	InstanceID string `json:"instanceId"`
}

// persistSessionInstance durably records the instance id backing an external session.
// Best-effort, mirroring persistSessionAgent; builds its own store so the driver
// (which holds none) can call it.
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

// readSessionInstance returns the persisted instance id for an external session, or
// "" when none was stored or on a read failure.
func (t *Transport) readSessionInstance(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID) string {
	var rec sessionInstanceRecord
	if err := store.GetKV(ctx, acpSessionInstanceKVPrefix+string(sid), &rec); err != nil {
		return ""
	}
	return rec.InstanceID
}

// acpSessionDownstreamKVPrefix stores the DOWNSTREAM agent's own session id (minted at
// the downstream session/new) keyed by the upstream session id. Under the R1 kernel the
// externalBridge is a per-attachment VIEWER, not the instance's surviving harness — a
// reconnecting Transport builds a FRESH bridge and no longer inherits the downstream id
// from a surviving one. So it is persisted alongside the instance id: a session/load
// that re-attaches to a still-running instance recovers the downstream session id from
// here and drives prompts against the SAME downstream session (preserving the agent's
// context), instead of a fresh session/new. Only the Instances path writes it; a stale
// value is harmless (the re-attach falls back to a fresh bring-up, overwriting it). It
// is deleted with the agent-name key on session delete.
const acpSessionDownstreamKVPrefix = "acp:session_downstream:"

type sessionDownstreamRecord struct {
	DownstreamID string `json:"downstreamId"`
}

// persistSessionDownstream durably records the downstream session id backing an external
// session. Best-effort, mirroring persistSessionInstance.
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

// readSessionDownstream returns the persisted downstream session id for an external
// session, or "" when none was stored or on a read failure.
func (t *Transport) readSessionDownstream(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID) libacp.SessionID {
	var rec sessionDownstreamRecord
	if err := store.GetKV(ctx, acpSessionDownstreamKVPrefix+string(sid), &rec); err != nil {
		return ""
	}
	return libacp.SessionID(rec.DownstreamID)
}

// persistSessionAgentCommands durably records an external session's downstream
// slash-command menu (full-replacement per spec) keyed by the upstream session id,
// so a later session/load can re-emit it before the downstream is respawned.
// Best-effort, mirroring persistSessionAgent; builds its own store so the bridge
// (which holds no store) can call it.
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

// persistSessionAgentConfigOptions durably records an external session's downstream
// config-option set (full-replacement per spec) keyed by the upstream session id, so
// a later session/load can carry them in its response before the downstream is
// respawned. Best-effort; builds its own store for the same reason as above.
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

// readSessionAgentCommands returns the persisted downstream slash-command menu for
// an external session, or nil when none was stored or on a read failure.
func (t *Transport) readSessionAgentCommands(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID) []libacp.AvailableCommand {
	var cmds []libacp.AvailableCommand
	if err := store.GetKV(ctx, acpSessionAgentCommandsKVPrefix+string(sid), &cmds); err != nil {
		return nil
	}
	return cmds
}

// readSessionAgentConfigOptions returns the persisted downstream config-option set
// for an external session, or nil when none was stored or on a read failure.
func (t *Transport) readSessionAgentConfigOptions(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID) []libacp.SessionConfigOption {
	var opts []libacp.SessionConfigOption
	if err := store.GetKV(ctx, acpSessionAgentConfigOptionsKVPrefix+string(sid), &opts); err != nil {
		return nil
	}
	return opts
}

// acpSessionHITLPolicyKVPrefix stores an external session's contenox-NATIVE HITL
// policy selection (the per-session approval policy the runtime enforces for its
// runtime-mediated actions — the terminal bridge, future fs — and that drives
// beam's file-explorer HITL labels) durably alongside the agent-name key. The
// native chain path keeps this selection only in-memory on the sessionEntry
// (reset to the sentinel default on every rebuild); an external session persists
// it because — like its downstream config-option surface — its toolbar is
// restored from persistence on a session/load, before any prompt respawns the
// downstream. Deleted with the agent-name key on session delete.
const acpSessionHITLPolicyKVPrefix = "acp:session_hitl_policy:"

type sessionHITLPolicyRecord struct {
	Policy string `json:"policy"`
}

// persistSessionHITLPolicy durably records an external session's contenox-native
// HITL policy selection keyed by the upstream session id. Best-effort, mirroring
// persistSessionAgentConfigOptions; builds its own store so a driver holding no
// store can call it.
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

// readSessionHITLPolicy returns the persisted contenox-native HITL policy selection
// for an external session, or "" when none was stored or on a read failure.
func (t *Transport) readSessionHITLPolicy(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID) string {
	var rec sessionHITLPolicyRecord
	if err := store.GetKV(ctx, acpSessionHITLPolicyKVPrefix+string(sid), &rec); err != nil {
		return ""
	}
	return rec.Policy
}

// externalBridge bridges one external-agent-backed session's DOWNSTREAM stream to
// the connected upstream client. It wears two hats depending on how the downstream
// is owned:
//
//   - connCtx path (Deps.Instances nil, stdio `contenox acp`): the bridge IS the
//     libacp.Client wired directly into the connCtx-bound subprocess. The downstream
//     calls SessionUpdate / RequestPermission / terminal/* on it in person.
//   - Instances path (Deps.Instances set, serve): the bridge is an
//     agentinstance.Viewer ATTACHED to a Manager-owned instance's session. The
//     instance owns the real libacp.Client (its journaling harness); it journals +
//     fans out every session/update to the bridge's Deliver and routes
//     session/request_permission to the controller viewer's RequestPermission. The
//     bridge is per-attachment (ID() is a unique viewer id) — a reconnecting
//     Transport builds a FRESH bridge and Attaches it, rather than inheriting a
//     surviving one.
//
// Either way it relays the downstream session/update stream up to the upstream client
// (remapping only the session id to the upstream ACP session, passing the Update
// through as-is) and forwards session/request_permission up (beam answers it). The
// terminal/* client-callback family (external_terminal.go) is serviced only on the
// connCtx path, where the bridge is the wired client; on the Instances path the
// instance's harness answers terminal/* with MethodNotFound (see initExternalConn,
// which withholds the terminal capability there). fs/* is refused with MethodNotFound
// via the embedded UnimplementedClient on both paths.
type externalBridge struct {
	libacp.UnimplementedClient

	// upstreamID is the STABLE upstream ACP session id this bridge serves. It is the
	// durable session identity (minted at session/new, re-supplied verbatim by the
	// client on every session/load), so it never changes across reconnects — only
	// the attached Transport does. Set once at construction.
	upstreamID libacp.SessionID

	// viewerID is this bridge's per-attachment agentinstance.Viewer id (ID()). It is
	// unique per bridge (a fresh uuid), so each Transport connection's bridge is a
	// distinct viewer of the instance's session and Detach names exactly it. Immutable.
	viewerID string

	// relayMu guards relayT (the CURRENTLY-attached upstream Transport this bridge
	// relays to), relaySuppressed (the re-attach backlog gate), and the Manager
	// viewer identity (mgr + instanceID) used for self-detach. For the connCtx path
	// relayT is set once and self-clears when that one connection ends. For the
	// Instances path the bridge is 1:1 with its Transport connection; relayT clears —
	// and, when it is a viewer, the bridge Detaches itself from the instance — when
	// that connection ends (a bare WebSocket drop fires connCtx and never calls
	// Transport.Close, so attach() arms a connCtx watcher for both). While relayT is
	// nil the downstream has no upstream to reach, so relays are dropped.
	relayMu sync.Mutex
	relayT  *Transport
	// relaySuppressed drops the upstream relay while set. It gates the journal REPLAY
	// on a re-attach: when a reconnecting bridge attaches to a still-running instance,
	// Attach synchronously replays that session's journal backlog — events the durable
	// chatservice transcript ALREADY replayed at session/load (the source of truth).
	// Suppressing the relay during that replay prevents double-emitting the pre-drop
	// turn; resumeRelay (called right after Attach, before any prompt) re-enables live
	// relay. A fresh bring-up never sets it — its instance's journal is empty.
	relaySuppressed bool
	// relayHeld + relayQueue BUFFER the relay instead of dropping it, the inverse of
	// relaySuppressed. It exists for ADOPT (see adopt.go): an adopted session's journal
	// replay is its ONLY history — there is no durable transcript to re-emit it from —
	// but Attach replays synchronously, i.e. before the session/new response reaches the
	// client, and a client drops updates for a session id it has not learned yet (the
	// reason `bound` exists). So the replay is queued in arrival order and flushed by
	// releaseRelay once the response is on the wire. Live updates arriving during the
	// hold queue behind it, so nothing is lost or reordered. Bounded in practice by the
	// journal size plus whatever the downstream emits inside one session/new — the hold
	// is always released by the same handler that took it.
	relayHeld  bool
	relayQueue []libacp.SessionUpdate
	// mgr + instanceID name the Manager-owned instance this bridge is a viewer of
	// (Instances path); both "" / nil on the connCtx path. Set by bindInstance before
	// Attach so the connCtx watcher and driver.Close can self-detach the viewer.
	mgr        agentinstance.Manager
	instanceID string
	// detachOnce makes the viewer Detach idempotent across its two triggers: the
	// connCtx watcher (bare WS drop) and driver.Close (explicit close/teardown).
	detachOnce sync.Once

	// mu guards capture, bound, cachedCommands, and downstreamID. capture is the
	// per-turn accumulator of the downstream agent's transcript — its
	// agent_message_chunk text, agent_thought_chunk reasoning, AND tool calls:
	// externalDriver.Prompt sets a fresh capture before the downstream
	// session/prompt and reads it back after, so the whole turn (not just the
	// final text) can be persisted for session/load replay. SessionUpdate runs on
	// the downstream read-loop goroutine; externalDriver.Prompt runs on the
	// upstream request goroutine.
	mu      sync.Mutex
	capture *externalTurnCapture

	// downstreamID is the downstream agent's own session id (from the downstream
	// session/new). It is held HERE — on the Manager-owned, Transport-independent
	// bridge — rather than only on the per-connection externalDriver, so a
	// reconnecting Transport that re-attaches via agentinstance.Attach recovers the
	// live downstream session id and drives the SAME session, without a fresh
	// session/new. Empty on the connCtx-spawn path until the handshake completes.
	downstreamID libacp.SessionID

	// bound reports whether the upstream client can resolve upstreamID yet — i.e.
	// whether the session/new response carrying this session id has reached it. A
	// downstream available_commands_update relayed BEFORE that point references a
	// session the client has not learned and is silently dropped (see beam's
	// handleNotification), which is exactly why the downstream agent's slash menu
	// never rendered. While unbound the menu is cached, not relayed; markBound (run
	// via libacp.AfterResponse from the external session/new handler) flips this and
	// flushes the cached menu, mirroring sendAvailableCommands' ordering contract. A
	// bridge spawned lazily after a session/load starts bound: its upstream session
	// already exists, so live relay reaches the client directly.
	bound bool
	// cachedCommands is the latest downstream available_commands_update payload
	// (full-replacement semantics per spec). Kept so the menu can be (re)emitted the
	// moment the upstream session becomes resolvable.
	cachedCommands []libacp.AvailableCommand

	// configOptions is the downstream agent's own advertised config-option set
	// (full-replacement per spec), seeded from the downstream session/new response
	// and replaced by each downstream config_option_update (and each confirmed
	// session/set_config_option). externalDriver.ConfigOptions returns it verbatim,
	// so the upstream client renders the downstream agent's real pickers.
	configOptions []libacp.SessionConfigOption
	// configReceived records that a LIVE downstream config-option payload (a
	// config_option_update, or a session/set_config_option confirmation) has
	// superseded the session/new seed, so a late seed write never clobbers it.
	configReceived bool
	// configOptionsPending mirrors cachedCommands' gating for config_option_update:
	// a downstream update arriving BEFORE the upstream session/new response is on the
	// wire references a session the client cannot resolve yet, so it is cached and
	// re-emitted by markBound instead of relayed live and dropped. It also gates a
	// mode change: a current_mode_update is surfaced as a config_option_update over
	// the synthetic mode id, so the same pre-bind hold applies.
	configOptionsPending bool

	// modeState is the downstream agent's session Modes (SessionModeState), seeded
	// from the downstream session/new response and kept current by each downstream
	// current_mode_update and each confirmed session/set_mode. It is folded into the
	// driver's config-option output as the synthetic AgentModeConfigOptionID select
	// (mode first, ahead of the downstream's own configOptions). nil when the
	// downstream advertises no modes (e.g. a bare agent) — no synthetic option then.
	modeState *libacp.SessionModeState
	// modeReceived records that a LIVE mode change (a current_mode_update, or a
	// confirmed session/set_mode) has landed, so a late session/new seed cannot
	// clobber the current mode — mirroring configReceived for the config options.
	modeReceived bool
	// pendingModeID holds a currentModeId from a current_mode_update that raced
	// AHEAD of the session/new seed (so modeState — which carries availableModes — is
	// not built yet); seedModes applies it once the availableModes arrive.
	pendingModeID string

	// modelState is the downstream agent's UNSTABLE model-picker state
	// (SessionModelState), seeded from the downstream session/new response and updated
	// only by a confirmed session/set_model (applyModel adopts the requested model into
	// its currentModelId). It is folded into the driver's config-option output as the
	// synthetic AgentModelConfigOptionID select, placed AFTER the mode option and before
	// the downstream's own configOptions. nil when the downstream advertises no models
	// (e.g. a bare agent, or a modes-only agent) — no synthetic model option then.
	// Unlike modeState there is no *received/*pending race machinery: the ACP
	// session/update stream carries no model-update kind, so nothing can race the seed —
	// the only mutation is applyModel, which happens strictly after the seed (a set
	// requires a spawned bridge, which the seed already populated).
	modelState *libacp.SessionModelState

	// termMu guards terminals, the live set of downstream-created terminals for
	// this session keyed by the bridge-minted terminal id. It is independent of mu
	// (which guards the update-relay caches) so terminal lifecycle never contends
	// with the session/update stream. Each bridgeTerminal owns a shell-session
	// scrollback watcher and is torn down on terminal/release, terminal/kill, or
	// connection/session teardown (closeAllTerminals). See external_terminal.go.
	termMu    sync.Mutex
	terminals map[string]*bridgeTerminal
}

// newExternalBridge builds a bridge for upstreamID relaying to t, watching t's
// connection so it self-detaches (relay AND, when it is a viewer, the Manager
// attachment) when that connection ends. bound seeds the live-relay readiness (see
// the bound field). Each bridge gets a fresh per-attachment viewer id.
func newExternalBridge(t *Transport, upstreamID libacp.SessionID, bound bool) *externalBridge {
	b := &externalBridge{upstreamID: upstreamID, bound: bound, viewerID: "acp-bridge-" + uuid.NewString()}
	b.attach(t)
	return b
}

// ID is the agentinstance.Viewer id: this bridge's per-attachment identity, the key
// the instance's viewer hub registers it under and Detach names.
func (b *externalBridge) ID() string { return b.viewerID }

// bindInstance records the Manager instance this bridge is a viewer of, so the
// connCtx watcher and driver.Close can self-detach it. Called before Attach on the
// Instances path; never called on the connCtx path (mgr stays nil, detach is a no-op).
func (b *externalBridge) bindInstance(mgr agentinstance.Manager, instanceID string) {
	b.relayMu.Lock()
	b.mgr = mgr
	b.instanceID = instanceID
	b.relayMu.Unlock()
}

// suppressReplay silences the upstream relay so the journal backlog Attach is about to
// replay is dropped (the chatservice transcript already replayed it at session/load).
// Paired with resumeRelay, called right after Attach returns.
func (b *externalBridge) suppressReplay() {
	b.relayMu.Lock()
	b.relaySuppressed = true
	b.relayMu.Unlock()
}

// resumeRelay re-enables the upstream relay after a suppressed re-attach replay, so
// subsequent LIVE downstream updates reach the reconnected client.
func (b *externalBridge) resumeRelay() {
	b.relayMu.Lock()
	b.relaySuppressed = false
	b.relayMu.Unlock()
}

// holdRelay queues upstream relays instead of sending them, so an Attach that replays a
// journal BEFORE this connection's session/new response is on the wire loses nothing (see
// relayHeld). Paired with releaseRelay, which MUST be called — from libacp.AfterResponse —
// or the queued backlog never reaches the client.
func (b *externalBridge) holdRelay() {
	b.relayMu.Lock()
	b.relayHeld = true
	b.relayMu.Unlock()
}

// releaseRelay flushes the held backlog in arrival order and returns the bridge to live
// relay. It drains under repeated locking rather than clearing the flag first, so an
// update produced concurrently with the flush either joins the tail of the queue (still
// held) or relays live strictly AFTER it — the held and live streams can never interleave
// out of order. A no-op when nothing was held.
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
// when t's connection ends. Used at construction. Detach is wired to connCtx
// cancellation, not Transport.Close, because a bare WebSocket drop fires connCtx and
// never calls Transport.Close (see transport.go). The watcher's detachFrom is
// pointer-identity guarded, so a stale connection ending never clears a newer target,
// and detachViewer removes the bridge from the instance's fan-out (idempotent).
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

// detachFrom clears the relay target iff it is still t, so a re-attach to a newer
// Transport is never clobbered by an older one's later teardown.
func (b *externalBridge) detachFrom(t *Transport) {
	b.relayMu.Lock()
	if b.relayT == t {
		b.relayT = nil
	}
	b.relayMu.Unlock()
}

// detachViewer removes this bridge from its Manager instance's session fan-out. A
// no-op on the connCtx path (no mgr) and idempotent across its triggers (connCtx
// watcher, driver.Close). It never STOPS the instance — a viewer leaving keeps the
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

// transport returns the currently-attached upstream Transport, or nil when the
// bridge is detached (no upstream to relay to — updates are dropped).
func (b *externalBridge) transport() *Transport {
	b.relayMu.Lock()
	defer b.relayMu.Unlock()
	return b.relayT
}

// relayUpstream forwards a downstream update to the currently-attached upstream
// client, remapping onto upstreamID. A no-op when detached (no upstream) or while the
// re-attach replay is suppressed (the backlog is the chatservice transcript's job); while
// the relay is HELD (adopt) it is queued instead, and releaseRelay sends it.
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
// instance's journaling harness calls it for each REPLAYED and LIVE session/update.
// It shares the relay/capture logic with SessionUpdate (the connCtx-path libacp.Client
// entry point) — both remap the downstream session id onto upstreamID and relay.
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
	// available_commands_update carries the downstream agent's slash-command menu
	// (full-replacement per spec). Cache the latest, and relay live only once the
	// upstream client can resolve this session — a menu relayed before the
	// session/new response is dropped as referencing an unknown session, so while
	// unbound it is held and re-emitted by markBound after the response.
	if n.Update.SessionUpdate == libacp.SessionUpdateAvailableCommands {
		b.mu.Lock()
		b.cachedCommands = n.Update.AvailableCommands
		relay := b.bound
		b.mu.Unlock()
		// Persist the menu (live truth) so a later session/load can re-emit it before
		// the downstream is respawned. Independent of bind state — the menu can be
		// stored the moment it arrives even if the upstream relay is still gated.
		b.persistCommands(ctx, n.Update.AvailableCommands)
		if relay {
			b.relayUpstream(ctx, n.Update)
		}
		return nil
	}
	// config_option_update carries the downstream agent's own config pickers
	// (full-replacement per spec). Cache the latest as the driver's live option set
	// and relay only once the upstream session is resolvable — same pre-bind gating
	// as the command menu (a pre-bind update is held and flushed by markBound).
	if n.Update.SessionUpdate == libacp.SessionUpdateConfigOption {
		b.mu.Lock()
		b.configReceived = true
		b.configOptions = n.Update.ConfigOptions
		relay := b.bound
		if !relay {
			b.configOptionsPending = true
		}
		b.mu.Unlock()
		// The advertised surface has exactly ONE owner per path (see
		// configOptionsSurface): the local fold above on the connCtx path, the kernel's
		// already-merged capture on the Instances path. Read outside b.mu — the
		// Instances read reaches into the Manager.
		merged := b.configOptionsSurface()
		// Persist the pickers (live truth, synthetic mode option folded in) so a later
		// session/load can carry them in its response before the downstream respawns.
		b.persistConfigOptions(ctx, merged)
		if relay {
			b.relayUpstream(ctx, libacp.SessionUpdate{
				SessionUpdate: libacp.SessionUpdateConfigOption,
				ConfigOptions: merged,
			})
		}
		return nil
	}
	// current_mode_update carries the downstream agent's newly-current session mode.
	// contenox surfaces the downstream's modes as the synthetic AgentModeConfigOptionID
	// config option (not a first-class client mode toggle), so this is TRANSLATED into a
	// config_option_update carrying the refreshed synthetic set — the raw
	// current_mode_update is not forwarded. Same pre-bind gating as the config pickers:
	// while unbound it is held and re-emitted by markBound after the session/new result.
	if n.Update.SessionUpdate == libacp.SessionUpdateCurrentMode {
		b.mu.Lock()
		b.modeReceived = true
		if b.modeState != nil {
			b.modeState.CurrentModeID = n.Update.CurrentModeID
		} else {
			// Raced ahead of the session/new seed; remember the new current for seedModes.
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
// session/update into the active per-turn capture (a no-op outside a turn). It
// captures assistant text, assistant reasoning, and tool calls — everything the
// upstream client rendered live — so a later session/load replay reconstructs
// the same transcript instead of collapsing the turn to its final text. Tool
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

// markBound records that the upstream client can now resolve this session (its
// session/new response is on the wire) and flushes the latest cached downstream
// available_commands_update. Scheduled via libacp.AfterResponse from the external
// session/new handler so the menu reaches the client strictly AFTER the result —
// the same ordering contract sendAvailableCommands documents for the native menu.
// Idempotent, and a no-op when the downstream has advertised no menu yet: a menu
// that arrives after this point relays live, since the session is now bound.
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
	// A config_option_update or current_mode_update that raced ahead of the
	// session/new response (cached pre-bind, unlike the seed which the response
	// already carried) is flushed now that the client can resolve this session — as a
	// config_option_update carrying the merged synthetic-mode-first set.
	if flushConfig {
		b.relayUpstream(ctx, libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateConfigOption,
			ConfigOptions: configOpts,
		})
	}
}

// seedConfigOptions records the downstream session/new response's advertised
// config options as the initial set, unless a live downstream update has already
// superseded the seed (the downstream read-loop can deliver a config_option_update
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

// seedModes records the downstream session/new response's SessionModeState as the
// initial mode set (copied, so later currentModeId mutations stay local). A
// current_mode_update that raced ahead of this seed left its new currentModeId in
// pendingModeID; it is folded in so the raced update is not lost. Never overwrites an
// already-established modeState.
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

// applyMode adopts an upstream-confirmed session mode (from a session/set_mode the
// driver forwarded downstream) into the synthetic option's currentValue. The
// set_mode response carries no state, so the requested modeId is authoritative; the
// downstream's own current_mode_update, if it also emits one, reconfirms it.
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

// seedModels records the downstream session/new response's SessionModelState as the
// initial model set (copied, so a later currentModelId mutation stays local). Never
// overwrites an already-established modelState. Mirrors seedModes, but without the
// pending/received race fold-in: the ACP stream carries no model-update kind, so no
// live update can race this seed.
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

// applyModel adopts an upstream-confirmed model (from a session/set_model the driver
// forwarded downstream) into the synthetic option's currentValue. The set_model
// response carries no state, and no model-update notification kind exists, so the
// requested modelId is authoritative. No-op when the downstream advertised no models
// (modelState nil) — a set could not have targeted the synthetic model id then.
func (b *externalBridge) applyModel(modelID string) {
	b.mu.Lock()
	if b.modelState != nil {
		b.modelState.CurrentModelID = modelID
	}
	b.mu.Unlock()
}

// buildConfigOptionsLocked assembles the driver's full upstream config-option set:
// the synthetic downstream-mode select FIRST (when the downstream advertises modes),
// then the synthetic downstream-model select (when it advertises a model picker), then
// the downstream agent's own config options. Caller holds b.mu.
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

// snapshotConfigOptions returns the driver's current upstream option set: the
// synthetic downstream-mode select first (when present), then the synthetic
// downstream-model select (when present), then the downstream agent's own options.
// It is the connCtx path's builder — on the Instances path the surface belongs to
// the kernel, so read configOptionsSurface instead of this.
func (b *externalBridge) snapshotConfigOptions() []libacp.SessionConfigOption {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buildConfigOptionsLocked()
}

// configOptionsSurface returns the DOWNSTREAM-derived config-option surface this
// session advertises upstream. It is the single read every advertisement path goes
// through, because the surface has exactly ONE owner and which one depends on the
// path:
//
//   - connCtx path (no Manager): the BRIDGE owns it. It seeds from the session/new
//     response and folds in its own synthetic mode/model selects — snapshotConfigOptions.
//   - Instances path (viewer of a Manager instance): the KERNEL owns it. OpenSession
//     returns only a session id; the session/new response's options/modes/models were
//     captured into the instance's per-session state, and Manager.SessionConfigOptions
//     already returns them WITH the synthetic mode + model selects folded in. Re-seeding
//     the bridge from that merged set would make the bridge synthesize a second time
//     (mode/model options duplicated on top of an already-merged set), so the bridge
//     simply does not maintain a surface there and defers.
//
// Live config_option_update / current_mode_update notifications keep BOTH owners
// current — the kernel captures them BEFORE fanning out to viewers, so a read taken
// right after the bridge observed an update already reflects it. An unknown/stopped
// instance yields nil (nothing to advertise), never the bridge's empty local state.
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

// persistCommands durably records the latest downstream command menu keyed by this
// bridge's upstream session id, so a later session/load restores it before a respawn.
func (b *externalBridge) persistCommands(ctx context.Context, cmds []libacp.AvailableCommand) {
	if t := b.transport(); t != nil {
		t.persistSessionAgentCommands(ctx, b.upstreamID, cmds)
	}
}

// persistConfigOptions durably records the latest downstream config-option set keyed
// by this bridge's upstream session id, so a later session/load restores the pickers
// before a respawn.
func (b *externalBridge) persistConfigOptions(ctx context.Context, opts []libacp.SessionConfigOption) {
	if t := b.transport(); t != nil {
		t.persistSessionAgentConfigOptions(ctx, b.upstreamID, opts)
	}
}

// RequestPermission forwards the downstream agent's permission request to the
// upstream client, remapping the session id. The upstream client (beam) already
// answers session/request_permission — this reuses the exact path AskApproval
// uses for the native engine, calling the upstream connection directly.
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
// connected upstream client, remapping only the session id. Unlike sendUpdate it
// applies no tool-call normalization: the downstream agent is a conformant ACP peer
// and owns its own tool-call framing. The one enrichment is on config_option_update:
// contenox's own per-session HITL policy select is appended so the relayed set stays
// consistent with the set_config_option response (see externalConfigOptionsForRelay).
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

// externalConfigOptionsForRelay appends contenox's OWN per-session HITL policy select
// to a downstream/merged config-option set bound for a relayed config_option_update.
// The upstream client applies a config_option_update as a WHOLESALE replacement of the
// session's options (beam's reducer), so a relay carrying only the downstream/synthetic
// surface would blank the HITL picker mid-session — and with it the file-explorer HITL
// labels — until the next set_config_option response restored it. Appending here keeps
// every relay path (downstream update, confirmed set, markBound flush, lazy-respawn
// push) consistent with that response. Falls back to the bare set when the session is
// not resolvable (it always is for a live relay).
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

// externalDriver drives a session against a REGISTERED downstream ACP agent
// instead of the native chain engine. The downstream connection is acquired lazily
// (session/new acquires eagerly; the first prompt after a session/load re-attaches
// or freshly brings one up) via ensureAttached, and released on Close.
//
// Two ownership modes:
//   - Manager-owned (Deps.Instances set): the connection belongs to an
//     agentinstance.Manager instance (instanceID names it) that OUTLIVES this
//     connection. handle is nil (the driver does not own the process); Close DETACHES
//     the bridge from this transport but leaves the instance Running for reconnect.
//   - connCtx-owned (Deps.Instances nil, today's behavior): the driver spawns and
//     OWNS the subprocess (handle) bound to the connection's connCtx; Close closes it.
type externalDriver struct {
	t         *Transport
	agentName string

	// upstreamID is the ACP session id this driver serves. Kept so a config change
	// routed through the NATIVE per-session path (contenox's own HITL policy select)
	// can persist the selection under the session's key even when no downstream is
	// spawned (a loaded session before its first prompt has a nil bridge). Set once
	// at construction, never mutated.
	upstreamID libacp.SessionID

	// mu guards the live downstream state: set when the session attaches and re-set
	// lazily on the first prompt after a session/load. The attached/not-attached
	// sentinel is BRIDGE (nil = not attached) — not conn, which is nil on the
	// Instances path by design.
	mu sync.Mutex
	// conn is the raw downstream connection — set ONLY on the connCtx-owned path,
	// where the driver owns the process. On the Instances path it stays nil: the
	// Manager owns the connection and every downstream operation is driven through
	// its session API (OpenSession/Prompt/Cancel/SetConfigOption), so this transport
	// holds no connection at all.
	conn *libacp.ClientSideConnection
	// handle is the connCtx-owned subprocess handle — set ONLY on the nil-Instances
	// path (the driver owns the process); nil when the connection is a Manager
	// instance's (the Manager owns it).
	handle *agenthost.Handle
	// instanceID names the Manager-owned instance backing this session — set ONLY on
	// the Instances path; "" on the connCtx-owned path. It is what a reconnect
	// re-attaches to (agentinstance.Attach) and what DeleteSession stops.
	instanceID   string
	downstreamID libacp.SessionID
	bridge       *externalBridge
}

// AgentName returns the registered downstream agent name, echoed in session/new's
// `_meta` and used for session/list attribution.
func (d *externalDriver) AgentName() string { return d.agentName }

// AvailableCommands returns nil: an external session bypasses contenox's slash
// commands and relays the downstream agent's own menu live.
func (d *externalDriver) AvailableCommands() []libacp.AvailableCommand { return nil }

// ConfigOptions returns the DOWNSTREAM agent's own advertised config options —
// prefixed by the synthetic "Mode" select mapped from the downstream's session
// Modes (see AgentModeConfigOptionID) and the synthetic "Model" select mapped from
// its UNSTABLE model-picker state (see AgentModelConfigOptionID) when it advertises
// them — captured from its session/new response and kept current by
// config_option_update / current_mode_update relays and confirmed set_config_option /
// set_mode / set_model calls, and SUFFIXED by contenox's OWN per-session HITL policy
// select. The chain-engine's own model/think/token selects stay suppressed (an external
// session does not drive the chain — the synthetic model select above is the DOWNSTREAM
// agent's picker, not the chain's), but the HITL policy
// is a real per-session capability: the runtime gates the foreign agent's
// runtime-mediated actions (terminal bridge, future fs) under it and it drives beam's
// file-explorer HITL labels, so it is appended after the downstream surface. The
// downstream part is absent before the downstream is spawned (a loaded session before
// its first prompt has a nil bridge) — the HITL select still shows, and the lazy
// respawn pushes a config_option_update to restore the downstream pickers.
func (d *externalDriver) ConfigOptions(_ context.Context, sess *sessionEntry) []libacp.SessionConfigOption {
	d.mu.Lock()
	bridge := d.bridge
	d.mu.Unlock()
	var base []libacp.SessionConfigOption
	if bridge != nil {
		base = bridge.configOptionsSurface()
	}
	// A fresh slice avoids aliasing the bridge's backing array (snapshotConfigOptions
	// may return it directly), so appending here never corrupts the bridge's set.
	out := make([]libacp.SessionConfigOption, 0, len(base)+1)
	out = append(out, base...)
	out = append(out, d.t.hitlPolicyConfigOption(sess))
	return out
}

// SetConfigOption forwards an upstream config-option change to the downstream
// agent's session/set_config_option and adopts the option set it confirms, so the
// upstream SetSessionConfigOption response (built from ConfigOptions) reflects the
// downstream's authoritative value. A downstream config_option_update, if the agent
// also emits one, relays through the bridge and updates the same cache. The value
// union (string/boolean) is forwarded intact so a boolean downstream option keeps
// its wire type. Contenox performs no upstream validation: the downstream owns its
// option semantics and rejects an unknown id/value with its own error.
func (d *externalDriver) SetConfigOption(ctx context.Context, sess *sessionEntry, configID string, value libacp.SessionConfigOptionValue) error {
	// contenox's OWN per-session HITL policy is enforced by the RUNTIME, not the
	// downstream agent: route it through the NATIVE per-session path (the exact
	// setSessionConfigOption the native driver uses — validate + store on the
	// session), never forwarding it downstream (the agent knows no such id), and
	// persist it so the selection survives a session/load that rebuilds the entry
	// with the sentinel default. This works before the downstream is (re)spawned (a
	// nil bridge after a load): the native path needs no downstream connection, so
	// the picker is settable on a loaded session too.
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
	// Instances path: the KERNEL performs this exact mapping (the synthetic mode/model
	// ids to session/set_mode / session/set_model, everything else to
	// session/set_config_option) and adopts the confirmed value into its per-session
	// state — the surface it owns here (see configOptionsSurface). So the three
	// branches below collapse to one call, and only the transport-side DB persistence
	// (the runtime's rows, not the kernel's) stays here.
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
	// The synthetic mode option is not a real downstream config option: a set on its
	// reserved id translates to the downstream's session/set_mode, and the confirmed
	// mode is adopted into the synthetic option's currentValue. Every other id forwards
	// to the downstream's session/set_config_option unchanged.
	if configID == AgentModeConfigOptionID {
		if _, err := conn.SetSessionMode(ctx, libacp.SetSessionModeRequest{
			SessionID: downstreamID,
			ModeID:    value.AsString(),
		}); err != nil {
			return err
		}
		bridge.applyMode(value.AsString())
		// Persist the merged set (synthetic mode first) so a session/load reflects the
		// new mode even before the next prompt respawns the downstream.
		bridge.persistConfigOptions(ctx, bridge.snapshotConfigOptions())
		return nil
	}
	// The synthetic model option is not a real downstream config option either: a set on
	// its reserved id translates to the downstream's UNSTABLE session/set_model, and the
	// confirmed model is adopted into the synthetic option's currentValue. The set_model
	// response is stateless and no model-update notification exists, so the requested id
	// is authoritative — nothing to relay, unlike the mode path's current_mode_update.
	if configID == AgentModelConfigOptionID {
		if _, err := conn.SetSessionModel(ctx, libacp.SetSessionModelRequest{
			SessionID: downstreamID,
			ModelID:   value.AsString(),
		}); err != nil {
			return err
		}
		bridge.applyModel(value.AsString())
		// Persist the merged set (synthetic mode + model folded in) so a session/load
		// reflects the new model even before the next prompt respawns the downstream.
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
	// Persist the merged set (synthetic mode option folded in alongside the confirmed
	// downstream options) so a session/load reflects the new value even before the
	// next prompt respawns the downstream.
	bridge.persistConfigOptions(ctx, bridge.snapshotConfigOptions())
	return nil
}

// Close releases this connection's hold on the downstream agent. Its meaning
// depends on ownership (see externalDriver):
//
//   - Manager-owned instance (instanceID set): DETACH only. The bridge is unbound
//     from this transport (it stops relaying to — and retaining — a connection that
//     is ending) but the instance is left RUNNING for reconnect. It is NOT stopped
//     here: a plain disconnect or session/close must leave the agent's process (and
//     context) alive; only DeleteSession / Manager.Close stop it.
//   - connCtx-owned subprocess (handle set, nil-Instances path): CLOSE the handle,
//     tearing down the subprocess the driver owns — today's behavior.
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
	// Tear down any live downstream-created terminals (their scrollback watchers)
	// before dropping the connection, so an explicit session/close or a stdio
	// Transport.Close leaks no watcher goroutines. The serve WebSocket path, which
	// never calls Transport.Close, is covered independently: each terminal's owner
	// goroutine also watches connCtx and self-terminates on connection teardown.
	if bridge != nil {
		bridge.closeAllTerminals()
	}
	// Manager-owned: detach the bridge VIEWER from the instance's session fan-out and
	// clear its relay target, but leave the instance RUNNING for reconnect. detachViewer
	// is idempotent with the connCtx watcher's detach; detachFrom is pointer-identity
	// guarded. Only DeleteSession / Manager.Close stop the instance.
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

// resolveExternalAgent resolves a registered agent by name and returns BOTH the
// declared record and its external_acp config, rejecting an unknown or disabled
// agent with a clear JSON-RPC error (the client's session/new fails cleanly). The
// registry service is constructed from the existing DB dep — declared external
// agents are a polymorphic resource over the same store the transport already
// holds.
//
// The record is returned, not just the config, so the Manager-owned branch of
// bringUpExternal can spawn from THIS read (Instances.StartResolved) instead of
// making the kernel re-read the same row: the Enabled check below and the spawn
// must be made against the same bytes, or an agent disabled in between still
// spawns.
func (t *Transport) resolveExternalAgent(ctx context.Context, name string) (*runtimetypes.Agent, *runtimetypes.ExternalACPConfig, error) {
	if t.deps.DB == nil {
		return nil, nil, libacp.InternalError("acpsvc: no database configured for external agents")
	}
	reg := agentregistryservice.New(t.deps.DB)
	// Refuse a disabled agent via the ONE shared judgment
	// agentregistryservice.ResolveForSpawn makes for every agent-spawn path
	// (see its doc comment): fleetservice.Dispatch calls the same helper, so
	// "disabled" cannot drift into two different checks or two different
	// messages between the REST dispatch path and this chat path. Called
	// here — before EITHER of bringUpExternal's two branches (Manager-owned
	// or connCtx-owned/stdio) — so a disabled agent is refused regardless of
	// which one this Transport is configured for.
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
	// KIND-POLYMORPHIC from here on. A CHAIN-kind agent has no external_acp
	// config to read — its config names a chain file, and the Manager builds the
	// spawn from it (agentinstance.StartResolved's chain branch) — so asking for
	// one would refuse a perfectly runnable agent with a kind-mismatch error.
	// That is exactly what happened: a discovered chain agent was selectable in
	// the picker and unusable in chat.
	//
	// The zero config is not a stand-in for a missing one: on the Manager path
	// it is read for exactly one thing, the mcp_servers allowlist
	// (resolveMcpAllowlist), and a chain unit declares none — it runs THIS
	// runtime's tools, configured by the chain file, not a foreign agent's
	// forwarded servers. An empty allowlist is therefore the truthful answer,
	// not a degraded one.
	//
	// The connCtx branch of bringUpExternal, which spawns FROM this config
	// directly, refuses chain kind in person rather than spawning something
	// wrong out of these zero bytes.
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
// external session: whichever ownership token applies — handle + conn (connCtx-owned
// subprocess, nil-Instances path) XOR instanceID (Manager-owned instance, which owns
// the connection itself, so conn stays nil here). teardown reverses exactly the one
// that was set.
type externalAttach struct {
	conn         *libacp.ClientSideConnection // set on the nil-Instances path only
	handle       *agenthost.Handle            // set on the nil-Instances path
	instanceID   string                       // set on the Instances path
	downstreamID libacp.SessionID
	bridge       *externalBridge
}

// teardown reverses a bring-up: closes the connCtx-owned subprocess, or stops the
// Manager-owned instance. Used when a caller decides not to keep this attach (a
// session/new that fails after the downstream came up, or a lost lazy-attach race).
func (ea *externalAttach) teardown(t *Transport) {
	if ea.handle != nil {
		_ = ea.handle.Close()
	}
	if ea.instanceID != "" && t.deps.Instances != nil {
		_ = t.deps.Instances.Stop(ea.instanceID)
	}
}

// resolveMcpAllowlist resolves a declared external agent's mcp_servers ALLOWLIST
// (names) against the store into the ACP session/new wire shapes to forward. It stays
// on the transport on BOTH paths: resolution needs the DB, which the kernel
// deliberately does not have — agentinstance.SessionSpec.McpServers takes an
// already-resolved set for exactly this reason.
func (t *Transport) resolveMcpAllowlist(ctx context.Context, cfg *runtimetypes.ExternalACPConfig, agentName string) ([]libacp.McpServer, error) {
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())
	servers, err := agenthost.ResolveForwardedMcpServers(ctx, storeMcpResolver{store: store}, cfg.McpServers)
	if err != nil {
		return nil, libacp.InternalError(fmt.Sprintf("acpsvc: resolve mcp allowlist for agent %q: %v", agentName, err))
	}
	return servers, nil
}

// openInstanceSession drives the downstream handshake for a MANAGER-OWNED instance —
// the Instances-path counterpart of initExternalConn. It holds no connection: the
// kernel owns the initialize-once negotiation, session/new, and the capture of the
// response's advertised surface (config options / modes / models), returning only the
// downstream session id. This transport keeps the two things that are genuinely its
// own: resolving the mcp allowlist against the DB (see resolveMcpAllowlist) and
// persisting the advertised option set into ITS rows so a session/load before the next
// prompt can restore the pickers.
//
// SessionSpec.Terminal is deliberately FALSE. Advertising the terminal client
// capability here would be a lie: the instance's own journaling harness is the wired
// libacp.Client and answers terminal/* with MethodNotFound — the bridge, a mere
// Viewer, is never called for it — so the downstream would route shell commands to a
// dead surface. Withholding it is a documented regression of this slice (the connCtx
// path, where the bridge IS the wired client, still serves terminals).
//
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
	// Persist the kernel's captured surface (synthetic mode + model selects folded in
	// by Manager.SessionConfigOptions) so a session/load before the first prompt can
	// restore the pickers. Every (re)open overwrites it with fresh downstream truth.
	bridge.persistConfigOptions(ctx, bridge.configOptionsSurface())
	return downstreamID, nil
}

// initExternalConn drives the downstream ACP handshake (initialize + session/new) on
// an already-connected downstream connection wired to bridge, seeding the bridge's
// advertised surface and recording the downstream session id ON the bridge (so a
// reconnect recovers it). It is the connCtx-spawn path ONLY — the driver owns the
// connection there; the Manager-instance path drives the same handshake through the
// kernel instead (openInstanceSession), holding no connection. On any failure the
// CALLER owns teardown of the connection (close the handle).
func (t *Transport) initExternalConn(ctx context.Context, conn *libacp.ClientSideConnection, bridge *externalBridge, cfg *runtimetypes.ExternalACPConfig, cwd, agentName string, terminalCapable bool) (libacp.SessionID, error) {
	mcpServers, err := t.resolveMcpAllowlist(ctx, cfg, agentName)
	if err != nil {
		return "", err
	}

	// Advertise the terminal client capability to the downstream agent ONLY when this
	// path can actually service terminal/* — i.e. a shell manager is present (the bridge
	// IS the wired libacp.Client here, so it answers the family in person). With it
	// advertised, a downstream agent (e.g. claude-code-acp) routes its shell commands
	// through the runtime's terminals (external_terminal.go) so beam's terminal panel
	// shows them live. The Instances path withholds it for the reason spelled out on
	// openInstanceSession.
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
	// Capture the downstream agent's own advertised config options AND its session
	// modes AND its UNSTABLE model-picker state so the upstream session/new response
	// carries them synchronously (no timing gap) and externalDriver.ConfigOptions can
	// surface the agent's real pickers plus the synthetic mode and model selects.
	// Seeding the config options / modes yields to any config_option_update /
	// current_mode_update the downstream read-loop already delivered; the model seed has
	// no such race (no model-update kind exists on the stream).
	bridge.seedConfigOptions(downstream.ConfigOptions)
	bridge.seedModes(downstream.Modes)
	bridge.seedModels(downstream.Models)
	bridge.setDownstreamID(downstream.SessionID)
	// Persist the current live option set (the seed — synthetic mode option folded in —
	// or a concurrently-received update; snapshot returns whichever is live) so a
	// session/load before the first prompt can restore the pickers. Every (re)spawn
	// overwrites the persisted value with fresh downstream truth.
	bridge.persistConfigOptions(ctx, bridge.snapshotConfigOptions())
	return downstream.SessionID, nil
}

// bringUpExternal establishes a FRESH downstream connection for agentName under a new
// bridge and drives its handshake. When Deps.Instances is wired the downstream is a
// Manager-owned instance (StartExternal) that SURVIVES this connection's teardown;
// otherwise it is a subprocess bound to this connection's connCtx (today's behavior).
// Every failure after connect tears the connection back down, so a rejected
// session/new never leaks a process or instance.
//
// bound seeds the bridge's readiness to relay the downstream slash-command menu live:
// false for the eager session/new bring-up (the upstream session/new response is not
// on the wire yet, so the menu is cached and re-emitted by markBound after it), true
// for a lazy bring-up on the first prompt after a session/load (the upstream session
// already exists, so a live relay reaches the client directly).
func (t *Transport) bringUpExternal(ctx context.Context, upstreamID libacp.SessionID, cwd, agentName string, bound bool) (*externalAttach, error) {
	agent, cfg, err := t.resolveExternalAgent(ctx, agentName)
	if err != nil {
		return nil, err
	}
	bridge := newExternalBridge(t, upstreamID, bound)

	if t.deps.Instances != nil {
		// Manager-owned: the subprocess is bound to the Manager's root ctx, so it
		// outlives this connection. The instance owns its own journaling harness (it
		// spawned wired to it); this driver DRIVES the downstream through the Manager's
		// session API and OBSERVES it by Attaching the bridge as a viewer — it holds NO
		// connection. Any failure Stops the instance so nothing leaks.
		//
		// StartResolved, not Start(agentName): the record resolveExternalAgent just
		// read is the one the Enabled check was made against, so spawning from it
		// closes the TOCTOU window Start's second read would open — and saves a query
		// per bring-up. (The connCtx branch below already spawns from this same read.)
		instanceID, err := t.deps.Instances.StartResolved(ctx, agent, cwd)
		if err != nil {
			return nil, libacp.InternalError(fmt.Sprintf("acpsvc: start agent %q instance: %v", agentName, err))
		}
		// Bind BEFORE the handshake: openInstanceSession persists the kernel-owned
		// config-option surface, which the bridge can only read once it knows which
		// instance it is a viewer of.
		bridge.bindInstance(t.deps.Instances, instanceID)
		downstreamID, err := t.openInstanceSession(ctx, instanceID, bridge, cfg, cwd, agentName)
		if err != nil {
			_ = t.deps.Instances.Stop(instanceID)
			return nil, err
		}
		// Attach the bridge as a VIEWER of the downstream session (keyed by the
		// downstream session id — the id the instance journals/fans out under). The
		// first viewer becomes the session's controller and thereby answers the
		// downstream's permission requests. A fresh instance's journal is empty, so the
		// replay is a no-op; no suppression is needed.
		if _, err := t.deps.Instances.Attach(ctx, instanceID, downstreamID, bridge); err != nil {
			_ = t.deps.Instances.Stop(instanceID)
			return nil, libacp.InternalError(fmt.Sprintf("acpsvc: attach to agent %q instance: %v", agentName, err))
		}
		return &externalAttach{instanceID: instanceID, downstreamID: downstreamID, bridge: bridge}, nil
	}

	// connCtx-owned (fallback): the subprocess dies with this connection, and the
	// bridge IS the wired libacp.Client — so terminal/* is serviced here when a shell
	// manager is present.
	//
	// This branch spawns from the external_acp config in person, and a chain agent
	// has none (resolveExternalAgent hands back a deliberately zero one — see
	// there). Building a subprocess out of those zero bytes would spawn nothing
	// coherent, so refuse honestly and name the remedy. A chain unit needs the
	// Manager, which owns the self-spawn that binds this binary to a chain file;
	// an editor session's embedded fleet wires one, the bare stdio transport does
	// not.
	if agent.Kind == runtimetypes.AgentKindChain {
		return nil, libacp.NewErrorf(libacp.ErrInvalidParams,
			"contenox.agent %q is a chain agent, which this transport cannot run: chain units are spawned by the fleet manager (fire them as a mission from an editor session, e.g. `/mission %s <intent>`)", agentName, agentName)
	}
	// Seed the sandbox workspace from this session's cwd when the agent declares
	// none of its own — the same defaulting the Instances path (Manager.StartResolved)
	// and the one-shot path (agenthost.DriveTurn) do. An agent is normally registered
	// by command alone, its working directory chosen per session, so without this the
	// wall fail-closes on an empty cwd. A declared Cwd still wins.
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

// externalTarget names the LIVE downstream a driver call drives. It carries exactly
// one of the two ownership modes (see externalDriver): a raw connection the driver
// owns (connCtx path, instanceID ""), or a Manager instance id to drive THROUGH (the
// Instances path, conn nil — the connection belongs to the kernel and no consumer
// holds it). Every downstream operation branches on instanceID exactly once, in the
// drive* helpers below.
type externalTarget struct {
	conn         *libacp.ClientSideConnection
	instanceID   string
	downstreamID libacp.SessionID
	bridge       *externalBridge
}

// ensureAttached returns the driver's live downstream target, acquiring it lazily on
// first use. On the Manager path a session/load persisted the instanceID, so the first
// prompt after a load RE-ATTACHES to the still-running instance (Attach) — the
// downstream agent's context is PRESERVED — falling back to a fresh bring-up only when
// that instance is gone. On the nil-Instances (connCtx) path there is no instance to
// re-attach to: the first prompt after a load freshly respawns, restarting the
// downstream's context (a documented v1 limit of that path).
//
// The attached/not-attached sentinel is the BRIDGE, not the connection: the Instances
// path holds no connection at all, so a nil conn there means "kernel-owned", not
// "detached".
func (d *externalDriver) ensureAttached(ctx context.Context, upstreamID libacp.SessionID, sess *sessionEntry) (*externalTarget, error) {
	d.mu.Lock()
	if d.bridge != nil {
		tgt := &externalTarget{conn: d.conn, instanceID: d.instanceID, downstreamID: d.downstreamID, bridge: d.bridge}
		d.mu.Unlock()
		return tgt, nil
	}
	instanceID, downstreamID := d.instanceID, d.downstreamID
	d.mu.Unlock()

	// Re-attach to a still-running Manager instance first (survives reconnect). Under the
	// R1 kernel the bridge no longer survives on the instance, so a reconnect builds a
	// FRESH viewer: it recovers the downstream session id (persisted at bring-up, restored
	// onto the driver by markExternalIfPersisted), attaches a new bridge as a viewer keyed
	// by that downstream session id, and drives prompts against the SAME downstream
	// session — preserving the agent's context. No connection is fetched: the instance
	// keeps it and is driven through the Manager's session API.
	// The journal REPLAY is suppressed: the durable chatservice transcript already replayed
	// the pre-drop turn at session/load, so relaying the backlog too would double-emit it.
	if d.t.deps.Instances != nil && instanceID != "" && downstreamID != "" {
		if st, err := d.t.deps.Instances.Get(instanceID); err == nil && st.State == agentinstance.StateRunning {
			bridge := newExternalBridge(d.t, upstreamID, true)
			bridge.setDownstreamID(downstreamID)
			bridge.suppressReplay()
			bridge.bindInstance(d.t.deps.Instances, instanceID)
			if _, err := d.t.deps.Instances.Attach(ctx, instanceID, downstreamID, bridge); err == nil {
				return d.commitReattach(instanceID, downstreamID, bridge), nil
			}
			// Attach failed: drop this redundant bridge and fall through to a fresh
			// bring-up (which restarts the downstream's context — the documented loss).
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

// commitReattach adopts a re-attached Manager instance and its fresh viewer bridge onto
// the driver. It re-enables the (suppressed) relay now that Attach's backlog replay has
// drained, so subsequent LIVE downstream updates reach this connection. No config push
// is needed: session/load already restored the reopened toolbar from the persisted
// config-option set (reloadedConfigOptions), and the kernel's captured surface is the
// live truth from here on.
func (d *externalDriver) commitReattach(instanceID string, downstreamID libacp.SessionID, bridge *externalBridge) *externalTarget {
	bridge.resumeRelay()
	d.mu.Lock()
	if d.bridge != nil {
		// Lost a race: another prompt re-attached first. Detach our redundant viewer
		// (distinct bridge id, so it is a real second attachment) and use the winner.
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

// commitBringUp adopts a freshly brought-up downstream onto the driver (winner logic
// for a concurrent prompt), persists the possibly-new Manager instanceID and downstream
// session id so a later reconnect re-attaches to THIS instance's SAME session, and pushes
// the reconnected downstream's config options to restore the reloaded toolbar (the lazy
// bring-up case).
func (d *externalDriver) commitBringUp(ctx context.Context, upstreamID libacp.SessionID, att *externalAttach) (*externalTarget, error) {
	d.mu.Lock()
	if d.bridge != nil {
		// Lost a race: keep the winner, discard (close/stop) ours.
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

// promptDownstream drives one downstream prompt turn against tgt and returns its stop
// reason. On the Instances path the kernel owns the connection, so the turn goes
// through Manager.Prompt (which is itself cancellation-aware and resolves a cancelled
// turn as StopReasonCancelled with a nil error, exactly as the caller's error branch
// does for the raw path).
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
// slash-command interception and the native chain engine entirely: the prompt
// blocks go straight to the downstream session/prompt and its stopReason is
// returned. Upstream session/cancel is forwarded downstream as session/cancel,
// and the user prompt plus the concatenated downstream reply are persisted so
// session/list titles and session/load replay work.
func (d *externalDriver) Prompt(ctx context.Context, req libacp.PromptRequest, sess *sessionEntry) (libacp.PromptResponse, error) {
	t := d.t
	reportErr, reportChange, end := t.tracker().Start(ctx, "prompt", "acp_external_session", "session_id", string(req.SessionID))
	defer end()

	tgt, err := d.ensureAttached(ctx, req.SessionID, sess)
	if err != nil {
		reportErr(err)
		return libacp.PromptResponse{}, err
	}

	// Forward an upstream session/cancel to the downstream agent as session/cancel.
	// Registered so Transport.Cancel, a Close/Delete, or a connection drop invokes
	// it; the sync.Once keeps the deferred unregister from ever sending a stray
	// cancel after a normal completion.
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
		// JSON-RPC error, per the ACP contract (both the $/cancel_request path,
		// which cancels ctx, and the session/cancel path, which force-resolves the
		// downstream turn, land here or below).
		if errors.Is(promptErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			reportChange(string(req.SessionID), map[string]any{"stop_reason": string(libacp.StopReasonCancelled)})
			return libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, nil
		}
		reportErr(promptErr)
		return libacp.PromptResponse{}, libacp.InternalError(promptErr.Error())
	}

	// Push the post-turn session_info_update (freshness + derived title) exactly
	// as the native path does, so the client's sidebar label updates without a
	// re-list.
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

// externalTurnCapture records a downstream agent's turn as an ORDERED stream of
// display segments so the whole turn — prose, reasoning, and tool cards — can be
// persisted and replayed faithfully on a later session/load, not collapsed to
// its final assistant text. It is display-only state: an external session's
// persisted history is never fed to a model (the downstream agent keeps its own
// context), so it mirrors exactly what the transcript showed rather than a
// model-valid message list. Guarded by externalBridge.mu.
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
// tool call, accumulated across its tool_call/tool_call_update frames. It is the
// JSON persisted as a "tool"-role message's Content and re-emitted verbatim as a
// tool_call update on replay (see externalToolReplayUpdate), preserving the
// downstream's own title/kind/diffs that the native ToolCall model can't carry.
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
	// Later frames win per field (last non-empty), mirroring the client reducer's
	// merge so the persisted record matches the final rendered card.
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
// assistant message, flushed at every tool boundary so the interleaving of prose
// and tool cards survives replay. Monotonic timestamps (base + i ms) preserve
// order through the store's added_at sort.
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

// persistExternalTurn records a downstream agent's turn — the user prompt plus
// the captured assistant transcript (text, reasoning, and tool calls) — into the
// same message store the native path uses (identity 'acp-client', keyed by the
// internal contenox session id), so session/list titles and session/load replay
// work for external sessions too. Fresh message IDs make PersistDiff's ID-based
// dedupe append them. Uses a cancellation-immune context so a cancelled turn
// still records what was said.
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
// session/resume) onto an external driver when a persisted agent name exists, so
// its config options come back minimal and the next prompt routes to the
// downstream agent (re-attached or lazily brought up) instead of the native chain.
// This — with NewSession's `_meta` check — is the sole place the native-vs-external
// driver is chosen.
func (t *Transport) markExternalIfPersisted(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID, entry *sessionEntry) {
	name := t.readSessionAgent(ctx, store, sid)
	if name == "" {
		return
	}
	ed := &externalDriver{t: t, agentName: name, upstreamID: sid}
	// On the Manager path, recover the persisted instanceID AND downstream session id so
	// the first prompt after this load RE-ATTACHES to the same still-running instance and
	// drives its SAME downstream session (ensureAttached), rather than bringing up a fresh
	// one. Absent/stale is fine — a missing downstream id or a failed re-attach falls back
	// to a fresh bring-up. The nil-Instances path stores neither, so both stay "".
	if t.deps.Instances != nil {
		ed.instanceID = t.readSessionInstance(ctx, store, sid)
		ed.downstreamID = t.readSessionDownstream(ctx, store, sid)
	}
	entry.driver = ed
	// Restore the persisted per-session HITL policy so the reopened toolbar's picker
	// shows the previously-chosen value (the entry was rebuilt with the sentinel
	// default) and resolveSessionHITLPolicy enforces it — the native chain keeps this
	// selection only in-memory, so unlike a native session an external one persists it.
	if policy := t.readSessionHITLPolicy(ctx, store, sid); policy != "" {
		entry.setHITLPolicy(policy)
	}
}

// reloadedConfigOptions returns the config options to advertise on a session/load or
// session/resume response. A native session dispatches to its driver (the chain-engine
// selects). An external session's downstream is NOT respawned during load, so its
// downstream surface (synthetic mode select + synthetic model select + the agent's own
// pickers) comes from the set persisted at session/new (and each later update/set), and
// contenox's own HITL
// policy select rides AFTER it — its CurrentValue restored from the persisted
// per-session selection (see markExternalIfPersisted, which runs before this) so the
// reopened toolbar's picker survives the reload without waiting for a respawn.
func (t *Transport) reloadedConfigOptions(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID, entry *sessionEntry) []libacp.SessionConfigOption {
	if _, ok := entry.driver.(*externalDriver); ok {
		downstream := t.readSessionAgentConfigOptions(ctx, store, sid)
		return append(downstream, t.hitlPolicyConfigOption(entry))
	}
	return t.sessionConfigOptions(ctx, entry)
}

// reemitExternalCommandMenu schedules the persisted downstream slash-command menu to
// be relayed to the upstream client strictly AFTER the load/resume result is on the
// wire (via libacp.AfterResponse — the same ordering contract sendAvailableCommands and
// the session/new re-emit follow, since a client drops updates for a session id it has
// not yet learned), so a reopened external session shows its menu without a first prompt
// to respawn the downstream. A no-op when the session persisted no menu.
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
