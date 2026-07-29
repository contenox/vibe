package benchreport

import (
	"context"
	"testing"

	"github.com/contenox/contenox/internal/kernel/contextasm"
	"github.com/contenox/contenox/internal/transport"
)

// memHarness drives the runner against the in-memory transport.Service, proving
// the harness mechanics (cold/warm/changed/edited/decode) without a real backend.
type memHarness struct{ svc *transport.MemoryService }

func (h memHarness) OpenSession(ctx context.Context) (transport.Session, error) {
	return h.svc.OpenSession(ctx, transport.OpenSessionRequest{
		ModelName: "tiny",
		Path:      "tiny",
		Config:    transport.Config{NumCtx: 4096},
	})
}

func (h memHarness) Turn(stable, suffix string) (transport.PrefixInput, transport.SuffixInput) {
	// The prefix manifest depends only on the stable text, so a changed suffix
	// keeps the stable prefix warm while an edited stable text is a precise miss.
	m := transport.ContextManifest{
		ProfileID:            "bench",
		Backend:              "mem",
		BackendVersion:       "test",
		ModelDigest:          "tiny",
		PromptFormat:         "chatml",
		PromptTemplateDigest: "t",
		RuntimeDigest:        "r",
		StableBytes:          len(stable),
		TotalBytes:           len(stable),
		StableByteHash:       contextasm.HashString(stable),
		Segments: []contextasm.ManifestSegment{
			{Kind: "system", Stable: true, ByteStart: 0, ByteEnd: len(stable), ByteHash: contextasm.HashString(stable)},
		},
	}
	return transport.PrefixInput{Text: stable, Manifest: m}, transport.SuffixInput{Text: suffix, Manifest: m}
}

func TestUnit_BenchReport_DrivesContractAndFillsReport(t *testing.T) {
	h := memHarness{svc: transport.NewMemoryService()}
	sc := Scenario{
		Stable:        "You are a precise coding assistant.\n",
		Suffix:        "fix the bug\n",
		ChangedSuffix: "add a test\n",
		EditedStable:  "You are a TERSE coding assistant.\n",
		SuffixCurve:   []string{"a", "ab", "abc"},
		MaxDecode:     8,
	}

	rep, err := Run(context.Background(), h, sc, Report{Model: "tiny", Backend: "mem", Mode: "live_prefix_reuse"})
	if err != nil {
		t.Fatal(err)
	}

	if rep.ColdFullPrefill.Tokens == 0 {
		t.Fatalf("cold prefill tokens = 0: %+v", rep.ColdFullPrefill)
	}
	if rep.Decode.OutputTokens != sc.MaxDecode {
		t.Fatalf("decode tokens = %d, want %d", rep.Decode.OutputTokens, sc.MaxDecode)
	}
	if rep.WarmSamePrefix.HitRate != 1 {
		t.Fatalf("warm same-prefix hit rate = %v, want 1 (full reuse)", rep.WarmSamePrefix.HitRate)
	}
	if !rep.WarmChangedSuffix.OutputEqualsCold {
		t.Fatalf("warm changed-suffix output != cold: %+v", rep.WarmChangedSuffix)
	}
	if !rep.EditedStable.ActualCacheMiss {
		t.Fatalf("edited stable segment did not miss the cache: %+v", rep.EditedStable)
	}
	if !rep.EditedStable.OutputEqualsCold {
		t.Fatalf("edited stable output != cold reference: %+v", rep.EditedStable)
	}
	if len(rep.SuffixTTFTCurve) != len(sc.SuffixCurve) {
		t.Fatalf("suffix curve points = %d, want %d", len(rep.SuffixTTFTCurve), len(sc.SuffixCurve))
	}
	if !rep.FailureCases.CancelDecode {
		t.Fatalf("cancel-decode failure case not handled")
	}
}
