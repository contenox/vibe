//go:build llamacpp_direct

package llama

import (
	"os"
	"testing"
)

// TestSystem_ContextLength_RealGGUFReturnsNonZero proves ContextLength's
// header-only parse actually works against a genuine GGUF file through the
// real llamacppshim (not the inspectLlamaModel test-seam fake every other
// test in this package uses) — the round trip modelstore.Admin.ListModels
// relies on for the cheap context-length enrichment.
func TestSystem_ContextLength_RealGGUFReturnsNonZero(t *testing.T) {
	gguf := os.Getenv("CONTENOX_LLAMA_TINY_GGUF")
	if gguf == "" {
		t.Skip("set CONTENOX_LLAMA_TINY_GGUF to a small GGUF to exercise the real header parse")
	}
	got, err := ContextLength(gguf)
	if err != nil {
		t.Fatalf("ContextLength(%s): %v", gguf, err)
	}
	if got <= 0 {
		t.Fatalf("ContextLength(%s) = %d, want a real positive trained context ceiling", gguf, got)
	}
	t.Logf("%s: ContextLength = %d", gguf, got)
}
