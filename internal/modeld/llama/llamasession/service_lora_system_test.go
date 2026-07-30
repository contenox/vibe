//go:build llamanode

package llamasession

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/modeld/llama"
	"github.com/contenox/contenox/libtransport"
)

func TestSystem_LlamaService_AdapterRequestChangesGeneration(t *testing.T) {
	model := os.Getenv("CONTENOX_LLAMA_LORA_GGUF")
	adapter := os.Getenv("CONTENOX_LLAMA_LORA_ADAPTER")
	if model == "" || adapter == "" {
		t.Skip("set CONTENOX_LLAMA_LORA_GGUF and CONTENOX_LLAMA_LORA_ADAPTER")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	svc := &llama.Service{}
	cfg := llama.Config{NumCtx: 128, NumBatch: 32, NumThreads: 2, DisableBOS: true}
	stable, suffix := "system\n", "def add(a, b):\n"

	base := serviceDecode(t, ctx, svc, transport.OpenSessionRequest{
		Type: "llama", Path: model, Config: cfg,
	}, stable, suffix)
	variant := serviceDecode(t, ctx, svc, transport.OpenSessionRequest{
		Type: "llama", Path: model, Config: cfg,
		Adapters: []transport.AdapterSpec{{Name: "smoke", Path: adapter, Scale: 8.0}},
	}, stable, suffix)

	t.Logf("base=%q", base)
	t.Logf("variant=%q", variant)
	if strings.TrimSpace(variant) == "" {
		t.Fatal("variant produced empty output")
	}
	if base == variant {
		t.Fatal("adapter on the OpenSessionRequest did not change generation through the Service")
	}
}

func serviceDecode(t *testing.T, ctx context.Context, svc *llama.Service, req transport.OpenSessionRequest, stable, suffix string) string {
	t.Helper()
	sess, err := svc.OpenSession(ctx, req)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer func() { _ = sess.Close() }()
	out, err := decodeContinuation(ctx, sess, stable, suffix, 12)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}
