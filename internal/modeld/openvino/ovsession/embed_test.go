//go:build openvino && openvino_genai

package ovsession_test

import (
	"context"
	"os"
	"testing"

	"github.com/contenox/contenox/internal/modeld/openvino/ovsession"
)

func TestSystem_OpenVINOEmbed_GeneratesVectors(t *testing.T) {
	// The path to the downloaded bge-small-en-v1.5-ov
	path := os.Getenv("CONTENOX_OPENVINO_EMBED_MODEL")
	if path == "" {
		t.Skip("set CONTENOX_OPENVINO_EMBED_MODEL to run embedding tests")
	}

	session, err := ovsession.NewEmbed(path, "CPU")
	if err != nil {
		t.Fatalf("NewEmbed: %v", err)
	}
	defer session.Close()

	ctx := context.Background()
	emb1, err := session.Embed(ctx, "hello world")
	if err != nil {
		t.Fatalf("Embed 1 failed: %v", err)
	}
	if len(emb1) == 0 {
		t.Fatal("expected non-empty embedding vector")
	}

	emb2, err := session.Embed(ctx, "hello world")
	if err != nil {
		t.Fatalf("Embed 2 failed: %v", err)
	}
	if len(emb1) != len(emb2) {
		t.Fatalf("embedding lengths differ: %d vs %d", len(emb1), len(emb2))
	}
	
	// They should be roughly identical
	diff := float32(0.0)
	for i := range emb1 {
		d := emb1[i] - emb2[i]
		if d < 0 {
			d = -d
		}
		diff += d
	}
	if diff > 1e-4 {
		t.Fatalf("embeddings for same prompt are not deterministic, diff=%f", diff)
	}
}
