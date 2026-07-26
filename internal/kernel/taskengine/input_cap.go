package taskengine

func capTaskInputForExecution(input any, dataType DataType, maxBytes int) (any, DataType) {
	if maxBytes <= 0 || input == nil {
		return input, dataType
	}
	switch dataType {
	case DataTypeString:
		s, ok := input.(string)
		if !ok {
			return input, dataType
		}
		return capTaskString(s, maxBytes), dataType
	case DataTypeChatHistory:
		h, ok := input.(ChatHistory)
		if !ok {
			return input, dataType
		}
		return capTaskChatHistory(h, maxBytes), dataType
	default:
		return input, dataType
	}
}

func capTaskString(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len([]byte(s)) <= maxBytes {
		return s
	}
	return capStringForPersistence(s, maxBytes)
}

func capTaskChatHistory(h ChatHistory, maxBytes int) ChatHistory {
	if maxBytes <= 0 || chatHistoryContentBytes(h) <= maxBytes {
		return h
	}
	out := h
	out.InputTokens = 0
	out.OutputTokens = 0
	out.Messages = make([]Message, 0, len(h.Messages))
	remaining := maxBytes
	for i := len(h.Messages) - 1; i >= 0; i-- {
		if remaining <= 0 {
			break
		}
		msg := h.Messages[i]
		msgBytes := len([]byte(msg.Content)) + len([]byte(msg.Thinking))
		if msgBytes > remaining {
			if len([]byte(msg.Content)) >= remaining || len(msg.Thinking) == 0 {
				msg.Content = capTaskString(msg.Content, remaining)
				msg.Thinking = ""
			} else {
				remaining -= len([]byte(msg.Content))
				msg.Thinking = capTaskString(msg.Thinking, remaining)
			}
			remaining = 0
		} else {
			remaining -= msgBytes
		}
		out.Messages = append(out.Messages, msg)
	}
	reverseMessages(out.Messages)
	return out
}

func chatHistoryContentBytes(h ChatHistory) int {
	total := 0
	for _, msg := range h.Messages {
		total += len([]byte(msg.Content))
		total += len([]byte(msg.Thinking))
	}
	return total
}

func reverseMessages(messages []Message) {
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
}
