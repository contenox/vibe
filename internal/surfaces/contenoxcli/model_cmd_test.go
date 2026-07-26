package contenoxcli

import (
	"testing"
)

func TestUnit_DisplayModelNameStripsGeminiResourcePrefix(t *testing.T) {
	if got := displayModelName("models/gemini-3.1-pro-preview"); got != "gemini-3.1-pro-preview" {
		t.Fatalf("displayModelName stripped = %q", got)
	}
	if got := displayModelName("openai/gpt-5"); got != "openai/gpt-5" {
		t.Fatalf("displayModelName must not strip non-Gemini-looking names: %q", got)
	}
}
