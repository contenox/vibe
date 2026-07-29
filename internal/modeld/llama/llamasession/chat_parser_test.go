//go:build llamanode && llamacpp_direct

package llamasession

import (
	"errors"
	"strings"
	"testing"
)

func TestUnit_ChatParsePreview_BoundsLongOutput(t *testing.T) {
	raw := strings.Repeat("a", maxChatParsePreviewRunes) + strings.Repeat("z", 100)

	got := chatParsePreview(raw)
	if !strings.Contains(got, "...<truncated>...") {
		t.Fatalf("preview missing truncation marker: %q", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("a", maxChatParsePreviewRunes/2)) {
		t.Fatalf("preview missing head: %q", got)
	}
	if !strings.HasSuffix(got, strings.Repeat("z", 100)) {
		t.Fatalf("preview missing tail: %q", got)
	}
	if len([]rune(got)) > maxChatParsePreviewRunes+len("...<truncated>...") {
		t.Fatalf("preview too long: %d runes", len([]rune(got)))
	}
}

func TestUnit_ChatOutputParser_ParseErrorIncludesBoundedRawOutput(t *testing.T) {
	p := &chatOutputParser{reasoningFormat: "deepseek", parseToolCalls: true}
	raw := strings.Repeat("x", maxChatParsePreviewRunes+100)

	err := p.parseError(errors.New("parse failed"), false, raw)
	msg := err.Error()
	for _, want := range []string{
		"parse failed",
		"partial=false",
		"parse_tool_calls=true",
		`reasoning_format="deepseek"`,
		"raw_len=612",
		"raw_preview=",
		"...<truncated>...",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
	if len(msg) > 900 {
		t.Fatalf("error unexpectedly long: %d bytes", len(msg))
	}
}
