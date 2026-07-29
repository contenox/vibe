//go:build openvino && openvino_genai

package openvino

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/contextasm"
	"github.com/contenox/contenox/internal/transport"
)

// TestSystem_OpenVINOService_AdapterRequestChangesGeneration drives the modeld
// OpenVINO Service end to end with adapters set on the OpenSessionRequest, proving
// the request path req.Adapters -> toGenAILoRA -> genaiCfg.LoRAAdapters -> NewGenAI
// reaches the MODE_DYNAMIC attach and changes generation. The pure-Go
// TestUnit_ToGenAILoRA_MapsScaleToAlphaInOrder proves the mapping; this proves the
// same threading against the real OpenVINO GenAI backend.
//
// Reuses the IR model + safetensors adapter fixtures from
// TestSystem_OpenVINOGenAI_LoRAAdapterGenerates.
func TestSystem_OpenVINOService_AdapterRequestChangesGeneration(t *testing.T) {
	modelDir := os.Getenv("CONTENOX_OPENVINO_TEST_MODEL")
	loraPath := os.Getenv("CONTENOX_OPENVINO_TEST_LORA")
	if modelDir == "" || loraPath == "" {
		t.Skip("set CONTENOX_OPENVINO_TEST_MODEL and CONTENOX_OPENVINO_TEST_LORA")
	}
	ctx := context.Background()
	const prompt = "def add(a, b):"

	base := openvinoServiceDecode(t, ctx, transport.OpenSessionRequest{
		ModelName: "test", Type: "openvino", Path: modelDir,
		Config: transport.Config{NumCtx: 4096},
	}, prompt)
	variant := openvinoServiceDecode(t, ctx, transport.OpenSessionRequest{
		ModelName: "test", Type: "openvino", Path: modelDir,
		Config:   transport.Config{NumCtx: 4096},
		Adapters: []transport.AdapterSpec{{Name: "smoke", Path: loraPath, Scale: 4.0}},
	}, prompt)

	t.Logf("base=%q", base)
	t.Logf("variant=%q", variant)
	if strings.TrimSpace(variant) == "" {
		t.Fatal("variant produced empty output")
	}
	if base == variant {
		t.Fatal("adapter on the OpenSessionRequest did not change generation through the Service")
	}
}

func openvinoServiceDecode(t *testing.T, ctx context.Context, req transport.OpenSessionRequest, suffix string) string {
	t.Helper()
	sess, err := (&Service{}).OpenSession(ctx, req)
	if err != nil {
		t.Fatalf("OpenSession on %s: %v", resolveDevice(), err)
	}
	defer func() { _ = sess.Close() }()

	stable := "You are a precise Go coding assistant. "
	manifest := contextasm.ContextManifest{
		Backend:              "openvino",
		ModelDigest:          "test-model",
		PromptFormat:         "openvino_chat_template",
		PromptTemplateDigest: "test-model",
		RuntimeDigest:        resolveDevice(),
		StableByteHash:       contextasm.HashString(stable),
	}
	if _, err := sess.EnsurePrefix(ctx, transport.PrefixInput{Text: stable, Manifest: manifest}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	if _, err := sess.PrefillSuffix(ctx, transport.SuffixInput{Text: suffix, Manifest: manifest}); err != nil {
		t.Fatalf("PrefillSuffix: %v", err)
	}
	ch, err := sess.Decode(ctx, transport.DecodeConfig{MaxTokens: 24})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	var b strings.Builder
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("stream: %v", chunk.Error)
		}
		b.WriteString(chunk.Text)
	}
	return strings.TrimSpace(b.String())
}
