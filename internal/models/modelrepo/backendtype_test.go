package modelrepo

import "testing"

func TestUnit_CanonicalBackendType(t *testing.T) {
	cases := map[string]string{
		"ollama":     "ollama",
		" OLLAMA ":   "ollama",
		"OpenAI":     "openai",
		"vllm":       "vllm",
		"":           "",
		" Anthropic": "anthropic",
	}
	for in, want := range cases {
		if got := CanonicalBackendType(in); got != want {
			t.Errorf("CanonicalBackendType(%q) = %q, want %q", in, got, want)
		}
	}
}
