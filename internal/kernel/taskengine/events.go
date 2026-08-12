package taskengine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/libtracker"
)

const TaskEventSubjectAll = "taskengine.events"

type TaskEventKind string

const (
	TaskEventChainStarted  TaskEventKind = "chain_started"
	TaskEventStepStarted   TaskEventKind = "step_started"
	TaskEventStepChunk     TaskEventKind = "step_chunk"
	TaskEventStepStreamEnd TaskEventKind = "step_stream_end"
	TaskEventStepCompleted TaskEventKind = "step_completed"
	TaskEventStepFailed    TaskEventKind = "step_failed"

	TaskEventChainCompleted TaskEventKind = "chain_completed"
	TaskEventChainFailed    TaskEventKind = "chain_failed"
	// TaskEventChainSuspended terminates a run segment parked on a human approval past the fast window; carries the interrupt address ({chain, task, tool_call}) and approval_id.
	TaskEventChainSuspended TaskEventKind = "chain_suspended"

	TaskEventApprovalRequested TaskEventKind = "approval_requested"
	TaskEventHITLDecision      TaskEventKind = "hitl_decision"
	TaskEventToolCallPending   TaskEventKind = "tool_call_pending"
	TaskEventToolCall          TaskEventKind = "tool_call"
	TaskEventPrint             TaskEventKind = "print"
	TaskEventTokenUsage        TaskEventKind = "token_usage"
)

// AllTaskEventKinds enumerates every kind the engine can emit.
func AllTaskEventKinds() []TaskEventKind {
	return []TaskEventKind{
		TaskEventChainStarted,
		TaskEventStepStarted,
		TaskEventStepChunk,
		TaskEventStepStreamEnd,
		TaskEventStepCompleted,
		TaskEventStepFailed,
		TaskEventChainCompleted,
		TaskEventChainFailed,
		TaskEventChainSuspended,
		TaskEventApprovalRequested,
		TaskEventHITLDecision,
		TaskEventToolCallPending,
		TaskEventToolCall,
		TaskEventPrint,
		TaskEventTokenUsage,
	}
}

// EventScope is the hierarchical address (chain/task/tool_call) of an event or captured state unit.
type EventScope struct {
	Chain    string `json:"chain,omitempty"`
	Task     string `json:"task,omitempty"`
	ToolCall string `json:"tool_call,omitempty"`
}

type TaskEvent struct {
	Kind      TaskEventKind `json:"kind"`
	Timestamp time.Time     `json:"timestamp"`
	RequestID string        `json:"request_id,omitempty"`
	// Scope is the event's hierarchical address (chain/task/tool-call); additive on the wire alongside the legacy flat fields below.
	Scope        EventScope `json:"scope,omitzero"`
	ChainID      string     `json:"chain_id,omitempty"`
	TaskID       string     `json:"task_id,omitempty"`
	TaskHandler  string     `json:"task_handler,omitempty"`
	Retry        int        `json:"retry"`
	ModelName    string     `json:"model_name,omitempty"`
	ProviderType string     `json:"provider_type,omitempty"`
	BackendID    string     `json:"backend_id,omitempty"`
	OutputType   string     `json:"output_type,omitempty"`
	Transition   string     `json:"transition,omitempty"`
	Content      string     `json:"content,omitempty"`
	Thinking     string     `json:"thinking,omitempty"`
	Error        string     `json:"error,omitempty"`

	ApprovalID   string         `json:"approval_id,omitempty"`
	HookName     string         `json:"hook_name,omitempty"`
	ToolName     string         `json:"tool_name,omitempty"`
	ApprovalArgs map[string]any `json:"approval_args,omitempty"`
	ApprovalDiff string         `json:"approval_diff,omitempty"`

	HITLAction            string `json:"hitl_action,omitempty"`
	HITLReason            string `json:"hitl_reason,omitempty"`
	HITLPolicyName        string `json:"hitl_policy_name,omitempty"`
	HITLPolicyPath        string `json:"hitl_policy_path,omitempty"`
	HITLArgsSummary       string `json:"hitl_args_summary,omitempty"`
	HITLMatchedRule       *int   `json:"hitl_matched_rule,omitempty"`
	HITLTimeoutS          int    `json:"hitl_timeout_s,omitempty"`
	HITLApprovalRequested *bool  `json:"hitl_approval_requested,omitempty"`

	ToolDiffPath    string `json:"tool_diff_path,omitempty"`
	ToolDiffOldText string `json:"tool_diff_old_text,omitempty"`
	ToolDiffNewText string `json:"tool_diff_new_text,omitempty"`

	// For token_usage
	TokenUsed int `json:"token_used,omitempty"`
	TokenSize int `json:"token_size,omitempty"`

	// step_stream_end bracket: ChunkCount counts step_chunk parcels seen, FinishReason is the provider's verbatim reason, Usage is provider-reported token usage.
	ChunkCount   int         `json:"chunk_count,omitempty"`
	FinishReason string      `json:"finish_reason,omitempty"`
	Usage        *TokenUsage `json:"usage,omitempty"`
}

type TaskEventScope struct {
	ChainID     string
	TaskID      string
	TaskHandler string
	Retry       int
}

// TaskEventSink receives engine observation events; Wants gates whether events are built and published but must never select an execution path.
type TaskEventSink interface {
	PublishTaskEvent(ctx context.Context, event TaskEvent) error
	Wants(kind TaskEventKind) bool
}

type NoopTaskEventSink struct{}

func (NoopTaskEventSink) PublishTaskEvent(context.Context, TaskEvent) error { return nil }
func (NoopTaskEventSink) Wants(TaskEventKind) bool                          { return false }

type BusTaskEventSink struct {
	bus            libbus.Messenger
	publishTimeout time.Duration
}

func NewBusTaskEventSink(bus libbus.Messenger) *BusTaskEventSink {
	return &BusTaskEventSink{
		bus:            bus,
		publishTimeout: 100 * time.Millisecond,
	}
}

func TaskEventRequestSubject(requestID string) string {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return TaskEventSubjectAll
	}
	return TaskEventSubjectAll + ".request." + requestID
}

// Wants accepts every kind while a bus is attached.
func (s *BusTaskEventSink) Wants(TaskEventKind) bool {
	return s != nil && s.bus != nil
}

func (s *BusTaskEventSink) PublishTaskEvent(ctx context.Context, event TaskEvent) error {
	if !s.Wants(event.Kind) {
		return nil
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal task event: %w", err)
	}

	subjects := []string{TaskEventSubjectAll}
	if event.RequestID != "" {
		subjects = append(subjects, TaskEventRequestSubject(event.RequestID))
	}

	var firstErr error
	for _, subject := range subjects {
		publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.publishTimeout)
		err := s.bus.Publish(publishCtx, subject, payload)
		cancel()
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("publish task event to %s: %w", subject, err)
		}
	}
	return firstErr
}

type taskEventSinkContextKey struct{}
type taskEventScopeContextKey struct{}

func WithTaskEventSink(ctx context.Context, sink TaskEventSink) context.Context {
	if sink == nil {
		sink = NoopTaskEventSink{}
	}
	return context.WithValue(ctx, taskEventSinkContextKey{}, sink)
}

func taskEventSinkFromContext(ctx context.Context) TaskEventSink {
	if ctx == nil {
		return NoopTaskEventSink{}
	}
	sink, ok := ctx.Value(taskEventSinkContextKey{}).(TaskEventSink)
	if !ok || sink == nil {
		return NoopTaskEventSink{}
	}
	return sink
}

func WithTaskEventScope(ctx context.Context, scope TaskEventScope) context.Context {
	return context.WithValue(ctx, taskEventScopeContextKey{}, scope)
}

func taskEventScopeFromContext(ctx context.Context) (TaskEventScope, bool) {
	if ctx == nil {
		return TaskEventScope{}, false
	}
	scope, ok := ctx.Value(taskEventScopeContextKey{}).(TaskEventScope)
	return scope, ok
}

// NewTaskEvent builds an event of the given kind addressed from ctx (request ID, chain/task scope, and tool-call address when present).
func NewTaskEvent(ctx context.Context, kind TaskEventKind) TaskEvent {
	event := TaskEvent{
		Kind:      kind,
		Timestamp: time.Now().UTC(),
	}
	if reqID, ok := ctx.Value(libtracker.ContextKeyRequestID).(string); ok {
		event.RequestID = reqID
	}
	if scope, ok := taskEventScopeFromContext(ctx); ok {
		event.ChainID = scope.ChainID
		event.TaskID = scope.TaskID
		event.TaskHandler = scope.TaskHandler
		event.Retry = scope.Retry
		event.Scope.Chain = scope.ChainID
		event.Scope.Task = scope.TaskID
	}
	if callID, ok := ctx.Value(ContextKeyToolCallID).(string); ok && callID != "" {
		event.Scope.ToolCall = callID
	}
	return event
}

func publishTaskEventBestEffort(ctx context.Context, tracker libtracker.ActivityTracker, sink TaskEventSink, event TaskEvent) {
	if sink == nil || !sink.Wants(event.Kind) {
		return
	}
	if err := sink.PublishTaskEvent(ctx, event); err != nil {
		if tracker == nil {
			tracker = libtracker.NoopTracker{}
		}
		reportErr, _, end := tracker.Start(ctx, "publish", "task_event", "kind", string(event.Kind))
		reportErr(err)
		end()
	}
}
