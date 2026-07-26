package taskengine

import (
	"crypto/sha1"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SynthesizeHistory rebuilds a conversation transcript from a chain run by
// walking the captured step stream. It exists so that hard-failed turns
// (errors, timeouts, cancellations, denied/timed-out HITL gates) make it
// into the persisted ChatHistory — the chain's returned ChatHistory only
// contains messages from steps that completed successfully.
//
// prior is the session history that was sent into the chain.
// units is the captured step stream from Inspector.GetExecutionHistory().
// chainErr is the error returned by the chain runner, if any.
//
// Messages are collected by identity (Message.ID, with a content-derived
// fallback for messages that predate creation-time IDs), never by index
// arithmetic: task handlers legitimately mutate the message list between a
// unit's input and output (system-instruction prepends, shift-to-fit
// trimming), so positional diffing re-emits or drops messages.
//
// Engine-injected system messages are excluded: task-level system
// instructions are re-applied from the task definition on every run and must
// not accumulate in the session transcript.
//
// The result satisfies the tool-call pairing invariant (see
// repairToolCallPairing): a chain that dies between an assistant tool call
// and its execution must not persist a transcript that strict providers
// reject on every subsequent turn.
//
// The result is a candidate []Message ready for chatservice.PersistDiff —
// PersistDiff handles dedupe against already-stored messages by ID, so the
// synthesizer is free to emit overlapping prefixes between runs.
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

// messageIdentity returns a stable identity key for dedupe. Messages created
// by the engine carry a creation-time ID; the fallback hash covers messages
// from sessions persisted before IDs were assigned at creation.
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
