package contenoxcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/eventtrigger"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/librelay"
	"github.com/contenox/contenox/libtracker"
	"github.com/google/uuid"
)

var errChainTriggerRefused = errors.New("chain trigger refused")

func refuseChainTrigger(err error) error {
	return fmt.Errorf("%w: %w", errChainTriggerRefused, err)
}

type relayChainRunner interface {
	RunChain(ctx context.Context, req relayChainRequest) (relayChainOutcome, error)
}

type relayChainRequest struct {
	RequestID   string
	Chain       string
	AgentName   string
	Policy      string
	SessionMode string
	SessionName string
	Input       json.RawMessage
}

func (req relayChainRequest) target() string {
	if req.AgentName != "" {
		return fmt.Sprintf("agent %q", req.AgentName)
	}
	return fmt.Sprintf("chain %q", req.Chain)
}

type relayChainOutcome struct {
	Suspended  bool
	ApprovalID string
}

type relayChainTriggers struct {
	runner      relayChainRunner
	resumes     *relayResumeBridge
	unavailable string
}

func buildRelayChainTriggers(db libdbexec.DBManager, contenoxDir, workspaceID string, engine *Engine, opts chatOpts, resumes *relayResumeBridge) relayChainTriggers {
	if resumes == nil {
		resumes = newRelayResumeBridge(nil)
	}
	if engine == nil {
		return relayChainTriggers{
			resumes:     resumes,
			unavailable: "no engine is running in this process; configure a default model and restart",
		}
	}
	runner := &relayTriggerRunner{
		agent: agentservice.New(agentservice.Deps{
			Engine:      engine,
			DB:          db,
			WorkspaceID: workspaceID,
			Identity:    acpsvc.ClientIdentity,
		}),
		opts:        opts,
		contenoxDir: contenoxDir,
		tracker:     engine.Tracker,
		resumes:     resumes,
	}
	if db != nil {
		runner.agents = agentregistryservice.New(db)
		runner.store = runtimetypes.New(db.WithoutTransaction())
	}
	return relayChainTriggers{runner: runner, resumes: resumes}
}

type relayTriggerHandler struct {
	triggers relayChainTriggers
	tracker  libtracker.ActivityTracker

	instance string
	send     func(librelay.Frame) error

	mu       sync.Mutex
	inflight map[string]bool
	wg       sync.WaitGroup
}

func newRelayTriggerHandler(triggers relayChainTriggers, tracker libtracker.ActivityTracker) *relayTriggerHandler {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	if triggers.unavailable == "" {
		triggers.unavailable = "chain triggers are not available in this process"
	}
	return &relayTriggerHandler{
		triggers: triggers,
		tracker:  tracker,
		inflight: map[string]bool{},
	}
}

func (h *relayTriggerHandler) handle(ctx context.Context, f librelay.Frame) {
	if f.Instance != "" && f.Instance != h.instance {
		return
	}
	var req librelay.ChainTrigger
	err := f.DecodePayload(&req)
	if err == nil && !validChainRequestID(req.RequestID) {
		err = errors.New("missing or malformed request_id")
	}
	if err != nil {
		reportErr, _, end := h.tracker.Start(ctx, "refuse", "chain_trigger")
		reportErr(err)
		end()
		if f.IsRequest() {
			_ = h.send(librelay.NewError(f, librelay.CodeMalformedFrame, "malformed chain_trigger payload"))
		}
		return
	}

	var refusal string
	switch {
	case h.triggers.runner == nil:
		refusal = h.triggers.unavailable
	case req.Chain == "" && req.AgentName == "":
		refusal = "chain or agent_name is required"
	case req.AgentName != "" && !validChainAgentName(req.AgentName):
		refusal = "malformed agent_name"
	case req.SessionMode != "" && req.SessionMode != librelay.ChainSessionNew && req.SessionMode != librelay.ChainSessionReused:
		refusal = fmt.Sprintf("unknown session_mode %q", req.SessionMode)
	case req.SessionName != "" && !validChainSessionName(req.SessionName):
		refusal = "malformed session_name"
	}
	if refusal != "" {
		h.sendResult(ctx, f, req.RequestID, librelay.ChainTriggerStatusRefused, refusal)
		return
	}
	if !h.begin(req.RequestID) {
		// The first delivery still owes its result; a second run would answer twice.
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer h.end(req.RequestID)
		// The frame's request_id becomes the run's correlation key.
		runCtx := context.WithValue(ctx, libtracker.ContextKeyRequestID, req.RequestID)
		outcome, err := h.triggers.runner.RunChain(runCtx, relayChainRequest{
			RequestID:   req.RequestID,
			Chain:       req.Chain,
			AgentName:   strings.TrimSpace(req.AgentName),
			Policy:      req.Policy,
			SessionMode: req.SessionMode,
			SessionName: strings.TrimSpace(req.SessionName),
			Input:       req.Input,
		})
		status, msg := chainTriggerOutcome(outcome, err)
		h.sendResult(ctx, f, req.RequestID, status, msg)
	}()
}

func chainTriggerOutcome(outcome relayChainOutcome, err error) (status, msg string) {
	switch {
	case err != nil:
		if errors.Is(err, errChainTriggerRefused) {
			return librelay.ChainTriggerStatusRefused, err.Error()
		}
		return librelay.ChainTriggerStatusError, err.Error()
	case outcome.Suspended:
		msg = "awaiting a human verdict"
		if outcome.ApprovalID != "" {
			msg += " on approval " + outcome.ApprovalID
		}
		return librelay.ChainTriggerStatusAwaitingHuman, msg
	}
	return librelay.ChainTriggerStatusOK, ""
}

func (h *relayTriggerHandler) wait() { h.wg.Wait() }

func (h *relayTriggerHandler) begin(requestID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inflight[requestID] {
		return false
	}
	h.inflight[requestID] = true
	return true
}

func (h *relayTriggerHandler) end(requestID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.inflight, requestID)
}

func (h *relayTriggerHandler) sendResult(ctx context.Context, trigger librelay.Frame, requestID, status, msg string) {
	reportErr, reportChange, end := h.tracker.Start(ctx, "fire", "relay_chain_trigger", "request_id", requestID)
	switch status {
	case librelay.ChainTriggerStatusOK:
		reportChange(requestID, status)
	case librelay.ChainTriggerStatusAwaitingHuman:
		reportChange(requestID, map[string]any{"status": status, "detail": msg})
	default:
		reportErr(fmt.Errorf("chain trigger %s: %s", status, msg))
	}
	end()
	res := librelay.Frame{
		Type:     librelay.TypeChainTriggerResult,
		Instance: h.instance,
		ReplyTo:  trigger.ID,
		Trace:    trigger.Trace,
	}
	res, err := res.WithPayload(librelay.ChainTriggerResult{RequestID: requestID, Status: status, Error: msg})
	if err == nil {
		err = h.send(res)
	}
	if err != nil {
		reportErr, _, end := h.tracker.Start(ctx, "deliver", "chain_trigger_result", "request_id", requestID)
		reportErr(err)
		end()
	}
}

func validChainRequestID(id string) bool {
	if id == "" || len(id) > librelay.MaxIDBytes || !utf8.ValidString(id) {
		return false
	}
	return !strings.ContainsFunc(id, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

const triggerSessionPrefix = "trigger-"

const maxChainSessionNameBytes = 255 - len(triggerSessionPrefix)

func validChainSessionName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxChainSessionNameBytes || !utf8.ValidString(name) {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

func validChainAgentName(name string) bool { return validChainSessionName(name) }

type relayTriggerRunner struct {
	agent       agentservice.Agent
	opts        chatOpts
	contenoxDir string
	agents      agentregistryservice.Service
	store       runtimetypes.Store
	tracker     libtracker.ActivityTracker
	resumes     *relayResumeBridge

	names sessionGates

	discoverMu sync.Mutex
}

func (r *relayTriggerRunner) enterSession(name string) func() {
	return r.names.enter(name)
}

func (r *relayTriggerRunner) sessionNameFor(req relayChainRequest) (string, error) {
	if req.SessionMode != librelay.ChainSessionReused {
		return triggerSessionPrefix + uuid.NewString(), nil
	}
	name := req.SessionName
	if name == "" {
		name = req.AgentName
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(req.Chain), ".json")
	}
	if !validChainSessionName(name) {
		return "", fmt.Errorf("cannot name a reused session for %s", req.target())
	}
	return triggerSessionPrefix + strings.TrimSpace(name), nil
}

func (r *relayTriggerRunner) ensureSession(ctx context.Context, mode, name string) (string, error) {
	if mode == librelay.ChainSessionReused {
		sessions, err := r.agent.SessionList(ctx)
		if err != nil {
			return "", refuseChainTrigger(fmt.Errorf("list sessions: %w", err))
		}
		for _, s := range sessions {
			if s != nil && s.Name == name {
				return s.ID, nil
			}
		}
	}
	id, err := r.agent.SessionNew(ctx, name)
	if err != nil {
		return "", refuseChainTrigger(fmt.Errorf("create session %q: %w", name, err))
	}
	return id, nil
}

func (r *relayTriggerRunner) resolveChainPath(ctx context.Context, req relayChainRequest) (string, error) {
	if req.AgentName == "" {
		return lookupSystemFile(r.contenoxDir, req.Chain)
	}
	if r.agents == nil {
		return "", fmt.Errorf("agent %q cannot be resolved: this process runs no agent registry", req.AgentName)
	}

	r.discoverMu.Lock()
	discoverChainAgents(ctx, r.agents, r.contenoxDir, r.tracker, DiscoverDeps{Store: r.store})
	r.discoverMu.Unlock()

	agent, err := agentregistryservice.ResolveForSpawn(ctx, r.agents, req.AgentName)
	if err != nil {
		if errors.Is(err, libdbexec.ErrNotFound) {
			return "", fmt.Errorf("no agent named %q is declared on this machine: %w", req.AgentName, err)
		}
		return "", err
	}
	if agent.Kind != runtimetypes.AgentKindChain {
		return "", fmt.Errorf("agent %q is kind %q; a trigger runs a declared chain agent", req.AgentName, agent.Kind)
	}
	cfg, err := agent.ChainConfig()
	if err != nil {
		return "", fmt.Errorf("agent %q: %w", req.AgentName, err)
	}
	if err := cfg.Validate(); err != nil {
		return "", fmt.Errorf("agent %q: %w", req.AgentName, err)
	}
	return cfg.Path, nil
}

func (r *relayTriggerRunner) RunChain(ctx context.Context, req relayChainRequest) (relayChainOutcome, error) {
	path, err := r.resolveChainPath(ctx, req)
	if err != nil {
		return relayChainOutcome{}, refuseChainTrigger(err)
	}
	chain, err := loadChainFromFile(path)
	if err != nil {
		return relayChainOutcome{}, refuseChainTrigger(err)
	}
	var input map[string]any
	if err := json.Unmarshal(req.Input, &input); err != nil {
		return relayChainOutcome{}, refuseChainTrigger(fmt.Errorf("input is not a JSON object: %w", err))
	}
	// An envelope past the hop budget is refused before the chain; a run that
	// proceeds executes at hop+1.
	var envelope struct {
		Hop int `json:"hop"`
	}
	if err := json.Unmarshal(req.Input, &envelope); err != nil {
		return relayChainOutcome{}, refuseChainTrigger(fmt.Errorf("input hop is not an integer: %w", err))
	}
	if envelope.Hop > eventtrigger.DefaultMaxHop {
		return relayChainOutcome{}, refuseChainTrigger(fmt.Errorf("event hop %d exceeds limit %d", envelope.Hop, eventtrigger.DefaultMaxHop))
	}
	execCtx := runtimetypes.WithEventHop(ctx, envelope.Hop+1)
	if req.Policy != "" {
		execCtx = hitlservice.WithPolicyName(execCtx, req.Policy)
	}
	execCtx = hitlservice.WithAgentName(execCtx, req.AgentName)
	execCtx = agentservice.WithTriggerRequestID(execCtx, req.RequestID)
	if cwd, cwdErr := os.Getwd(); cwdErr == nil {
		execCtx = vfs.WithSessionCwd(execCtx, cwd)
	}

	sessionName, err := r.sessionNameFor(req)
	if err != nil {
		return relayChainOutcome{}, refuseChainTrigger(err)
	}
	release := r.enterSession(sessionName)
	defer release()
	contenoxSessionID, err := r.ensureSession(execCtx, req.SessionMode, sessionName)
	if err != nil {
		return relayChainOutcome{}, err
	}
	releaseRun := r.resumes.enterSession(contenoxSessionID)
	defer releaseRun()

	resp, err := r.agent.Prompt(execCtx, agentservice.PromptRequest{
		Input:         string(req.Input),
		InputValue:    input,
		InputType:     taskengine.DataTypeJSON,
		Chain:         chain,
		ChainRef:      path,
		SessionID:     contenoxSessionID,
		TemplateVars:  buildTemplateVars(r.opts),
		ContextLength: r.opts.EffectiveContext,
	})
	if err != nil {
		return relayChainOutcome{}, err
	}
	if resp != nil && resp.StopReason == agentservice.StopSuspended {
		return relayChainOutcome{Suspended: true, ApprovalID: resp.SuspendedApprovalID}, nil
	}
	return relayChainOutcome{}, nil
}
