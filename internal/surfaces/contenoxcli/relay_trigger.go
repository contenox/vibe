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
	RunChain(ctx context.Context, req relayChainRequest) error
}

type relayChainRequest struct {
	Chain       string
	Policy      string
	SessionMode string
	SessionName string
	Input       json.RawMessage
}

type relayChainTriggers struct {
	runner      relayChainRunner
	unavailable string
}

func buildRelayChainTriggers(db libdbexec.DBManager, contenoxDir, workspaceID string, engine *Engine, opts chatOpts) relayChainTriggers {
	if engine == nil {
		return relayChainTriggers{unavailable: "no engine is running in this process; configure a default model and restart"}
	}
	return relayChainTriggers{runner: &relayTriggerRunner{
		agent: agentservice.New(agentservice.Deps{
			Engine:      engine,
			DB:          db,
			WorkspaceID: workspaceID,
			Identity:    acpsvc.ClientIdentity,
		}),
		opts:        opts,
		contenoxDir: contenoxDir,
	}}
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
	case req.Chain == "":
		refusal = "chain is required"
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
		status, msg := librelay.ChainTriggerStatusOK, ""
		if err := h.triggers.runner.RunChain(runCtx, relayChainRequest{
			Chain:       req.Chain,
			Policy:      req.Policy,
			SessionMode: req.SessionMode,
			SessionName: strings.TrimSpace(req.SessionName),
			Input:       req.Input,
		}); err != nil {
			status = librelay.ChainTriggerStatusError
			if errors.Is(err, errChainTriggerRefused) {
				status = librelay.ChainTriggerStatusRefused
			}
			msg = err.Error()
		}
		h.sendResult(ctx, f, req.RequestID, status, msg)
	}()
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
	if status == librelay.ChainTriggerStatusOK {
		reportChange(requestID, status)
	} else {
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

type relayTriggerRunner struct {
	agent       agentservice.Agent
	opts        chatOpts
	contenoxDir string

	mu        sync.Mutex
	inSession map[string]*triggerSessionGate
}

type triggerSessionGate struct {
	mu   sync.Mutex
	refs int
}

func (r *relayTriggerRunner) enterSession(name string) func() {
	r.mu.Lock()
	if r.inSession == nil {
		r.inSession = map[string]*triggerSessionGate{}
	}
	gate := r.inSession[name]
	if gate == nil {
		gate = &triggerSessionGate{}
		r.inSession[name] = gate
	}
	gate.refs++
	r.mu.Unlock()

	gate.mu.Lock()
	return func() {
		gate.mu.Unlock()
		r.mu.Lock()
		defer r.mu.Unlock()
		gate.refs--
		if gate.refs == 0 {
			delete(r.inSession, name)
		}
	}
}

func (r *relayTriggerRunner) sessionNameFor(req relayChainRequest) (string, error) {
	if req.SessionMode != librelay.ChainSessionReused {
		return triggerSessionPrefix + uuid.NewString(), nil
	}
	name := req.SessionName
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(req.Chain), ".json")
	}
	if !validChainSessionName(name) {
		return "", fmt.Errorf("cannot name a reused session for chain %q", req.Chain)
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

func (r *relayTriggerRunner) RunChain(ctx context.Context, req relayChainRequest) error {
	path, err := lookupSystemFile(r.contenoxDir, req.Chain)
	if err != nil {
		return refuseChainTrigger(err)
	}
	chain, err := loadChainFromFile(path)
	if err != nil {
		return refuseChainTrigger(err)
	}
	var input map[string]any
	if err := json.Unmarshal(req.Input, &input); err != nil {
		return refuseChainTrigger(fmt.Errorf("input is not a JSON object: %w", err))
	}
	// An envelope past the hop budget is refused before the chain; a run that
	// proceeds executes at hop+1.
	var envelope struct {
		Hop int `json:"hop"`
	}
	if err := json.Unmarshal(req.Input, &envelope); err != nil {
		return refuseChainTrigger(fmt.Errorf("input hop is not an integer: %w", err))
	}
	if envelope.Hop > eventtrigger.DefaultMaxHop {
		return refuseChainTrigger(fmt.Errorf("event hop %d exceeds limit %d", envelope.Hop, eventtrigger.DefaultMaxHop))
	}
	execCtx := runtimetypes.WithEventHop(ctx, envelope.Hop+1)
	if req.Policy != "" {
		execCtx = hitlservice.WithPolicyName(execCtx, req.Policy)
	}
	if cwd, cwdErr := os.Getwd(); cwdErr == nil {
		execCtx = vfs.WithSessionCwd(execCtx, cwd)
	}

	sessionName, err := r.sessionNameFor(req)
	if err != nil {
		return refuseChainTrigger(err)
	}
	release := r.enterSession(sessionName)
	defer release()
	contenoxSessionID, err := r.ensureSession(execCtx, req.SessionMode, sessionName)
	if err != nil {
		return err
	}

	_, err = r.agent.Prompt(execCtx, agentservice.PromptRequest{
		Input:         string(req.Input),
		InputValue:    input,
		InputType:     taskengine.DataTypeJSON,
		Chain:         chain,
		ChainRef:      path,
		SessionID:     contenoxSessionID,
		TemplateVars:  buildTemplateVars(r.opts),
		ContextLength: r.opts.EffectiveContext,
	})
	return err
}
