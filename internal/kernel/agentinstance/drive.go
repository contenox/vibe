package agentinstance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/contenox/contenox/internal/version"
	"github.com/contenox/contenox/libacp"
)

const driverClientName = "contenox-runtime"

var errNoConn = errors.New("agentinstance: instance has no live downstream connection")

// AgentModeConfigOptionID is the reserved SessionConfigOption id that surfaces the
// downstream agent's session Modes as a synthetic select option; setting it
// translates to session/set_mode.
const AgentModeConfigOptionID = "contenox.agent-mode"

// AgentModelConfigOptionID is the reserved SessionConfigOption id that surfaces the
// downstream agent's model-picker state as a synthetic select option; setting it
// translates to session/set_model.
const AgentModelConfigOptionID = "contenox.agent-model"

// SessionSpec is the fully-resolved input to Manager.OpenSession: everything
// the kernel needs to negotiate the downstream connection's capabilities and
// drive session/new.
type SessionSpec struct {
	// Cwd is the downstream session's working directory; ACP requires one,
	// and spec-correct agents expect it absolute.
	Cwd string

	// AdditionalDirectories are extra absolute workspace roots beyond Cwd; omitted/empty
	// means none.
	AdditionalDirectories []string

	// McpServers are the already-resolved MCP servers to forward in session/new,
	// filtered to what the downstream's mcpCapabilities can consume; nil forwards none.
	McpServers []libacp.McpServer

	// Meta is an opaque session/new `_meta` blob forwarded verbatim, unread by the
	// kernel; nil forwards none.
	Meta json.RawMessage

	// Terminal, if set, advertises the terminal client capability to the downstream at
	// initialize; terminal/* is refused unless the session's controller implements
	// TerminalServer.
	Terminal bool
}

type sessionDriver struct {
	initMu   sync.Mutex
	initConn *libacp.ClientSideConnection
	initResp libacp.InitializeResponse

	mu       sync.Mutex
	sessions map[libacp.SessionID]*driveSession
}

func newSessionDriver() *sessionDriver {
	return &sessionDriver{sessions: make(map[libacp.SessionID]*driveSession)}
}

type driveSession struct {
	mu sync.Mutex

	cwd string

	configOptions  []libacp.SessionConfigOption
	configReceived bool

	modeState     *libacp.SessionModeState
	modeReceived  bool
	pendingModeID string

	modelState *libacp.SessionModelState

	commands []libacp.AvailableCommand
}

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

func (sd *sessionDriver) peek(sid libacp.SessionID) *driveSession {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	return sd.sessions[sid]
}

func (sd *sessionDriver) drop(sid libacp.SessionID) {
	sd.mu.Lock()
	delete(sd.sessions, sid)
	sd.mu.Unlock()
}

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

func (sd *sessionDriver) owns(sid libacp.SessionID) bool {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	_, ok := sd.sessions[sid]
	return ok
}

func (sd *sessionDriver) cwd(sid libacp.SessionID) string {
	if ds := sd.peek(sid); ds != nil {
		return ds.getCwd()
	}
	return ""
}

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

func (ds *driveSession) setCwd(cwd string) {
	ds.mu.Lock()
	ds.cwd = cwd
	ds.mu.Unlock()
}

func (ds *driveSession) getCwd() string {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return ds.cwd
}

func (ds *driveSession) applyConfigOptions(opts []libacp.SessionConfigOption) {
	ds.mu.Lock()
	ds.configReceived = true
	ds.configOptions = opts
	ds.mu.Unlock()
}

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

func (ds *driveSession) applyModel(modelID string) {
	ds.mu.Lock()
	if ds.modelState != nil {
		ds.modelState.CurrentModelID = modelID
	}
	ds.mu.Unlock()
}

func (ds *driveSession) snapshotConfigOptions() []libacp.SessionConfigOption {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return ds.buildConfigOptionsLocked()
}

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

func (ds *driveSession) availableCommands() []libacp.AvailableCommand {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return append([]libacp.AvailableCommand(nil), ds.commands...)
}

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

func (i *instance) cancelSession(sid libacp.SessionID) error {
	conn := i.conn()
	if conn == nil {
		return errNoConn
	}
	return conn.CancelPrompt(sid)
}

func (i *instance) closeSession(sid libacp.SessionID) error {
	if conn := i.conn(); conn != nil {
		_ = conn.CancelSession(libacp.CancelNotification{SessionID: sid})
	}
	i.driver.drop(sid)
	i.hub.closeSession(sid)
	return nil
}

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

func (i *instance) sessionConfigOptions(sid libacp.SessionID) []libacp.SessionConfigOption {
	ds := i.driver.peek(sid)
	if ds == nil {
		return nil
	}
	return ds.snapshotConfigOptions()
}

func (i *instance) availableCommands(sid libacp.SessionID) []libacp.AvailableCommand {
	ds := i.driver.peek(sid)
	if ds == nil {
		return nil
	}
	return ds.availableCommands()
}
