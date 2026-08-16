package contenoxcli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

func splitAndTrim(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func lastAssistantContentFromHistory(chat taskengine.ChatHistory) string {
	for i := len(chat.Messages) - 1; i >= 0; i-- {
		m := chat.Messages[i]
		if m.Role == "assistant" && m.Content != "" {
			return m.Content
		}
	}
	return ""
}

func printRelevantOutput(w io.Writer, output any, outputType taskengine.DataType, raw bool) {
	if raw {
		printOutput(w, output)
		return
	}
	switch outputType {
	case taskengine.DataTypeChatHistory:
		if ch, ok := output.(taskengine.ChatHistory); ok {
			if content := lastAssistantContentFromHistory(ch); content != "" {
				fmt.Fprintln(w, content)
				return
			}
		}
	case taskengine.DataTypeString:
		if s, ok := output.(string); ok {
			fmt.Fprintln(w, s)
			return
		}
	}
	printOutput(w, output)
}

func printOutput(w io.Writer, output any) {
	switch v := output.(type) {
	case string:
		fmt.Fprintln(w, v)
	case []byte:
		fmt.Fprintln(w, string(v))
	default:
		b, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			fmt.Fprintln(w, output)
			return
		}
		fmt.Fprintln(w, string(b))
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	if d < time.Second {
		return fmt.Sprintf("%.0fms", d.Seconds()*1000)
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}
