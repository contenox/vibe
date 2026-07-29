package vscodeagent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

type bridgeEventSink struct {
	server *Server
}

func (s *bridgeEventSink) Enabled() bool { return s != nil && s.server != nil }

// Wants reports the kinds publishTaskEvent maps to a bridge notification.
func (s *bridgeEventSink) Wants(kind taskengine.TaskEventKind) bool {
	if !s.Enabled() {
		return false
	}
	switch kind {
	case taskengine.TaskEventStepChunk,
		taskengine.TaskEventPrint,
		taskengine.TaskEventToolCallPending,
		taskengine.TaskEventToolCall,
		taskengine.TaskEventHITLDecision,
		taskengine.TaskEventStepStarted,
		taskengine.TaskEventStepCompleted,
		taskengine.TaskEventStepFailed,
		taskengine.TaskEventTokenUsage:
		return true
	default:
		return false
	}
}

func (s *bridgeEventSink) PublishTaskEvent(ctx context.Context, ev taskengine.TaskEvent) error {
	if !s.Wants(ev.Kind) {
		return nil
	}
	s.server.publishTaskEvent(ctx, ev)
	return nil
}

func (s *Server) publishTaskEvent(ctx context.Context, ev taskengine.TaskEvent) {
	turn, ok := s.turnByRequestID(ev.RequestID)
	if !ok {
		return
	}
	switch ev.Kind {
	case taskengine.TaskEventStepChunk:
		// Only a handler whose streamed output IS assistant narration reaches the
		// chat delta stream — the same judgement acpsvc applies, owned by the
		// package that owns the handler vocabulary.
		if !taskengine.IsAssistantProseHandler(ev.TaskHandler) {
			return
		}
		if ev.Content != "" || ev.Thinking != "" {
			_ = s.notify("chatDelta", chatDeltaEvent{
				SessionID: turn.SessionID,
				TurnID:    turn.TurnID,
				Content:   ev.Content,
				Thinking:  ev.Thinking,
			})
		}
	case taskengine.TaskEventPrint:
		if ev.Content != "" {
			_ = s.notify("chatDelta", chatDeltaEvent{
				SessionID: turn.SessionID,
				TurnID:    turn.TurnID,
				Content:   ev.Content,
			})
		}
	case taskengine.TaskEventToolCallPending:
		_ = s.notify("toolCall", toolCallEventFromTaskEvent(turn, ev, "pending"))
	case taskengine.TaskEventToolCall:
		status := "completed"
		if ev.Error != "" {
			status = "failed"
		}
		_ = s.notify("toolCall", toolCallEventFromTaskEvent(turn, ev, status))
	case taskengine.TaskEventHITLDecision:
		_ = s.notify("hitlDecision", s.hitlDecisionEventFromTaskEvent(ctx, turn, ev))
	case taskengine.TaskEventStepStarted:
		if !taskengine.IsToolBearingHandler(ev.TaskHandler) {
			_ = s.notify("toolCall", toolCallEventFromTaskEvent(turn, ev, "started"))
		}
	case taskengine.TaskEventStepCompleted:
		if !taskengine.IsToolBearingHandler(ev.TaskHandler) {
			_ = s.notify("toolCall", toolCallEventFromTaskEvent(turn, ev, "completed"))
		}
	case taskengine.TaskEventStepFailed:
		if !taskengine.IsToolBearingHandler(ev.TaskHandler) {
			_ = s.notify("toolCall", toolCallEventFromTaskEvent(turn, ev, "failed"))
		}
	case taskengine.TaskEventTokenUsage:
		_ = s.notify("contextUsage", contextUsageEvent{
			SessionID: turn.SessionID,
			TurnID:    turn.TurnID,
			Used:      ev.TokenUsed,
			Size:      ev.TokenSize,
		})
	}
}

func (s *Server) hitlDecisionEventFromTaskEvent(ctx context.Context, turn turnInfo, ev taskengine.TaskEvent) hitlDecisionEvent {
	policyName := ev.HITLPolicyName
	policyPath := ev.HITLPolicyPath
	if policyName == "" || policyPath == "" {
		active := s.activeHITLPolicy(ctx)
		if policyName == "" {
			policyName = active.Name
		}
		if policyPath == "" {
			policyPath = active.Path
		}
	}
	approvalRequested := false
	if ev.HITLApprovalRequested != nil {
		approvalRequested = *ev.HITLApprovalRequested
	}
	return hitlDecisionEvent{
		SessionID:         turn.SessionID,
		TurnID:            turn.TurnID,
		ToolsName:         ev.HookName,
		ToolName:          ev.ToolName,
		Action:            ev.HITLAction,
		Reason:            ev.HITLReason,
		PolicyName:        policyName,
		PolicyPath:        policyPath,
		ArgsSummary:       ev.HITLArgsSummary,
		MatchedRule:       ev.HITLMatchedRule,
		TimeoutS:          ev.HITLTimeoutS,
		ApprovalRequested: approvalRequested,
	}
}

func toolCallEventFromTaskEvent(turn turnInfo, ev taskengine.TaskEvent, status string) toolCallEvent {
	id := ev.ApprovalID
	if id == "" {
		id = ev.ToolName
	}
	if id == "" {
		id = ev.TaskID
	}
	title := ev.ToolName
	if title == "" {
		title = ev.TaskID
	}
	if title == "" {
		title = ev.TaskHandler
	}
	if summary := summarizeArgs(ev.ApprovalArgs); summary != "" && !strings.Contains(title, summary) {
		title += ": " + summary
	}
	return toolCallEvent{
		SessionID:  turn.SessionID,
		TurnID:     turn.TurnID,
		ToolCallID: id,
		Title:      title,
		Status:     status,
		ToolName:   ev.ToolName,
		TaskID:     ev.TaskID,
		Input:      ev.ApprovalArgs,
		Output:     ev.Content,
		Error:      ev.Error,
		DiffPath:   ev.ToolDiffPath,
		DiffOld:    ev.ToolDiffOldText,
		DiffNew:    ev.ToolDiffNewText,
	}
}

func summarizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	for _, key := range []string{"path", "command", "url", "pattern"} {
		if v, ok := args[key].(string); ok && strings.TrimSpace(v) != "" {
			return trimSummary(v)
		}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return trimSummary(string(raw))
}

func trimSummary(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len([]rune(s)) > 96 {
		return string([]rune(s)[:95]) + "..."
	}
	return s
}
