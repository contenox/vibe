// Package hitlservice evaluates approval policies before shell commands and tool calls execute.
// Each action pauses here and waits for an approve or deny decision before the chain continues.
package hitlservice

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/contenox/contenox/runtime/vfsservice"
	"github.com/google/uuid"
)

// KVReader is the minimal KV interface the HITL service needs to resolve the active policy name.
// runtimetypes.Store satisfies this interface.
type KVReader interface {
	GetKV(ctx context.Context, key string, out interface{}) error
}

// PolicyEvaluator determines the action for a tool call based on the loaded policy.
// args are the parsed tool call arguments and are used to evaluate When conditions.
// It is the minimal interface consumed by localtools.HITLWrapper.
type PolicyEvaluator interface {
	Evaluate(ctx context.Context, toolsName, toolName string, args map[string]any) (EvaluationResult, error)
}

// Service extends PolicyEvaluator with approval gate management for the server path.
type Service interface {
	PolicyEvaluator

	// RequestApproval emits a TaskEventApprovalRequested event on sink and blocks
	// until the human responds via Respond or ctx is cancelled.
	RequestApproval(ctx context.Context, req ApprovalRequest, sink taskengine.TaskEventSink) (bool, error)

	// Respond signals a pending approval request by approvalID.
	// Returns false when the ID is not found (already resolved, timed out, or invalid).
	Respond(approvalID string, approved bool) bool
}

type service struct {
	vfs     vfsservice.Service
	store   KVReader
	mu      sync.Mutex
	pending map[string]chan bool
	tracker libtracker.ActivityTracker
}

// New creates a Service. vfs is used to read the policy JSON file on every Evaluate call.
// store is used to look up the active policy name from KV (key: cli.hitl-policy-name).
// When no KV entry is set, the fallback policy file "hitl-policy-default.json" is used.
// runtimetypes.Store satisfies the KVReader interface.
func New(vfs vfsservice.Service, store KVReader, tracker libtracker.ActivityTracker) Service {
	return &service{
		vfs:     vfs,
		store:   store,
		pending: make(map[string]chan bool),
		tracker: tracker,
	}
}

var _ Service = (*service)(nil)

// readActivePolicyName reads the active HITL policy filename from KV.
// Uses the same key as clikv ("cli.hitl-policy-name") to stay consistent with
// the CLI config system. Returns "" when no policy has been selected.
func (s *service) readActivePolicyName(ctx context.Context) string {
	var val string
	if err := s.store.GetKV(ctx, kvPrefixHITLPolicy, &val); err != nil {
		return ""
	}
	return strings.TrimSpace(val)
}

const kvPrefixHITLPolicy = "cli.hitl-policy-name"

func (s *service) Evaluate(ctx context.Context, toolsName, toolName string, args map[string]any) (EvaluationResult, error) {
	reportErr, reportChange, end := s.tracker.Start(ctx, "hitl", "evaluate", "toolsName", toolsName, "toolName", toolName)
	defer end()
	policyPath := s.readActivePolicyName(ctx)
	if policyPath == "" {
		policyPath = "hitl-policy-default.json"
	}
	p, err := loadPolicy(ctx, s.vfs, policyPath)
	if err != nil {
		// Policy file absent or unreadable — fall back to built-in defaults so
		// deployments without a policy file keep the original HITL behaviour.
		reportErr(fmt.Errorf("hitl: falling back to built-in default policy: %w", err))
		p = defaultPolicy()
	}
	reportChange("policy", policyPath)
	return evaluate(p, toolsName, toolName, args), nil
}

func (s *service) RequestApproval(ctx context.Context, req ApprovalRequest, sink taskengine.TaskEventSink) (bool, error) {
	approvalID := uuid.NewString()

	// Register channel before publishing the event to avoid a race where the
	// frontend responds before we start listening.
	ch := make(chan bool, 1)
	s.mu.Lock()
	s.pending[approvalID] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, approvalID)
		s.mu.Unlock()
	}()

	ev := taskengine.NewTaskEvent(ctx, taskengine.TaskEventApprovalRequested)
	ev.ApprovalID = approvalID
	ev.ToolsName = req.ToolsName
	ev.ToolName = req.ToolName
	ev.ApprovalArgs = req.Args
	ev.ApprovalDiff = req.Diff
	if err := sink.PublishTaskEvent(ctx, ev); err != nil {
		slog.Warn("hitl: failed to publish approval_requested event", "error", err)
	}

	select {
	case approved := <-ch:
		return approved, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (s *service) Respond(approvalID string, approved bool) bool {
	s.mu.Lock()
	ch, ok := s.pending[approvalID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- approved:
		return true
	default:
		// Channel already has a value — duplicate response, ignore.
		return false
	}
}
