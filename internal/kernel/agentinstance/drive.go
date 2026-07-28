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

// driverClientName is the ClientInfo.Name the kernel presents to a
// downstream agent at initialize, identifying the runtime as the ACP client
// driving the agent.
const driverClientName = "contenox-runtime"

// errNoConn is returned by every session-driving method when the instance
// has no live downstream connection. A sentinel so a consumer can branch on
// it.
var errNoConn = errors.New("agentinstance: instance has no live downstream connection")

// AgentModeConfigOptionID is the reserved SessionConfigOption id under which
// a session surfaces the downstream agent's session Modes as a single
// synthetic "select" option (type "select", one value per available mode,
// currentValue the current mode id). SetConfigOption on this id translates
// to session/set_mode; a downstream current_mode_update is captured onto it.
// A reserved dotted namespace so it never collides with a downstream
// agent's own option ids.
const AgentModeConfigOptionID = "contenox.agent-mode"

// AgentModelConfigOptionID is the same synthetic-option scheme as
// AgentModeConfigOptionID, for the downstream agent's unstable model-picker
// state. SetConfigOption on this id translates to session/set_model; unlike
// modes, there is no model-update stream kind, so the set_model response
// alone becomes the new current value.
const AgentModelConfigOptionID = "contenox.agent-model"

// SessionSpec is the fully-resolved input to Manager.OpenSession: everything
// the kernel needs to negotiate the downstream connection's capabilities and
// drive session/new.
type SessionSpec struct {
	// Cwd is the downstream session's working directory; ACP requires one,
	// and spec-correct agents expect it absolute.
	Cwd string

	// AdditionalDirectories are extra absolute workspace roots for the session, on top of
	// Cwd. Omitted/empty means none.
	AdditionalDirectories []string

	// McpServers are the already-resolved MCP servers to forward downstream
	// in session/new; the kernel drops any the downstream's advertised
	// mcpCapabilities cannot consume. Nil forwards none.
	McpServers []libacp.McpServer

	// Meta is an opaque session/new `_meta` blob forwarded verbatim; the
	// kernel neither reads nor interprets it. Nil forwards none.
	Meta json.RawMessage

	// Terminal advertises the terminal client capability to the downstream
	// at initialize, iff set — negotiated once per connection, at the first
	// OpenSession. Even when set, every terminal/* is still gated on the
	// session's controller implementing TerminalServer (else
	// MethodNotFound); left false, terminals are never advertised and
	// terminal/* always refuses.
	Terminal bool
}

// sessionDriver is the instance's session-driving brain: the initialize-once
// handshake state for its downstream connection, plus the per-session
// captured surface (config options / modes / models / commands). Holds no
// connection itself — callers supply it.
type sessionDriver struct {
	// initMu serializes the initialize handshake to exactly once per
	// connection, held across the network call so concurrent OpenSessions
	// wait rather than double-initializing. Re-arms across a watchDog
	// restart, since the fresh connection is a different pointer.
	initMu   sync.Mutex
	initConn *libacp.ClientSideConnection
	initResp libacp.InitializeResponse

	mu       sync.Mutex
	sessions map[libacp.SessionID]*driveSession
}

func newSessionDriver() *sessionDriver {
	return &sessionDriver{sessions: make(map[libacp.SessionID]*driveSession)}
}

// driveSession is the kernel's captured state for one downstream session:
// the downstream's own advertised config options, its session Modes and
// model-picker state (surfaced as synthetic options), and its slash-command
// menu. Seeded from session/new and kept current by the downstream update
// stream and confirmed set_* calls. All access is under mu.
type driveSession struct {
	mu sync.Mutex

	// cwd is the session's working directory (SessionSpec.Cwd), recorded so
	// a policy-free reader can recover a session's workspace root without
	// attaching.
	cwd string

	// configOptions is the downstream's own advertised config-option set
	// (full-replacement per spec); configReceived marks that a live update
	// or confirmed set has superseded the session/new seed.
	configOptions  []libacp.SessionConfigOption
	configReceived bool

	// modeState is the downstream's session Modes (nil if it advertises
	// none). modeReceived + pendingModeID handle a current_mode_update that
	// raced ahead of the seed.
	modeState     *libacp.SessionModeState
	modeReceived  bool
	pendingModeID string

	// modelState is the downstream's unstable model-picker state (nil if
	// none). No race machinery needed: the stream carries no model-update
	// kind.
	modelState *libacp.SessionModelState

	// commands is the latest downstream available_commands_update payload (full-replacement).
	commands []libacp.AvailableCommand
}

// get returns the driveSession for sid, creating it on first use — either
// path (OpenSession's seed or a captured update) may win the race.
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

// peek returns the driveSession for sid, or nil — the read path that never
// creates state.
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

// sessionIDs returns the ids of every session currently open on this
// instance, sorted for a deterministic snapshot, always non-nil. It is the
// authoritative "what is open" fact — set by OpenSession, cleared by
// CloseSession — independent of the viewer hub, whose per-session state
// materializes lazily; an open-but-silent session must still appear here.
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
// membership fact sessionIDs exposes as a slice, tested directly against
// the map.
func (sd *sessionDriver) owns(sid libacp.SessionID) bool {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	_, ok := sd.sessions[sid]
	return ok
}

// cwd returns sid's recorded working directory, or "" if unknown or unset.
func (sd *sessionDriver) cwd(sid libacp.SessionID) string {
	if ds := sd.peek(sid); ds != nil {
		return ds.getCwd()
	}
	return ""
}

// capture folds a downstream session/update into the session's captured
// state. Called from the journaling harness on the read-loop goroutine,
// before the fan-out, so an accessor read right after a viewer observes an
// update sees the same value. Only the three surface-bearing update kinds
// touch state; everything else is ignored here (still journaled/fanned out
// via the hub).
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

// seed records the session/new response's advertised config, modes, and
// models as the initial surface, yielding to any live update that already
// arrived. A current_mode_update that raced ahead is folded in from
// pendingModeID so it isn't lost.
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

// setCwd records the session's working directory (OpenSession's spec.Cwd),
// under ds.mu like every other field.
func (ds *driveSession) setCwd(cwd string) {
	ds.mu.Lock()
	ds.cwd = cwd
	ds.mu.Unlock()
}

// getCwd returns the session's recorded working directory, or "" if none
// was set — read by the attention layer as "no workspace root, skip anomaly
// detection".
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

// applyMode adopts an upstream-confirmed mode into the synthetic option's
// currentValue. The set_mode response carries no state, so the requested
// modeId is authoritative; a downstream current_mode_update, if also
// emitted, reconfirms it.
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

// snapshotConfigOptions returns the session's full config-option surface:
// synthetic mode select, then synthetic model select, then the downstream's
// own options. Always a fresh slice, so a consumer can append without
// corrupting kernel state.
func (ds *driveSession) snapshotConfigOptions() []libacp.SessionConfigOption {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return ds.buildConfigOptionsLocked()
}

// buildConfigOptionsLocked assembles the surface described by
// snapshotConfigOptions. Caller holds ds.mu.
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

// syntheticModeOption maps a downstream SessionModeState onto the single
// synthetic "Mode" select (see AgentModeConfigOptionID). ok is false when
// there are no modes to surface.
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

// syntheticModelOption maps a downstream SessionModelState onto the single
// synthetic "Model" select (see AgentModelConfigOptionID). ok is false when
// there are no models to surface.
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

// filterMcpForCaps drops forwarded servers the downstream cannot consume,
// per its initialize-advertised mcpCapabilities: stdio always passes; http
// and sse are gated on the matching capability flag. Always non-nil, so
// session/new's mcpServers field never sends a JSON null.
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

// openSession ensures the downstream connection is initialized once, drives
// session/new, and seeds the session's captured surface. Returns the
// downstream session id that the instance journals/fans out under and
// viewers Attach to.
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

// ensureInitialized runs the downstream initialize handshake exactly once
// per connection, negotiating the terminal capability per
// SessionSpec.Terminal. A cached result is returned when conn matches the
// last-initialized connection; a fresh connection (a watchDog restart)
// re-initializes. initMu is held across the network call so concurrent
// OpenSessions serialize rather than double-initializing.
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

// promptSession drives one downstream session/prompt turn; every update
// during the turn is journaled and fanned out via the instance's harness.
// Cancellation-aware: a ctx cancellation, or a concurrent Cancel that
// force-resolves the turn, resolves as StopReasonCancelled with a nil error
// rather than a JSON-RPC error.
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

// cancelSession cancels sid's in-flight prompt turn: sends session/cancel
// and, while this session's Prompt call is outstanding, auto-resolves its
// permission requests as cancelled. Safe with no turn in flight.
func (i *instance) cancelSession(sid libacp.SessionID) error {
	conn := i.conn()
	if conn == nil {
		return errNoConn
	}
	return conn.CancelPrompt(sid)
}

// closeSession ends sid: best-effort tells the downstream to stop any
// in-flight turn, then drops the kernel's per-session state (captured
// surface, journal, viewer registry — firing a detach for each attached
// viewer). Does not tear down the instance or its connection.
func (i *instance) closeSession(sid libacp.SessionID) error {
	if conn := i.conn(); conn != nil {
		_ = conn.CancelSession(libacp.CancelNotification{SessionID: sid})
	}
	i.driver.drop(sid)
	i.hub.closeSession(sid)
	return nil
}

// setConfigOption forwards an upstream config-option change to the
// downstream and adopts the confirmed value into captured state.
// AgentModeConfigOptionID and AgentModelConfigOptionID translate to
// session/set_mode and session/set_model; every other id forwards to
// session/set_config_option unchanged. The kernel performs no validation —
// the downstream owns its option semantics.
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

// sessionConfigOptions returns the session's captured config-option surface
// (synthetic mode + synthetic model + downstream's own), or nil for an
// unknown session.
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
