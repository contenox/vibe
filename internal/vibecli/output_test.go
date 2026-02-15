package vibecli

import (
	"testing"
	"time"

	"github.com/contenox/vibe/taskengine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_splitAndTrim(t *testing.T) {
	tests := []struct {
		name string
		s    string
		sep  string
		want []string
	}{
		{"empty string", "", ",", nil},
		{"single token", "a", ",", []string{"a"}},
		{"multiple with spaces", " a , b , c ", ",", []string{"a", "b", "c"}},
		{"leading trailing sep", ",,a,,b,,", ",", []string{"a", "b"}},
		{"spaces only dropped", "  ,  ,  ", ",", nil},
		{"pipe sep", "x|y|z", "|", []string{"x", "y", "z"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitAndTrim(tt.s, tt.sep)
			assert.Equal(t, tt.want, got)
		})
	}
}

func Test_formatDuration(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero", 0, "0µs"},
		{"sub-ms", 500 * time.Microsecond, "500µs"},
		{"ms", 53 * time.Millisecond, "53ms"},
		{"seconds", 1700 * time.Millisecond, "1.70s"},
		{"one second", time.Second, "1.00s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDuration(tt.d)
			assert.Equal(t, tt.want, got)
		})
	}
}

func Test_lastAssistantContentFromHistory(t *testing.T) {
	tests := []struct {
		name string
		chat taskengine.ChatHistory
		want string
	}{
		{"empty", taskengine.ChatHistory{Messages: nil}, ""},
		{"empty messages", taskengine.ChatHistory{Messages: []taskengine.Message{}}, ""},
		{"no assistant", taskengine.ChatHistory{
			Messages: []taskengine.Message{
				{Role: "user", Content: "hi"},
			},
		}, ""},
		{"last is assistant", taskengine.ChatHistory{
			Messages: []taskengine.Message{
				{Role: "user", Content: "hi"},
				{Role: "assistant", Content: "hello"},
			},
		}, "hello"},
		{"last assistant wins", taskengine.ChatHistory{
			Messages: []taskengine.Message{
				{Role: "assistant", Content: "first"},
				{Role: "user", Content: "again"},
				{Role: "assistant", Content: "second"},
			},
		}, "second"},
		{"assistant with empty content skipped", taskengine.ChatHistory{
			Messages: []taskengine.Message{
				{Role: "assistant", Content: ""},
				{Role: "assistant", Content: "last"},
			},
		}, "last"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lastAssistantContentFromHistory(tt.chat)
			assert.Equal(t, tt.want, got)
		})
	}
}

func Test_lastAssistantContentFromHistory_returns_last_non_empty_assistant(t *testing.T) {
	chat := taskengine.ChatHistory{
		Messages: []taskengine.Message{
			{Role: "user", Content: "1"},
			{Role: "assistant", Content: "a"},
			{Role: "user", Content: "2"},
			{Role: "assistant", Content: "b"},
		},
	}
	require.Equal(t, "b", lastAssistantContentFromHistory(chat))
}
