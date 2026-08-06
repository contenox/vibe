package libacp

import (
	"fmt"
	"strings"
)

// TurnTracker watches one prompt turn's session/update stream and tells a
// client-side driver whether the agent ever produced a renderable answer —
// guarding against an agent returning a normal session/prompt result while
// never emitting an agent_message_chunk. A driver feeds each notification to
// Observe and calls Err at turn end to convert "nothing displayable" into an
// explicit ErrNoDisplayableOutput. Single-turn: construct fresh (or Reset)
// per turn. Not safe for concurrent use; drive from the read-loop goroutine.
type TurnTracker struct {
	sawDisplayable bool
	toolUpdates    int
}

// Observe records one inbound session/update. It only inspects the update
// payload; session-id matching (stale-update filtering) is a separate concern —
// see FilterSessionUpdates.
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
// were observed. Reported inside Err so an operator can tell "tool activity but
// no final text" from "literally nothing".
func (t *TurnTracker) ToolUpdateCount() int { return t.toolUpdates }

// Err returns nil when the turn produced displayable output, otherwise an
// ErrNoDisplayableOutput enriched with the turn's stop reason and tool-update
// count. Call it once the session/prompt result is in hand, passing that
// result's StopReason.
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

// isDisplayableContent reports whether a message chunk's content block is
// something a user would see: non-empty text, or any non-text block (image,
// resource, ...). A chunk whose only text is whitespace does not count, so a
// stream of blank chunks is still an empty turn.
func isDisplayableContent(c *ContentBlock) bool {
	if c == nil {
		return false
	}
	if c.Type == string(ContentKindText) {
		return strings.TrimSpace(c.Text) != ""
	}
	return true
}
