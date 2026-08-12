package acpsvc

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/libacp"
)

func (t *Transport) translateEvents(ctx context.Context, sid libacp.SessionID, ch <-chan []byte) {
	for payload := range ch {
		t.publishEvent(ctx, sid, payload)
	}
}

func (t *Transport) publishEvent(ctx context.Context, sid libacp.SessionID, payload []byte) {
	var ev taskengine.TaskEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}
	switch ev.Kind {
	case taskengine.TaskEventStepChunk:
		// Only a handler whose streamed output is assistant narration reaches the
		// transcript — a route task's streamed output, e.g., is its routing
		// decision, not prose. taskengine owns the handler vocabulary and judgement.
		if !taskengine.IsAssistantProseHandler(ev.TaskHandler) {
			return
		}
		if ev.Content != "" {
			t.sendUpdate(ctx, libacp.SessionNotification{
				SessionID: sid,
				Update:    libacp.NewAgentMessageChunk(ev.Content),
			})
		}
		if ev.Thinking != "" {
			t.sendUpdate(ctx, libacp.SessionNotification{
				SessionID: sid,
				Update:    libacp.NewAgentThoughtChunk(ev.Thinking),
			})
		}
	case taskengine.TaskEventStepStreamEnd:
		// Consumed without a wire notification: ACP has no end-of-stream frame,
		// and usage indicators come from token_usage. Kept as an explicit case so
		// the engine-events contract test can assert every kind is addressed.
	case taskengine.TaskEventChainSuspended:
		// Consumed without a wire notification: the permission flow's approval
		// card already stands in for the suspension; answering it resumes the run.
	case taskengine.TaskEventStepStarted:
		if taskengine.IsToolBearingHandler(ev.TaskHandler) {
			return
		}
		t.sendUpdate(ctx, toolCallNotification(sid, ev, libacp.ToolCallStatusInProgress))
	case taskengine.TaskEventStepCompleted:
		if taskengine.IsToolBearingHandler(ev.TaskHandler) {
			return
		}
		t.sendUpdate(ctx, toolCallNotification(sid, ev, libacp.ToolCallStatusCompleted))
	case taskengine.TaskEventStepFailed:
		if taskengine.IsToolBearingHandler(ev.TaskHandler) {
			return
		}
		t.sendUpdate(ctx, toolCallNotification(sid, ev, libacp.ToolCallStatusFailed))
	case taskengine.TaskEventPrint:
		if ev.Content != "" {
			t.sendUpdate(ctx, libacp.SessionNotification{
				SessionID: sid,
				Update:    libacp.NewAgentMessageChunk(ev.Content),
			})
		}
	case taskengine.TaskEventToolCallPending:
		id := t.toolCallWireID(sid, ev, false)
		t.sendToolCallUpdateGuarded(ctx, sid, id, toolCallPendingNotification(sid, ev, id))
	case taskengine.TaskEventToolCall:
		id := t.toolCallWireID(sid, ev, true)
		t.sendToolCallUpdateGuarded(ctx, sid, id, toolCallUpdateNotification(sid, ev, id))
		// A mission_plan call also projects the stored plan snapshot as a
		// full-snapshot ACP `plan` update, in addition to the tool-call card above.
		// planUpdateNotification returns ok only for a successful, parseable event.
		if note, ok := planUpdateNotification(sid, ev); ok {
			t.sendUpdate(ctx, note)
		}
	case taskengine.TaskEventTokenUsage:
		used := ev.TokenUsed
		size := ev.TokenSize
		// Fall back to the session's effective token limit when the chain has no
		// ctxLength budget set, so indicators reflect the user-configured budget.
		if size <= 0 {
			if s, ok := t.sessionFor(sid); ok && s != nil {
				if eff := s.effectiveTokenLimit(); eff > 0 {
					size = eff
				}
			}
		}
		if size > 0 || used > 0 {
			t.sendUpdate(ctx, libacp.SessionNotification{
				SessionID: sid,
				Update: libacp.SessionUpdate{
					SessionUpdate: libacp.SessionUpdateUsageUpdate,
					Used:          used,
					Size:          size,
				},
			})
		}
	}
}

func toolCallInProgressNotification(sid libacp.SessionID, ev taskengine.TaskEvent) libacp.SessionNotification {
	return libacp.SessionNotification{
		SessionID: sid,
		Update: libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateToolCallUpdate,
			ToolCallID:    fallbackToolCallID(ev),
			Kind:          toolKindFor(ev.ToolName),
			Status:        libacp.ToolCallStatusInProgress,
		},
	}
}

func toolCallNotification(sid libacp.SessionID, ev taskengine.TaskEvent, status libacp.ToolCallStatus) libacp.SessionNotification {
	update := libacp.SessionUpdate{
		SessionUpdate: libacp.SessionUpdateToolCallUpdate,
		ToolCallID:    ev.TaskID,
		Title:         taskTitle(ev),
		Kind:          libacp.ToolKindOther,
		Status:        status,
	}
	if ev.Error != "" {
		update.Meta = json.RawMessage(`{"error":` + jsonString(ev.Error) + `}`)
	}
	return libacp.SessionNotification{SessionID: sid, Update: update}
}

func toolCallPendingNotification(sid libacp.SessionID, ev taskengine.TaskEvent, toolCallID string) libacp.SessionNotification {
	update := libacp.SessionUpdate{
		SessionUpdate: libacp.SessionUpdateToolCall,
		ToolCallID:    toolCallID,
		Title:         toolCallTitle(ev),
		Kind:          toolKindFor(ev.ToolName),
		Status:        libacp.ToolCallStatusPending,
	}
	if raw, err := json.Marshal(ev.ApprovalArgs); err == nil && len(ev.ApprovalArgs) > 0 {
		update.RawInput = raw
	}
	if locs := toolCallLocations(ev); len(locs) > 0 {
		update.Locations = locs
	}
	return libacp.SessionNotification{SessionID: sid, Update: update}
}

func toolCallUpdateNotification(sid libacp.SessionID, ev taskengine.TaskEvent, toolCallID string) libacp.SessionNotification {
	// INVARIANT: a call whose tool never ran is never "completed"; ACP's
	// closed status set maps a policy denial to "failed".
	status := libacp.ToolCallStatusCompleted
	errText := ev.Error
	if errText == "" {
		if reason, denied := policyDenialReason(ev.Content); denied {
			errText = reason
		}
	}
	if errText != "" {
		status = libacp.ToolCallStatusFailed
	}

	update := libacp.SessionUpdate{
		SessionUpdate: libacp.SessionUpdateToolCallUpdate,
		ToolCallID:    toolCallID,
		Title:         toolCallTitle(ev),
		Kind:          toolKindFor(ev.ToolName),
		Status:        status,
	}

	if raw, err := json.Marshal(ev.ApprovalArgs); err == nil && len(ev.ApprovalArgs) > 0 {
		update.RawInput = raw
	}
	if ev.Content != "" {
		update.RawOutput = json.RawMessage(jsonString(ev.Content))
	}

	if diff := diffContentFromEvent(ev); diff != nil {
		update.ToolContent = []libacp.ToolCallContent{*diff}
	}

	if locs := toolCallLocations(ev); len(locs) > 0 {
		update.Locations = locs
	}

	if errText != "" {
		update.Meta = json.RawMessage(`{"error":` + jsonString(errText) + `}`)
	}

	return libacp.SessionNotification{SessionID: sid, Update: update}
}

func policyDenialReason(content string) (string, bool) {
	switch content {
	case localtools.DenyMessage, localtools.DenyTimeoutMessage:
		return content, true
	}
	return "", false
}

func toolCallIDFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(taskengine.ContextKeyToolCallID).(string)
	return v
}

// terminalAttachNotification uses SessionUpdateToolCall (create-or-update) so
// it's race-safe vs the pending notification. ACP requires `title` on
// tool_call notifications; omitting it makes Zed reject the update.
func terminalAttachNotification(sid libacp.SessionID, toolCallID, terminalID, title string) libacp.SessionNotification {
	return libacp.SessionNotification{
		SessionID: sid,
		Update: libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateToolCall,
			ToolCallID:    toolCallID,
			Title:         title,
			Kind:          libacp.ToolKindExecute,
			Status:        libacp.ToolCallStatusInProgress,
			ToolContent: []libacp.ToolCallContent{
				{Type: libacp.ToolCallContentTerminal, TerminalID: terminalID},
			},
		},
	}
}

// fallbackToolCallID is the name-derived id used when the engine minted no
// ApprovalID; not invocation-unique alone — see Transport.toolCallWireID.
func fallbackToolCallID(ev taskengine.TaskEvent) string {
	if ev.ApprovalID != "" {
		return ev.ApprovalID
	}
	if ev.ToolName != "" {
		return ev.ToolName
	}
	return ev.TaskID
}

func toolCallTitle(ev taskengine.TaskEvent) string {
	if ev.ToolName != "" {
		if summary := summarizeToolCallArgs(ev.ToolName, ev.ApprovalArgs); summary != "" {
			return ev.ToolName + ": " + summary
		}
		return ev.ToolName
	}
	return taskTitle(ev)
}

func summarizeToolCallArgs(toolName string, args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	base := toolName
	if idx := strings.LastIndex(toolName, "."); idx >= 0 {
		base = toolName[idx+1:]
	}

	asString := func(key string) string {
		if v, ok := args[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}
	asStringSlice := func(key string) []string {
		v, ok := args[key]
		if !ok {
			return nil
		}
		arr, ok := v.([]any)
		if !ok {
			return nil
		}
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}

	var summary string
	switch base {
	case "exec", "run", "execute", "local_shell":
		cmd := asString("command")
		if cmd == "" {
			break
		}
		parts := []string{cmd}
		if a := asString("args"); a != "" {
			parts = append(parts, a)
		} else {
			parts = append(parts, asStringSlice("args")...)
		}
		summary = strings.Join(parts, " ")
	case "read_file", "read_file_range", "write_file", "stat_file", "list_dir", "delete_file":
		summary = asString("path")
	case "grep":
		if p := asString("pattern"); p != "" {
			if path := asString("path"); path != "" {
				summary = p + " in " + path
			} else {
				summary = p
			}
		}
	case "sed":
		if path := asString("path"); path != "" {
			if pat := asString("pattern"); pat != "" {
				summary = pat + " in " + path
			} else {
				summary = path
			}
		}
	case "fetch_url", "fetch", "http_get":
		summary = asString("url")
	}

	if summary == "" {
		return ""
	}
	summary = strings.TrimSpace(strings.ReplaceAll(summary, "\n", " "))
	const maxRunes = 80
	if r := []rune(summary); len(r) > maxRunes {
		summary = string(r[:maxRunes-1]) + "…"
	}
	return summary
}

func toolKindFor(toolName string) libacp.ToolKind {
	if toolName == "" {
		return libacp.ToolKindOther
	}
	base := toolName
	if idx := strings.Index(toolName, "."); idx >= 0 {
		base = toolName[idx+1:]
	}
	switch {
	case strings.HasPrefix(base, "read"), strings.HasPrefix(base, "stat"), strings.HasPrefix(base, "list"), strings.HasPrefix(base, "grep"):
		return libacp.ToolKindRead
	case strings.HasPrefix(base, "write"), base == "sed":
		return libacp.ToolKindEdit
	case strings.HasPrefix(base, "delete"):
		return libacp.ToolKindDelete
	case strings.HasPrefix(base, "move"):
		return libacp.ToolKindMove
	case strings.HasPrefix(base, "fetch"), strings.HasPrefix(base, "http"):
		return libacp.ToolKindFetch
	case strings.HasPrefix(toolName, "local_shell."), strings.HasPrefix(base, "exec"), strings.HasPrefix(base, "run"):
		return libacp.ToolKindExecute
	}
	return libacp.ToolKindOther
}

func toolCallLocations(ev taskengine.TaskEvent) []libacp.ToolCallLocation {
	path := ""
	if diff := diffContentFromEvent(ev); diff != nil && diff.Path != "" {
		path = diff.Path
	} else if p, ok := ev.ApprovalArgs["path"].(string); ok {
		path = strings.TrimSpace(p)
	}
	if path == "" {
		return nil
	}
	return []libacp.ToolCallLocation{{Path: path}}
}

func diffContentFromEvent(ev taskengine.TaskEvent) *libacp.ToolCallContent {
	if ev.ToolDiffPath != "" && ev.ToolDiffOldText != ev.ToolDiffNewText {
		return &libacp.ToolCallContent{
			Type:    libacp.ToolCallContentDiff,
			Path:    ev.ToolDiffPath,
			OldText: ev.ToolDiffOldText,
			NewText: ev.ToolDiffNewText,
		}
	}
	return diffContentFromResult(ev.Content)
}

func diffContentFromResult(content string) *libacp.ToolCallContent {
	if content == "" {
		return nil
	}
	var fw struct {
		Path    string `json:"path"`
		Written bool   `json:"written"`
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
	}
	if err := json.Unmarshal([]byte(content), &fw); err != nil {
		return nil
	}
	if fw.Path == "" || !fw.Written {
		return nil
	}
	return &libacp.ToolCallContent{
		Type:    libacp.ToolCallContentDiff,
		Path:    fw.Path,
		OldText: fw.OldText,
		NewText: fw.NewText,
	}
}

func taskTitle(ev taskengine.TaskEvent) string {
	if ev.TaskID != "" {
		return ev.TaskID + " (" + ev.TaskHandler + ")"
	}
	return ev.TaskHandler
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
