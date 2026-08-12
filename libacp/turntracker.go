package libacp

import (
	"fmt"
	"strings"
)

// TurnTracker watches one prompt turn's session/update stream and tells a
// client-side driver whether the agent ever produced a renderable answer, via
// Observe per notification and Err at turn end; single-turn (construct fresh
// or Reset per turn) and not safe for concurrent use.
type TurnTracker struct {
	sawDisplayable bool
	toolUpdates    int
}

// Observe records one inbound session/update; it only inspects the update
// payload, leaving session-id matching to FilterSessionUpdates.
func (t *TurnTracker) Observe(n SessionNotification) {
	switch n.Update.SessionUpdate {
	case SessionUpdateAgentMessageChunk:
		if isDisplayableContent(n.Update.Content) {
			t.sawDisplayable = true
		}
	case SessionUpdateToolCall, SessionUpdateToolCallUpdate:
		t.toolUpdates++
	}
}

// SawDisplayableOutput reports whether any agent_message_chunk carrying
// renderable content has been observed this turn.
func (t *TurnTracker) SawDisplayableOutput() bool { return t.sawDisplayable }

// ToolUpdateCount reports how many tool_call / tool_call_update notifications
// were observed, so Err can let an operator tell "tool activity but no final
// text" from "literally nothing".
func (t *TurnTracker) ToolUpdateCount() int { return t.toolUpdates }

// Err returns nil when the turn produced displayable output, otherwise an
// ErrNoDisplayableOutput enriched with the turn's stop reason and tool-update
// count; call it once the session/prompt result is in hand.
func (t *TurnTracker) Err(stop StopReason) error {
	if t.sawDisplayable {
		return nil
	}
	reason := string(stop)
	if reason == "" {
		reason = "unknown"
	}
	if t.toolUpdates > 0 {
		return fmt.Errorf("%w (stopReason=%s, toolUpdates=%d)", ErrNoDisplayableOutput, reason, t.toolUpdates)
	}
	return fmt.Errorf("%w (stopReason=%s)", ErrNoDisplayableOutput, reason)
}

// Reset returns the tracker to its zero state so it can be reused for the next
// turn on the same session.
func (t *TurnTracker) Reset() { *t = TurnTracker{} }

func isDisplayableContent(c *ContentBlock) bool {
	if c == nil {
		return false
	}
	if c.Type == string(ContentKindText) {
		return strings.TrimSpace(c.Text) != ""
	}
	return true
}
