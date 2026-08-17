package contenoxcli

import (
	"context"
	"errors"
	"sync"

	"github.com/contenox/contenox/internal/relaylink"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/librelay"
	"github.com/contenox/contenox/libtracker"
)

type sessionGate struct {
	mu   sync.Mutex
	refs int
}

type sessionGates struct {
	mu    sync.Mutex
	gates map[string]*sessionGate
}

func (g *sessionGates) enter(key string) func() {
	if g == nil || key == "" {
		return func() {}
	}
	g.mu.Lock()
	if g.gates == nil {
		g.gates = map[string]*sessionGate{}
	}
	gate := g.gates[key]
	if gate == nil {
		gate = &sessionGate{}
		g.gates[key] = gate
	}
	gate.refs++
	g.mu.Unlock()

	gate.mu.Lock()
	return func() {
		gate.mu.Unlock()
		g.mu.Lock()
		defer g.mu.Unlock()
		gate.refs--
		if gate.refs == 0 {
			delete(g.gates, key)
		}
	}
}

func (g *sessionGates) idle() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.gates) == 0
}

type checkpointRowReader interface {
	GetChainCheckpoint(ctx context.Context, id string) (*runtimetypes.ChainCheckpoint, error)
}

type resumeRun func(ctx context.Context, approvalID string) (*agentservice.PromptResponse, error)

type relayResumeBridge struct {
	tracker libtracker.ActivityTracker
	runs    sessionGates

	mu       sync.Mutex
	instance string
	send     func(librelay.Frame) error
}

func newRelayResumeBridge(tracker libtracker.ActivityTracker) *relayResumeBridge {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &relayResumeBridge{tracker: tracker}
}

func (b *relayResumeBridge) attach(instance string, send func(librelay.Frame) error) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.instance, b.send = instance, send
	b.mu.Unlock()
}

func (b *relayResumeBridge) detach() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.send = nil
	b.mu.Unlock()
}

func (b *relayResumeBridge) link() (string, func(librelay.Frame) error) {
	if b == nil {
		return "", nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.instance, b.send
}

func (b *relayResumeBridge) enterSession(sessionID string) func() {
	if b == nil {
		return func() {}
	}
	return b.runs.enter(sessionID)
}

func (b *relayResumeBridge) hook(deps agentservice.Deps) hitlservice.ResumeHook {
	var store checkpointRowReader
	if deps.DB != nil {
		store = runtimetypes.New(deps.DB.WithoutTransaction())
	}
	return b.hookWith(store, func(ctx context.Context, approvalID string) (*agentservice.PromptResponse, error) {
		return agentservice.ResumeFromCheckpoint(ctx, deps, approvalID)
	})
}

func (b *relayResumeBridge) hookWith(store checkpointRowReader, resume resumeRun) hitlservice.ResumeHook {
	return func(ctx context.Context, approvalID string) error {
		requestID, sessionID := owedTriggerOutcome(ctx, store, approvalID)
		release := b.enterSession(sessionID)
		defer release()

		resp, err := resume(ctx, approvalID)
		if errors.Is(err, agentservice.ErrNoCheckpoint) {
			return hitlservice.ErrNoCheckpoint
		}
		b.reportResumed(ctx, requestID, resp, err)
		return err
	}
}

func owedTriggerOutcome(ctx context.Context, store checkpointRowReader, approvalID string) (requestID, sessionID string) {
	if store == nil {
		return "", ""
	}
	cp, err := store.GetChainCheckpoint(ctx, approvalID)
	if err != nil {
		return "", ""
	}
	return agentservice.TriggerRequestIDOf(cp), cp.SessionID
}

func (b *relayResumeBridge) reportResumed(ctx context.Context, requestID string, resp *agentservice.PromptResponse, err error) {
	if b == nil || requestID == "" {
		return
	}
	var outcome relayChainOutcome
	if resp != nil && resp.StopReason == agentservice.StopSuspended {
		outcome = relayChainOutcome{Suspended: true, ApprovalID: resp.SuspendedApprovalID}
	}
	status, msg := chainTriggerOutcome(outcome, err)
	b.report(ctx, requestID, status, msg)
}

func (b *relayResumeBridge) report(ctx context.Context, requestID, status, msg string) {
	instance, send := b.link()
	if send == nil {
		b.reportUndelivered(ctx, requestID, status, errors.New("no relay link is attached"))
		return
	}
	f, err := librelay.Frame{Type: librelay.TypeChainTriggerResult, Instance: instance}.
		WithPayload(librelay.ChainTriggerResult{RequestID: requestID, Status: status, Error: msg})
	if err == nil {
		err = send(f)
	}
	if err != nil {
		b.reportUndelivered(ctx, requestID, status, err)
		return
	}
	_, reportChange, end := b.tracker.Start(ctx, "settle", librelay.TypeChainTriggerResult, "request_id", requestID)
	reportChange(requestID, status)
	end()
}

func (b *relayResumeBridge) reportUndelivered(ctx context.Context, requestID, status string, cause error) {
	if errors.Is(cause, relaylink.ErrNotConnected) || errors.Is(cause, relaylink.ErrClosed) {
		cause = errors.New("the relay link is down")
	}
	reportErr, _, end := b.tracker.Start(ctx, "deliver", librelay.TypeChainTriggerResult,
		"request_id", requestID, "status", status)
	reportErr(cause)
	end()
}
