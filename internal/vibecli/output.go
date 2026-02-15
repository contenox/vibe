// output.go holds CLI output helpers.
package vibecli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/vibe/taskengine"
)

// splitAndTrim splits s by sep and trims each element.
func splitAndTrim(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// lastAssistantContentFromHistory returns the content of the last assistant message with non-empty content.
func lastAssistantContentFromHistory(chat taskengine.ChatHistory) string {
	for i := len(chat.Messages) - 1; i >= 0; i-- {
		m := chat.Messages[i]
		if m.Role == "assistant" && m.Content != "" {
			return m.Content
		}
	}
	return ""
}

// printRelevantOutput prints only the relevant part of the result based on output type, unless raw is true.
func printRelevantOutput(output any, outputType taskengine.DataType, raw bool) {
	if raw {
		printOutput(output)
		return
	}
	switch outputType {
	case taskengine.DataTypeChatHistory:
		if ch, ok := output.(taskengine.ChatHistory); ok {
			if content := lastAssistantContentFromHistory(ch); content != "" {
				fmt.Println(content)
				return
			}
		}
	case taskengine.DataTypeString:
		if s, ok := output.(string); ok {
			fmt.Println(s)
			return
		}
	}
	printOutput(output)
}

// printOutput prints output in a human-friendly way.
func printOutput(output any) {
	switch v := output.(type) {
	case string:
		fmt.Println(v)
	case []byte:
		fmt.Println(string(v))
	default:
		b, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			fmt.Println(output)
			return
		}
		fmt.Println(string(b))
	}
}

// formatDuration formats a duration for step output (e.g. "1.70s", "53ms").
func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	if d < time.Second {
		return fmt.Sprintf("%.0fms", d.Seconds()*1000)
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}
