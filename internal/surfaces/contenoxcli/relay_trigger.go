package contenoxcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/eventtrigger"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/librelay"
	"github.com/contenox/contenox/libtracker"
)

var errChainTriggerRefused = errors.New("chain trigger refused")

func refuseChainTrigger(err error) error {
	return fmt.Errorf("%w: %w", errChainTriggerRefused, err)
}

type relayChainRunner interface {
	RunChain(ctx context.Context, req relayChainRequest) error
}

type relayChainRequest struct {
	Chain  string
	Policy string
	Input  json.RawMessage
}

type relayChainTriggers struct {
	runner      relayChainRunner
	unavailable string
}

func buildRelayChainTriggers(db libdbexec.DBManager, contenoxDir, workspaceID string, engine *Engine, opts chatOpts) relayChainTriggers {
	if engine == nil {
		return relayChainTriggers{unavailable: "no engine is running in this process; configure a default model and restart"}
	}
	if !opts.EffectiveOptInBeta {
		return relayChainTriggers{unavailable: "event triggers are beta-gated off on this machine (contenox config set opt-in-beta true)"}
	}
	return relayChainTriggers{runner: &relayTriggerRunner{
		agent: agentservice.New(agentservice.Deps{
			Engine:      engine,
			DB:          db,
			WorkspaceID: workspaceID,
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
	case req.SessionMode == librelay.ChainSessionReused:
		// Running "reused" as "new" would report a reuse that never happened, so the mode is refused instead.
		refusal = `session_mode "reused" is not supported by this build; use "new"`
	case req.SessionMode != "" && req.SessionMode != librelay.ChainSessionNew:
		refusal = fmt.Sprintf("unknown session_mode %q", req.SessionMode)
	}
	if refusal != "" {
		h.sendResult(ctx, f, req.RequestID, librelay.ChainTriggerStatusRefused, refusal)
		return
	}
	if !h.begin(req.RequestID) {
		// A re-delivered RequestID while the first delivery still owes its result: a second run would answer twice.
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer h.end(req.RequestID)
		// The frame's request_id becomes the run's correlation key: events the chain appends and records the run writes all carry it.
		runCtx := context.WithValue(ctx, libtracker.ContextKeyRequestID, req.RequestID)
		status, msg := librelay.ChainTriggerStatusOK, ""
		if err := h.triggers.runner.RunChain(runCtx, relayChainRequest{
			Chain:  req.Chain,
			Policy: req.Policy,
			Input:  req.Input,
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

type relayTriggerRunner struct {
	agent       agentservice.Agent
	opts        chatOpts
	contenoxDir string
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
	// An envelope past the hop budget is refused before the chain; a run that proceeds executes at hop+1 so events the chain appends inherit it.
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
	_, err = r.agent.Prompt(execCtx, agentservice.PromptRequest{
		Input:         string(req.Input),
		InputValue:    input,
		InputType:     taskengine.DataTypeJSON,
		Chain:         chain,
		ChainRef:      path,
		TemplateVars:  buildTemplateVars(r.opts),
		ContextLength: r.opts.EffectiveContext,
	})
	return err
}
