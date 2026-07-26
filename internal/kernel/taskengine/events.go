package taskengine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/beam/internal/libbus"
	"github.com/contenox/beam/internal/libtracker"
)

const TaskEventSubjectAll = "taskengine.events"

type TaskEventKind string

const (
	TaskEventChainStarted   TaskEventKind = "chain_started"
	TaskEventStepStarted    TaskEventKind = "step_started"
	TaskEventStepChunk      TaskEventKind = "step_chunk"
	TaskEventStepCompleted  TaskEventKind = "step_completed"
	TaskEventStepFailed     TaskEventKind = "step_failed"
	TaskEventChainCompleted TaskEventKind = "chain_completed"
	TaskEventChainFailed    TaskEventKind = "chain_failed"

	TaskEventApprovalRequested TaskEventKind = "approval_requested"
	TaskEventHITLDecision      TaskEventKind = "hitl_decision"
	TaskEventToolCallPending   TaskEventKind = "tool_call_pending"
	TaskEventToolCall          TaskEventKind = "tool_call"
	TaskEventPrint             TaskEventKind = "print"
	TaskEventTokenUsage        TaskEventKind = "token_usage"
)

type TaskEvent struct {
	Kind         TaskEventKind `json:"kind"`
	Timestamp    time.Time     `json:"timestamp"`
	RequestID    string        `json:"request_id,omitempty"`
	ChainID      string        `json:"chain_id,omitempty"`
	TaskID       string        `json:"task_id,omitempty"`
	TaskHandler  string        `json:"task_handler,omitempty"`
	Retry        int           `json:"retry"`
	ModelName    string        `json:"model_name,omitempty"`
	ProviderType string        `json:"provider_type,omitempty"`
	BackendID    string        `json:"backend_id,omitempty"`
	OutputType   string        `json:"output_type,omitempty"`
	Transition   string        `json:"transition,omitempty"`
	Content      string        `json:"content,omitempty"`
	Thinking     string        `json:"thinking,omitempty"`
	Error        string        `json:"error,omitempty"`

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
}

type TaskEventScope struct {
	ChainID     string
	TaskID      string
	TaskHandler string
	Retry       int
}

type TaskEventSink interface {
	PublishTaskEvent(ctx context.Context, event TaskEvent) error
	Enabled() bool
}

type NoopTaskEventSink struct{}

func (NoopTaskEventSink) PublishTaskEvent(context.Context, TaskEvent) error { return nil }
func (NoopTaskEventSink) Enabled() bool                                     { return false }

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

func (s *BusTaskEventSink) Enabled() bool {
	return s != nil && s.bus != nil
}

func (s *BusTaskEventSink) PublishTaskEvent(ctx context.Context, event TaskEvent) error {
	if !s.Enabled() {
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
	}
	return event
}

func publishTaskEventBestEffort(ctx context.Context, tracker libtracker.ActivityTracker, sink TaskEventSink, event TaskEvent) {
	if sink == nil || !sink.Enabled() {
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
