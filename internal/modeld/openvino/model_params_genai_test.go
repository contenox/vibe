//go:build openvino && openvino_genai

package openvino

import (
	"os"
	"testing"
)

// TestSystem_ContextLength_RealIRReturnsNonZero proves ContextLength's
// header-only parse (config.json's max_position_embeddings, via the real
// native shim) actually works against a genuine OpenVINO IR directory — the
// round trip modelstore.Admin.ListModels relies on for the cheap
// context-length enrichment.
func TestSystem_ContextLength_RealIRReturnsNonZero(t *testing.T) {
	modelDir := os.Getenv("CONTENOX_OPENVINO_TEST_MODEL")
	if modelDir == "" {
		t.Skip("set CONTENOX_OPENVINO_TEST_MODEL (see Makefile.openvino)")
	}
	got, err := ContextLength(modelDir)
	if err != nil {
		t.Fatalf("ContextLength(%s): %v", modelDir, err)
	}
	if got <= 0 {
		t.Fatalf("ContextLength(%s) = %d, want a real positive trained context ceiling", modelDir, got)
	}
	t.Logf("%s: ContextLength = %d", modelDir, got)
}
