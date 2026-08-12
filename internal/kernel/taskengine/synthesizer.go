package taskengine

import (
	"crypto/sha1"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SynthesizeHistory rebuilds a persisted-ready transcript from prior history and a chain run's captured step stream, including hard-failed turns and deduped by message identity.
func SynthesizeHistory(prior []Message, units []CapturedStateUnit, chainErr error) []Message {
	out := make([]Message, 0, len(prior)+len(units))
	out = append(out, prior...)

	seen := make(map[string]bool, len(prior))
	for _, m := range prior {
		seen[messageIdentity(m)] = true
	}

	lastUnitErrored := false
	for _, unit := range units {
		appendedFromOutput := false

		if unit.OutputType == DataTypeChatHistory {
			if outHist, ok := unit.Output.(ChatHistory); ok {
				for _, msg := range outHist.Messages {
					if msg.Role == "system" {
						continue
					}
					if unit.Error.Error != "" && isEmptyAssistantShell(msg) {
						continue
					}
					key := messageIdentity(msg)
					if seen[key] {
						continue
					}
					seen[key] = true
					out = append(out, msg)
					appendedFromOutput = true
				}
			}
		}

		if unit.Error.Error != "" {
			out = append(out, failureAnnotation(unit))
			lastUnitErrored = true
		} else if appendedFromOutput {
			lastUnitErrored = false
		}
	}

	if chainErr != nil && !lastUnitErrored {
		out = append(out, Message{
			ID:        uuid.NewString(),
			Role:      "assistant",
			Content:   fmt.Sprintf("[chain failed: %s]", chainErr.Error()),
			Timestamp: time.Now().UTC(),
		})
	}

	return repairToolCallPairing(out)
}

func messageIdentity(m Message) string {
	if m.ID != "" {
		return "id:" + m.ID
	}
	h := sha1.New()
	h.Write([]byte(m.Role))
	h.Write([]byte{0})
	h.Write([]byte(m.ToolCallID))
	h.Write([]byte{0})
	h.Write([]byte(m.Content))
	h.Write([]byte{0})
	h.Write([]byte(m.Timestamp.UTC().Format(time.RFC3339Nano)))
	for _, tc := range m.CallTools {
		h.Write([]byte{0})
		h.Write([]byte(tc.ID))
	}
	return fmt.Sprintf("h:%x", h.Sum(nil))
}

func isEmptyAssistantShell(msg Message) bool {
	return msg.Role == "assistant" &&
		msg.Content == "" &&
		msg.Thinking == "" &&
		len(msg.CallTools) == 0
}

func failureAnnotation(unit CapturedStateUnit) Message {
	var content string
	switch {
	case unit.Cancelled:
		content = fmt.Sprintf("[step %q (%s) was cancelled before completion]", unit.TaskID, unit.TaskHandler)
	case unit.TimedOut:
		content = fmt.Sprintf("[step %q (%s) timed out]", unit.TaskID, unit.TaskHandler)
	default:
		content = fmt.Sprintf("[step %q (%s) failed: %s]", unit.TaskID, unit.TaskHandler, unit.Error.Error)
	}
	return Message{
		ID:        uuid.NewString(),
		Role:      "assistant",
		Content:   content,
		Timestamp: time.Now().UTC(),
	}
}
