package agentinstance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/contenox/beam/internal/version"
	"github.com/contenox/beam/libacp"
)

// driverClientName is the ClientInfo.Name the kernel presents to a downstream
// agent at initialize. It identifies the RUNTIME (not any transport) as the ACP
// client driving the agent — the kernel owns this handshake now, not acpsvc.
const driverClientName = "contenox-runtime"

// errNoConn is returned by every session-driving method when the instance has no
// live downstream connection to drive (a process-less instance, or one whose
// subprocess is not up). It is a sentinel so a consumer can branch on it.
var errNoConn = errors.New("agentinstance: instance has no live downstream connection")

// AgentModeConfigOptionID is the reserved SessionConfigOption id under which a
// session surfaces the DOWNSTREAM agent's session Modes (its SessionModeState) as a
// single synthetic "select" picker. The ACP spec models session modes (session/set_mode
// + SessionModeState) and config options (session/set_config_option + SessionConfigOption)
// as two distinct surfaces; the kernel folds the modes onto one synthetic config option so
// a single config-option picker can render both — id AgentModeConfigOptionID, label "Mode",
// type "select", each availableMode a value(id)→label(name), currentValue the currentModeId.
// A SetConfigOption on this id is translated back to session/set_mode (see setConfigOption),
// and a downstream current_mode_update is captured onto this same synthetic option. It lives
// in a reserved dotted namespace so it never collides with a downstream agent's own option
// ids. (Relocated from runtime/acpsvc.)
const AgentModeConfigOptionID = "contenox.agent-mode"

// AgentModelConfigOptionID is the reserved SessionConfigOption id under which a session
// surfaces the DOWNSTREAM agent's UNSTABLE model-picker state (its SessionModelState) as a
// single synthetic "select" picker — the exact parallel of AgentModeConfigOptionID for
// modes. A SetConfigOption on this id is translated back to the unstable session/set_model.
// Unlike modes, the ACP session/update stream carries NO model-update kind, so there is
// nothing to capture after a switch: the (stateless) set_model response is the truth and the
// confirmed model is adopted locally. Placed after the synthetic mode option and before the
// downstream's own config options. (Relocated from runtime/acpsvc.)
const AgentModelConfigOptionID = "contenox.agent-model"

// SessionSpec is the fully-resolved input to Manager.OpenSession: everything the kernel
// needs to negotiate the downstream connection's capabilities and drive session/new. It is
// transport-agnostic — a CLI, a scheduler, or acpsvc all describe a session the same way.
type SessionSpec struct {
	// Cwd is the downstream session's working directory (ACP session/new requires one;
	// spec-correct agents expect it absolute).
	Cwd string

	// AdditionalDirectories are extra absolute workspace roots for the session, on top of
	// Cwd. Omitted/empty means none.
	AdditionalDirectories []string

	// McpServers are the ALREADY-RESOLVED MCP servers to forward downstream in session/new.
	// Resolving an agent's mcp_servers allowlist against the store is a transport concern (it
	// needs the DB); the kernel only forwards what it is handed, dropping any the downstream's
	// initialize-advertised mcpCapabilities cannot consume. Nil/empty forwards none.
	McpServers []libacp.McpServer

	// Meta is an OPAQUE session/new `_meta` blob the consumer wants forwarded to the downstream
	// verbatim. The kernel stays policy-free: it neither reads nor interprets this — it forwards
	// exactly what it is handed, the same contract McpServers has. A dispatcher (fleetservice)
	// uses it to hand a spawned unit its mission id at construction (missionservice.MissionMetaKey);
	// the kernel does not know or care that that is what the bytes mean. Nil forwards no `_meta`.
	Meta json.RawMessage

	// Terminal advertises the terminal CLIENT capability to the downstream at initialize —
	// telling the agent it MAY route shell commands through the terminal/* callback family.
	//
	// Advertisement rule: the kernel advertises the terminal capability to the downstream IFF
	// this is set, i.e. iff the consumer says a terminal server MAY attach. It is negotiated
	// ONCE per connection at the first OpenSession (ACP client capabilities are connection-
	// scoped, not per session), so the first session's Terminal decides it for the connection's
	// lifetime. Setting it does not by itself serve terminals: the harness still gates every
	// terminal/* on the session's actual controller viewer implementing TerminalServer,
	// answering MethodNotFound otherwise. Left false, the downstream is never told terminals
	// exist and every terminal/* is refused with MethodNotFound.
	Terminal bool
}

// sessionDriver is the instance's session-driving brain: the initialize-once handshake
// state for its downstream connection, plus the per-session captured surface (config
// options / modes / models / slash-command menu). It is transport-agnostic and holds no
// connection — the instance supplies the live connection to each driving call; the driver
// only issues the state mapping and the capture.
type sessionDriver struct {
	// initMu serializes the initialize handshake so an instance's downstream connection is
	// initialize'd exactly once per connection. It is held across the network Initialize call
	// (bounded by the caller ctx); concurrent OpenSessions on the same instance wait, which is
	// correct — they all need the same connection handshaken before session/new. It re-arms
	// implicitly across a watchDog restart: the fresh connection is a different pointer, so
	// initConn no longer matches and the next OpenSession re-initializes it (the downstream's
	// session context was lost anyway — see the package doc's ACP restart caveat).
	initMu   sync.Mutex
	initConn *libacp.ClientSideConnection
	initResp libacp.InitializeResponse

	mu       sync.Mutex
	sessions map[libacp.SessionID]*driveSession
}

func newSessionDriver() *sessionDriver {
	return &sessionDriver{sessions: make(map[libacp.SessionID]*driveSession)}
}

// driveSession is the kernel's captured state for ONE downstream session: the downstream
// agent's own advertised config options, its session Modes and UNSTABLE model-picker state
// (surfaced as synthetic config options), and its slash-command menu. It is captured from
// the session/new seed and kept current by the downstream session/update stream
// (config_option_update / current_mode_update / available_commands_update) and by confirmed
// set_config_option / set_mode / set_model calls. All access is under mu. (The seed-vs-live
// received/pending machinery is relocated verbatim from acpsvc's externalBridge, because the
// same race exists: OpenSession seeds after session/new returns, while the downstream read
// loop may already have delivered a live update.)
type driveSession struct {
	mu sync.Mutex

	// cwd is the session's working directory, the SessionSpec.Cwd this session was
	// opened with (session/new's cwd). It is captured here purely so a policy-free
	// reader can recover a session's workspace root without attaching — the
	// attention layer's scope-anomaly check needs "which tree was this unit
	// supposed to work in" to tell an in-scope touch from a wander. The kernel
	// neither interprets nor enforces it; containment is the tool/HITL layer's job.
	cwd string

	// configOptions is the downstream agent's OWN advertised config-option set
	// (full-replacement per spec); configReceived records that a LIVE update or a confirmed
	// set has superseded the session/new seed, so a late seed never clobbers it.
	configOptions  []libacp.SessionConfigOption
	configReceived bool

	// modeState is the downstream agent's session Modes (nil when it advertises none).
	// modeReceived + pendingModeID handle a current_mode_update that raced ahead of the seed.
	modeState     *libacp.SessionModeState
	modeReceived  bool
	pendingModeID string

	// modelState is the downstream agent's UNSTABLE model-picker state (nil when none). No
	// received/pending race machinery: the stream carries no model-update kind, so nothing
	// can race the seed — the only live mutation is a confirmed set_model.
	modelState *libacp.SessionModelState

	// commands is the latest downstream available_commands_update payload (full-replacement).
	commands []libacp.AvailableCommand
}

// get returns the driveSession for sid, creating it on first use. The state may be created
// either by OpenSession's seed or by a captured update on the read loop — whichever wins the
// race — so both paths get-or-create.
func (sd *sessionDriver) get(sid libacp.SessionID) *driveSession {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	ds := sd.sessions[sid]
	if ds == nil {
		ds = &driveSession{}
		sd.sessions[sid] = ds
	}
	return ds
}

// peek returns the driveSession for sid or nil when none exists — the read path for the
// accessors and setConfigOption, which never want to create empty state.
func (sd *sessionDriver) peek(sid libacp.SessionID) *driveSession {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	return sd.sessions[sid]
}

// drop forgets sid's captured state (CloseSession).
func (sd *sessionDriver) drop(sid libacp.SessionID) {
	sd.mu.Lock()
	delete(sd.sessions, sid)
	sd.mu.Unlock()
}

// sessionIDs returns the ids of every session the driver currently holds, sorted for a
// deterministic snapshot (sd.sessions is a map, so iteration order is otherwise unstable).
// Always non-nil, so InstanceStatus.SessionIDs serializes as `[]`, never `null`.
//
// This is the AUTHORITATIVE answer to "which sessions are open on this instance": the
// entry is created by OpenSession's seed (or by a captured update on the read loop —
// whichever wins the race, see get) and removed by CloseSession's drop. It deliberately
// does NOT go through the viewer hub, whose per-session state materializes only on a
// session's first delivered update or first attach: a session that is open but has emitted
// nothing yet is absent from the hub and present here, which is the point — a
// cancel-everything fan-out (fleetservice.Cancel) and an adopt (acpsvc) must both see it.
//
// Same mutex discipline as get/peek/drop: sd.mu guards the map, which is written from BOTH
// the request path (OpenSession/CloseSession) and the downstream read loop (capture).
func (sd *sessionDriver) sessionIDs() []string {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	ids := make([]string, 0, len(sd.sessions))
	for id := range sd.sessions {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	return ids
}

// owns reports whether sid is currently open on this driver — the same
// authoritative membership fact sessionIDs exposes as a slice, answered directly
// against the map so the Manager's DeliverToSession scan need not allocate one
// slice per instance just to test containment. Same mu discipline as
// sessionIDs/get/peek/drop.
func (sd *sessionDriver) owns(sid libacp.SessionID) bool {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	_, ok := sd.sessions[sid]
	return ok
}

// cwd returns sid's recorded working directory, or "" when the session is
// unknown or was opened without one. Peek semantics (never creates state),
// matching the other read accessors.
func (sd *sessionDriver) cwd(sid libacp.SessionID) string {
	if ds := sd.peek(sid); ds != nil {
		return ds.getCwd()
	}
	return ""
}

// capture folds a downstream session/update into the session's captured state. It is called
// from the instance's journaling harness on the read-loop goroutine, BEFORE the fan-out, so
// an accessor called right after a viewer observed an update sees the same value. Only the
// three surface-bearing update kinds create/touch state; everything else is ignored here
// (it still journals + fans out via the hub). This is acpsvc's externalBridge.SessionUpdate
// capture logic relocated — minus the upstream relay/remap, which stays a transport concern.
func (sd *sessionDriver) capture(n libacp.SessionNotification) {
	switch n.Update.SessionUpdate {
	case libacp.SessionUpdateAvailableCommands, libacp.SessionUpdateConfigOption, libacp.SessionUpdateCurrentMode:
	default:
		return
	}
	ds := sd.get(n.SessionID)
	ds.mu.Lock()
	defer ds.mu.Unlock()
	switch n.Update.SessionUpdate {
	case libacp.SessionUpdateAvailableCommands:
		ds.commands = n.Update.AvailableCommands
	case libacp.SessionUpdateConfigOption:
		ds.configReceived = true
		ds.configOptions = n.Update.ConfigOptions
	case libacp.SessionUpdateCurrentMode:
		ds.modeReceived = true
		if ds.modeState != nil {
			ds.modeState.CurrentModeID = n.Update.CurrentModeID
		} else {
			// Raced ahead of the session/new seed; remember it for seed to fold in.
			ds.pendingModeID = n.Update.CurrentModeID
		}
	}
}

// seed records the session/new response's advertised config options, modes, and models as
// the initial surface, yielding to any live update that already arrived. A current_mode_update
// that raced ahead left its currentModeId in pendingModeID; it is folded in so the raced
// update is not lost.
func (ds *driveSession) seed(opts []libacp.SessionConfigOption, modes *libacp.SessionModeState, models *libacp.SessionModelState) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if !ds.configReceived {
		ds.configOptions = opts
	}
	if ds.modeState == nil && modes != nil {
		cp := *modes
		if ds.modeReceived && ds.pendingModeID != "" {
			cp.CurrentModeID = ds.pendingModeID
		}
		ds.modeState = &cp
	}
	if ds.modelState == nil && models != nil {
		cp := *models
		ds.modelState = &cp
	}
}

// setCwd records the session's working directory (OpenSession's spec.Cwd). Under
// ds.mu like every other field, so a concurrent reader (the scope-anomaly
// accessor) sees a consistent value.
func (ds *driveSession) setCwd(cwd string) {
	ds.mu.Lock()
	ds.cwd = cwd
	ds.mu.Unlock()
}

// getCwd returns the session's recorded working directory, or "" when none was
// set. "" reads as "no known workspace root", which the attention layer treats
// as "no lane to have left" (anomaly detection disabled) rather than guessing.
func (ds *driveSession) getCwd() string {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return ds.cwd
}

// applyConfigOptions adopts a downstream-confirmed option set (from a set_config_option
// response), marking the live set received so a later seed cannot clobber it.
func (ds *driveSession) applyConfigOptions(opts []libacp.SessionConfigOption) {
	ds.mu.Lock()
	ds.configReceived = true
	ds.configOptions = opts
	ds.mu.Unlock()
}

// applyMode adopts an upstream-confirmed mode (from a set_mode the kernel forwarded) into the
// synthetic mode option's currentValue. The set_mode response carries no state, so the
// requested modeId is authoritative; a downstream current_mode_update, if also emitted,
// reconfirms it.
func (ds *driveSession) applyMode(modeID string) {
	ds.mu.Lock()
	ds.modeReceived = true
	if ds.modeState != nil {
		ds.modeState.CurrentModeID = modeID
	} else {
		ds.pendingModeID = modeID
	}
	ds.mu.Unlock()
}

// applyModel adopts an upstream-confirmed model (from a set_model the kernel forwarded) into
// the synthetic model option's currentValue. No-op when the downstream advertised no models.
func (ds *driveSession) applyModel(modelID string) {
	ds.mu.Lock()
	if ds.modelState != nil {
		ds.modelState.CurrentModelID = modelID
	}
	ds.mu.Unlock()
}

// snapshotConfigOptions returns the session's full config-option surface: the synthetic mode
// select first (when the downstream advertises modes), then the synthetic model select (when
// it advertises models), then the downstream agent's own options. Always a fresh slice, so a
// consumer can append (e.g. a transport's own HITL select) without corrupting kernel state.
func (ds *driveSession) snapshotConfigOptions() []libacp.SessionConfigOption {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return ds.buildConfigOptionsLocked()
}

// buildConfigOptionsLocked assembles the synthetic-mode-first, synthetic-model-second,
// downstream-own surface. Caller holds ds.mu. Always returns a fresh slice.
func (ds *driveSession) buildConfigOptionsLocked() []libacp.SessionConfigOption {
	modeOpt, hasMode := syntheticModeOption(ds.modeState)
	modelOpt, hasModel := syntheticModelOption(ds.modelState)
	out := make([]libacp.SessionConfigOption, 0, len(ds.configOptions)+2)
	if hasMode {
		out = append(out, modeOpt)
	}
	if hasModel {
		out = append(out, modelOpt)
	}
	out = append(out, ds.configOptions...)
	return out
}

// availableCommands returns a copy of the session's latest downstream slash-command menu.
func (ds *driveSession) availableCommands() []libacp.AvailableCommand {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return append([]libacp.AvailableCommand(nil), ds.commands...)
}

// syntheticModeOption maps a downstream SessionModeState onto the single synthetic "Mode"
// select (see AgentModeConfigOptionID). ok=false when there are no modes to surface.
// (Relocated from runtime/acpsvc.)
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

// syntheticModelOption maps a downstream SessionModelState onto the single synthetic "Model"
// select (see AgentModelConfigOptionID). ok=false when there are no models to surface.
// (Relocated from runtime/acpsvc.)
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

// filterMcpForCaps drops forwarded servers the downstream cannot consume, per its
// initialize-advertised mcpCapabilities: stdio is the protocol baseline and always passes;
// http and sse are gated on the matching capability flag. Always returns a non-nil slice, so
// session/new (whose mcpServers field is not omitempty) never sends a JSON null. (Relocated
// from acpsvc's filterMcpForCaps / agenthost's filterMcpServersByCapabilities.)
func filterMcpForCaps(servers []libacp.McpServer, caps libacp.McpCapabilities) []libacp.McpServer {
	kept := make([]libacp.McpServer, 0, len(servers))
	for _, srv := range servers {
		switch srv.Kind() {
		case libacp.McpServerKindHTTP:
			if !caps.HTTP {
				continue
			}
		case libacp.McpServerKindSSE:
			if !caps.SSE {
				continue
			}
		}
		kept = append(kept, srv)
	}
	return kept
}

// -----------------------------------------------------------------------------
// Instance-level driving. These issue the downstream ACP calls on the instance's
// live connection and fold the results into the sessionDriver's captured state.
// The Manager exposes them as its session-driving API; a transport is a THIN
// consumer that calls these and OBSERVES the stream by attaching viewers.
// -----------------------------------------------------------------------------

// openSession ensures the downstream connection is initialize'd once (capabilities negotiated
// per spec), drives session/new, and seeds the session's captured surface. Returns the
// downstream session id — the id the instance journals/fans out under and viewers Attach to.
// This is acpsvc's initExternalConn, relocated (minus the transport-side persistence/relay).
func (i *instance) openSession(ctx context.Context, spec SessionSpec) (libacp.SessionID, error) {
	conn := i.conn()
	if conn == nil {
		return "", errNoConn
	}
	initResp, err := i.ensureInitialized(ctx, conn, spec.Terminal)
	if err != nil {
		return "", err
	}
	forwarded := filterMcpForCaps(spec.McpServers, initResp.AgentCapabilities.McpCapabilities)
	resp, err := conn.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:                   spec.Cwd,
		AdditionalDirectories: spec.AdditionalDirectories,
		McpServers:            forwarded,
		Meta:                  spec.Meta,
	})
	if err != nil {
		return "", fmt.Errorf("agentinstance: session/new: %w", err)
	}
	ds := i.driver.get(resp.SessionID)
	ds.seed(resp.ConfigOptions, resp.Modes, resp.Models)
	ds.setCwd(spec.Cwd)
	return resp.SessionID, nil
}

// ensureInitialized runs the downstream initialize handshake exactly once per connection,
// negotiating the terminal client capability per the spec (see SessionSpec.Terminal). A cached
// result is returned when conn matches the last-initialized connection; a fresh connection (a
// watchDog restart) re-initializes. The initMu is held across the network call so concurrent
// OpenSessions serialize on it rather than double-initializing.
func (i *instance) ensureInitialized(ctx context.Context, conn *libacp.ClientSideConnection, terminal bool) (libacp.InitializeResponse, error) {
	i.driver.initMu.Lock()
	defer i.driver.initMu.Unlock()
	if i.driver.initConn == conn {
		return i.driver.initResp, nil
	}
	clientCaps := libacp.ClientCapabilities{}
	if terminal {
		clientCaps.Terminal = true
	}
	resp, err := conn.Initialize(ctx, libacp.InitializeRequest{
		ProtocolVersion:    libacp.ProtocolVersion,
		ClientCapabilities: clientCaps,
		ClientInfo:         &libacp.Implementation{Name: driverClientName, Version: version.Get()},
	})
	if err != nil {
		return libacp.InitializeResponse{}, fmt.Errorf("agentinstance: initialize downstream: %w", err)
	}
	if resp.ProtocolVersion != libacp.ProtocolVersion {
		return libacp.InitializeResponse{}, fmt.Errorf("agentinstance: downstream negotiated unsupported protocol version %d", resp.ProtocolVersion)
	}
	i.driver.initConn = conn
	i.driver.initResp = resp
	return resp, nil
}

// promptSession drives one downstream session/prompt turn. Every downstream session/update
// during the turn is journaled + fanned out to viewers by the instance's harness (already the
// mechanism); this call only carries the request/response plane. It is cancellation-aware: a
// ctx cancellation (or a concurrent Cancel that force-resolves the turn) resolves cleanly as
// StopReasonCancelled with a nil error, per the ACP prompt-turn contract, rather than a
// JSON-RPC error. This is acpsvc's externalDriver.Prompt, relocated (minus transport-side
// capture/persistence, which a viewer does by accumulating from Deliver).
func (i *instance) promptSession(ctx context.Context, sid libacp.SessionID, prompt []libacp.ContentBlock) (libacp.StopReason, error) {
	conn := i.conn()
	if conn == nil {
		return "", errNoConn
	}
	resp, err := conn.Prompt(ctx, libacp.PromptRequest{SessionID: sid, Prompt: prompt})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return libacp.StopReasonCancelled, nil
		}
		return "", fmt.Errorf("agentinstance: session/prompt: %w", err)
	}
	return resp.StopReason, nil
}

// cancelSession cancels sid's in-flight prompt turn on the downstream: it sends session/cancel
// and, for as long as this session's Prompt call is outstanding, auto-resolves the turn's
// permission requests as cancelled (libacp.CancelPrompt implements both halves of the
// prompt-turn cancellation contract). Safe to call with no turn in flight (it degrades to a
// bare session/cancel notification).
func (i *instance) cancelSession(sid libacp.SessionID) error {
	conn := i.conn()
	if conn == nil {
		return errNoConn
	}
	return conn.CancelPrompt(sid)
}

// closeSession ends sid: it best-effort tells the downstream to stop any in-flight turn
// (session/cancel — a notification an idle agent ignores), then drops the kernel's per-session
// state (the captured surface AND the journal + viewer registry, firing a detach for each
// still-attached viewer). It does NOT tear down the instance or its connection: an instance
// multiplexes many sessions over one connection, so only Stop/Close end the process.
func (i *instance) closeSession(sid libacp.SessionID) error {
	if conn := i.conn(); conn != nil {
		_ = conn.CancelSession(libacp.CancelNotification{SessionID: sid})
	}
	i.driver.drop(sid)
	i.hub.closeSession(sid)
	return nil
}

// setConfigOption forwards an upstream config-option change to the downstream and adopts the
// confirmed value into captured state. The two synthetic option ids are not real downstream
// config options: AgentModeConfigOptionID translates to session/set_mode and
// AgentModelConfigOptionID to the unstable session/set_model; every other id forwards to
// session/set_config_option unchanged. The kernel performs NO validation — the downstream owns
// its option semantics and rejects an unknown id/value with its own error. (This is acpsvc's
// externalDriver.SetConfigOption mapping, relocated; the contenox-native HITL policy option,
// which the runtime enforces rather than the agent, stays a transport concern and is
// intercepted before reaching the kernel.)
func (i *instance) setConfigOption(ctx context.Context, sid libacp.SessionID, configID string, value libacp.SessionConfigOptionValue) error {
	conn := i.conn()
	if conn == nil {
		return errNoConn
	}
	ds := i.driver.peek(sid)
	if ds == nil {
		return fmt.Errorf("agentinstance: unknown session %q", sid)
	}
	switch configID {
	case AgentModeConfigOptionID:
		if _, err := conn.SetSessionMode(ctx, libacp.SetSessionModeRequest{SessionID: sid, ModeID: value.AsString()}); err != nil {
			return err
		}
		ds.applyMode(value.AsString())
		return nil
	case AgentModelConfigOptionID:
		if _, err := conn.SetSessionModel(ctx, libacp.SetSessionModelRequest{SessionID: sid, ModelID: value.AsString()}); err != nil {
			return err
		}
		ds.applyModel(value.AsString())
		return nil
	default:
		resp, err := conn.SetSessionConfigOption(ctx, libacp.SetSessionConfigOptionRequest{SessionID: sid, ConfigID: configID, Value: value})
		if err != nil {
			return err
		}
		ds.applyConfigOptions(resp.ConfigOptions)
		return nil
	}
}

// sessionConfigOptions returns the session's captured config-option surface (synthetic mode +
// synthetic model + downstream's own), or nil for an unknown session. It feeds a transport's
// upstream advertisement; the CAPTURE is the kernel's.
func (i *instance) sessionConfigOptions(sid libacp.SessionID) []libacp.SessionConfigOption {
	ds := i.driver.peek(sid)
	if ds == nil {
		return nil
	}
	return ds.snapshotConfigOptions()
}

// availableCommands returns the session's captured downstream slash-command menu, or nil for
// an unknown session.
func (i *instance) availableCommands(sid libacp.SessionID) []libacp.AvailableCommand {
	ds := i.driver.peek(sid)
	if ds == nil {
		return nil
	}
	return ds.availableCommands()
}
