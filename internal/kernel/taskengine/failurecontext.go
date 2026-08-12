package taskengine

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

func ensureFailureContext(input any, dataType DataType, failedTaskID string, failure error) (any, DataType) {
	if failure == nil {
		return input, dataType
	}
	switch dataType {
	case DataTypeChatHistory:
		hist, ok := input.(ChatHistory)
		if !ok {
			return input, dataType
		}
		// Stripped on a copy: the caller's history keeps its audio.
		hist = stripAudioFromHistory(hist)
		if chatHistoryHasReportableContent(hist) {
			return hist, dataType
		}
		hist.Messages = append(hist.Messages, Message{
			ID:        uuid.NewString(),
			Role:      "user",
			Content:   failureContextNotice(failedTaskID, failure),
			Timestamp: time.Now().UTC(),
		})
		// Force recount: the notice's tokens are not in the old values.
		hist.InputTokens = 0
		hist.OutputTokens = 0
		return hist, dataType
	case DataTypeString:
		s, ok := input.(string)
		if !ok || strings.TrimSpace(s) != "" {
			return input, dataType
		}
		return failureContextNotice(failedTaskID, failure), dataType
	default:
		return input, dataType
	}
}

func stripAudioFromHistory(hist ChatHistory) ChatHistory {
	hasAudio := false
	for _, m := range hist.Messages {
		if len(m.Audio) > 0 {
			hasAudio = true
			break
		}
	}
	if !hasAudio {
		return hist
	}
	msgs := make([]Message, len(hist.Messages))
	copy(msgs, hist.Messages)
	for i := range msgs {
		msgs[i].Audio = nil
	}
	hist.Messages = msgs
	return hist
}

func chatHistoryHasReportableContent(hist ChatHistory) bool {
	for _, m := range repairToolCallPairing(hist.Messages) {
		if m.Role == "system" {
			continue
		}
		if len(m.CallTools) > 0 || len(m.Images) > 0 || strings.TrimSpace(m.Content) != "" {
			return true
		}
	}
	return false
}

func failureContextNotice(failedTaskID string, failure error) string {
	return fmt.Sprintf(
		"Task %q failed and the conversation context did not survive the failure routing. The error was:\n\n%s\n\nReport this failure honestly: state that the task failed, quote the error, and suggest what would unblock the work.",
		failedTaskID, failure,
	)
}
