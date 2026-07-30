//go:build llamanode

package llamasession

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/modeld/llama"
	"github.com/contenox/contenox/libbenchreport"
	"github.com/contenox/contenox/libcontextasm"
	"github.com/contenox/contenox/libtransport"
)

// benchHarness drives the common benchmark report against a real llama.cpp
// session using the tiny GGUF fixture.
type benchHarness struct {
	modelPath string
	cfg       llama.Config
}

func (h benchHarness) OpenSession(_ context.Context) (transport.Session, error) {
	return New(h.modelPath, h.cfg)
}

func (h benchHarness) Turn(stable, suffix string) (transport.PrefixInput, transport.SuffixInput) {
	m := benchManifest(stable, suffix)
	return transport.PrefixInput{Text: stable, Manifest: m}, transport.SuffixInput{Text: suffix, Manifest: m}
}

func TestSystem_LlamaBenchReport_WarmReuseAndColdEquivalence(t *testing.T) {
	modelPath := os.Getenv("CONTENOX_LLAMA_TINY_GGUF")
	requireTinyGGUF(t, modelPath)

	h := benchHarness{
		modelPath: modelPath,
		cfg:       llama.Config{NumCtx: 256, NumBatch: 16, NumThreads: 1, DisableBOS: true},
	}
	sc := benchreport.Scenario{
		Stable:        "system\nYou are a precise coding assistant.\n",
		Suffix:        "user\nfix the bug\n",
		ChangedSuffix: "user\nadd a unit test\n",
		EditedStable:  "system\nYou are a terse coding assistant.\n",
		SuffixCurve:   []string{"user\na\n", "user\nab\n", "user\nabc\n"},
		MaxDecode:     4,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	rep, err := benchreport.Run(ctx, h, sc, benchreport.Report{
		Model: "tiny", Backend: "llamacpp", Mode: "live_prefix_reuse",
	})
	if err != nil {
		t.Fatal(err)
	}

	out, _ := json.MarshalIndent(rep, "", "  ")
	t.Logf("llama bench report:\n%s", out)

	if rep.WarmSamePrefix.HitRate != 1 {
		t.Fatalf("warm same-prefix hit rate = %v, want full reuse", rep.WarmSamePrefix.HitRate)
	}
	if !rep.WarmChangedSuffix.OutputEqualsCold {
		t.Fatalf("warm changed-suffix output != cold reference")
	}
	if !rep.EditedStable.ActualCacheMiss {
		t.Fatalf("edited stable segment was not a cache miss")
	}
	if !rep.EditedStable.OutputEqualsCold {
		t.Fatalf("edited stable output != cold reference")
	}
	if !rep.FailureCases.CancelDecode {
		t.Fatalf("cancel-decode failure case not handled")
	}
}

func benchManifest(stable, suffix string) transport.ContextManifest {
	return transport.ContextManifest{
		ProfileID:            "bench",
		Backend:              "llamacpp",
		BackendVersion:       "test",
		ModelDigest:          "tiny",
		PromptFormat:         "chatml",
		PromptTemplateDigest: "test-template",
		RuntimeDigest:        "test-runtime",
		AddBOS:               false,
		StableBytes:          len(stable),
		TotalBytes:           len(stable) + len(suffix),
		StableByteHash:       benchSHA(stable),
		Segments: []contextasm.ManifestSegment{
			{Kind: "system", Stable: true, ByteStart: 0, ByteEnd: len(stable), ByteHash: benchSHA(stable)},
			{Kind: "user", Stable: false, ByteStart: len(stable), ByteEnd: len(stable) + len(suffix), ByteHash: benchSHA(suffix)},
		},
	}
}

func benchSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
