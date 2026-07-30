package transport

import (
	"context"
	"errors"
	"testing"

	"github.com/contenox/contenox/libcontextasm"
)

// manifest builds a ContextManifest with the fields the warm-reuse contract keys
// on: the stable byte hash and the identity digest (profile/template/runtime).
func manifest(stableHash, templateDigest string) ContextManifest {
	return ContextManifest{
		Backend:              "noop",
		PromptFormat:         "chatml",
		PromptTemplateDigest: templateDigest,
		RuntimeDigest:        "rt-1",
		StableByteHash:       stableHash,
		StableTokenHash:      stableHash + "-tok",
	}
}

func openSession(t *testing.T, m *MemoryService, numCtx int) Session {
	t.Helper()
	s, err := m.OpenSession(context.Background(), OpenSessionRequest{Config: Config{NumCtx: numCtx}})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	return s
}

// TestWarmReuse is the headline: the same stable prefix under the same manifest
// is a warm hit on the second EnsurePrefix — the property the whole boundary
// exists for.
func TestWarmReuse(t *testing.T) {
	ctx := context.Background()
	s := openSession(t, NewMemoryService(), 0)
	prefix := PrefixInput{Text: "the stable repo context", Manifest: manifest("h1", "tmpl-1")}

	cold, err := s.EnsurePrefix(ctx, prefix)
	if err != nil {
		t.Fatalf("EnsurePrefix cold: %v", err)
	}
	if cold.ReusedTokens != 0 || cold.PrefilledTokens != cold.PrefixTokens {
		t.Fatalf("first call should be cold: reused=%d prefilled=%d prefix=%d", cold.ReusedTokens, cold.PrefilledTokens, cold.PrefixTokens)
	}

	warm, err := s.EnsurePrefix(ctx, prefix)
	if err != nil {
		t.Fatalf("EnsurePrefix warm: %v", err)
	}
	if warm.ReusedTokens != warm.PrefixTokens || warm.PrefilledTokens != 0 {
		t.Fatalf("second call should be fully warm: reused=%d prefilled=%d prefix=%d", warm.ReusedTokens, warm.PrefilledTokens, warm.PrefixTokens)
	}
}

func TestWarmReuseIgnoresVolatileManifestChange(t *testing.T) {
	ctx := context.Background()
	s := openSession(t, NewMemoryService(), 0)
	firstManifest := manifest("h1", "tmpl-1")
	firstManifest.StableBytes = len("stable")
	firstManifest.TotalBytes = len("stable") + len("turn one")
	firstManifest.Segments = []contextasm.ManifestSegment{
		{Kind: "system", Stable: true, ByteStart: 0, ByteEnd: len("stable"), ByteHash: "stable-one"},
		{Kind: "user", Stable: false, ByteStart: len("stable"), ByteEnd: len("stable") + len("turn one"), ByteHash: "volatile-one"},
	}
	secondManifest := firstManifest
	secondManifest.TotalBytes = len("stable") + len("turn two is longer")
	secondManifest.Segments = []contextasm.ManifestSegment{
		{Kind: "system", Stable: true, ByteStart: 0, ByteEnd: len("stable"), ByteHash: "stable-one"},
		{Kind: "user", Stable: false, ByteStart: len("stable"), ByteEnd: len("stable") + len("turn two is longer"), ByteHash: "volatile-two"},
	}

	if _, err := s.EnsurePrefix(ctx, PrefixInput{Text: "stable", Manifest: firstManifest}); err != nil {
		t.Fatalf("EnsurePrefix first: %v", err)
	}
	warm, err := s.EnsurePrefix(ctx, PrefixInput{Text: "stable", Manifest: secondManifest})
	if err != nil {
		t.Fatalf("EnsurePrefix second: %v", err)
	}
	if warm.ReusedTokens != warm.PrefixTokens || warm.PrefilledTokens != 0 {
		t.Fatalf("volatile-only manifest change should keep prefix warm: %+v", warm)
	}
}

// TestColdOnChangedStableSegment proves an edited stable prefix invalidates.
func TestColdOnChangedStableSegment(t *testing.T) {
	ctx := context.Background()
	s := openSession(t, NewMemoryService(), 0)

	a, _ := s.EnsurePrefix(ctx, PrefixInput{Text: "context A", Manifest: manifest("hA", "tmpl-1")})
	b, err := s.EnsurePrefix(ctx, PrefixInput{Text: "context B longer", Manifest: manifest("hB", "tmpl-1")})
	if err != nil {
		t.Fatalf("EnsurePrefix B: %v", err)
	}
	if b.ReusedTokens != 0 {
		t.Fatalf("changed stable segment must be cold, got reused=%d", b.ReusedTokens)
	}
	if b.DroppedTokens != a.PrefixTokens {
		t.Fatalf("dropped=%d, want the old prefix %d", b.DroppedTokens, a.PrefixTokens)
	}
}

// TestColdOnManifestChange is the manifest-keyed point: identical text but a
// different template digest must NOT reuse stale KV.
func TestColdOnManifestChange(t *testing.T) {
	ctx := context.Background()
	s := openSession(t, NewMemoryService(), 0)
	text := "identical prefix text"

	if _, err := s.EnsurePrefix(ctx, PrefixInput{Text: text, Manifest: manifest("h1", "tmpl-1")}); err != nil {
		t.Fatalf("EnsurePrefix 1: %v", err)
	}
	// Same stable byte hash, different template digest -> different identity.
	st, err := s.EnsurePrefix(ctx, PrefixInput{Text: text, Manifest: manifest("h1", "tmpl-2")})
	if err != nil {
		t.Fatalf("EnsurePrefix 2: %v", err)
	}
	if st.ReusedTokens != 0 {
		t.Fatalf("template change must invalidate reuse, got reused=%d", st.ReusedTokens)
	}
}

// TestSuffixThenDecode drives the full hot loop and checks resident accounting.
func TestSuffixThenDecode(t *testing.T) {
	ctx := context.Background()
	s := openSession(t, NewMemoryService(), 0)

	pre, _ := s.EnsurePrefix(ctx, PrefixInput{Text: "stable", Manifest: manifest("h1", "tmpl-1")})
	suf, err := s.PrefillSuffix(ctx, SuffixInput{Text: "user turn", Manifest: manifest("h1", "tmpl-1")})
	if err != nil {
		t.Fatalf("PrefillSuffix: %v", err)
	}
	if suf.ResidentTokens != pre.PrefixTokens+suf.SuffixTokens {
		t.Fatalf("resident=%d, want prefix %d + suffix %d", suf.ResidentTokens, pre.PrefixTokens, suf.SuffixTokens)
	}

	gen, err := s.Decode(ctx, DecodeConfig{MaxTokens: 5})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	var got int
	for chunk := range gen {
		if chunk.Error != nil {
			t.Fatalf("decode chunk error: %v", chunk.Error)
		}
		got++
	}
	if got != 5 {
		t.Fatalf("decoded %d chunks, want 5", got)
	}

	rep := s.ExplainContext()
	if rep.ResidentTokens != suf.ResidentTokens {
		t.Fatalf("ExplainContext resident=%d, want %d", rep.ResidentTokens, suf.ResidentTokens)
	}
}

// TestContextOverflow proves a prefix larger than the window is rejected.
func TestContextOverflow(t *testing.T) {
	ctx := context.Background()
	s := openSession(t, NewMemoryService(), 4)
	_, err := s.EnsurePrefix(ctx, PrefixInput{Text: "way too many runes", Manifest: manifest("h1", "tmpl-1")})
	if !errors.Is(err, ErrContextOverflow) {
		t.Fatalf("err = %v, want ErrContextOverflow", err)
	}
}

func TestDescribeReportsContextBudgetFields(t *testing.T) {
	info, err := NewMemoryService().Describe(context.Background(), OpenSessionRequest{Config: Config{NumCtx: 128}})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info.EffectiveContext != 128 || info.HotContextTokens != 128 || info.PlannerEffectiveContext != 128 {
		t.Fatalf("context budget fields = effective %d hot %d planner %d, want 128/128/128",
			info.EffectiveContext, info.HotContextTokens, info.PlannerEffectiveContext)
	}
	if info.MemoryContextTokens != 0 {
		t.Fatalf("MemoryContextTokens = %d, want 0 for memory service with unknown KV", info.MemoryContextTokens)
	}
}

// TestClosedSession proves a closed session refuses work.
func TestClosedSession(t *testing.T) {
	ctx := context.Background()
	s := openSession(t, NewMemoryService(), 0)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.EnsurePrefix(ctx, PrefixInput{Text: "x", Manifest: manifest("h1", "tmpl-1")}); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("err = %v, want ErrSessionClosed", err)
	}
}

// TestDecodeCancel proves a cancelled context ends the stream with an error.
func TestDecodeCancel(t *testing.T) {
	s := openSession(t, NewMemoryService(), 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gen, err := s.Decode(ctx, DecodeConfig{MaxTokens: 100})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	chunk, ok := <-gen
	if !ok {
		t.Fatal("stream closed with no response; want a cancellation error")
	}
	if !errors.Is(chunk.Error, context.Canceled) {
		t.Fatalf("first chunk error = %v, want context.Canceled", chunk.Error)
	}
}

// TestFenceAtOpen proves the fence is enforced when opening a session.
func TestFenceAtOpen(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryService(WithOwnerFence("owner-A"))

	if _, err := m.OpenSession(ctx, OpenSessionRequest{Fence: Fence{OwnerInstanceID: "owner-A"}}); err != nil {
		t.Fatalf("OpenSession with matching fence: %v", err)
	}
	if _, err := m.OpenSession(ctx, OpenSessionRequest{Fence: Fence{OwnerInstanceID: "owner-B"}}); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("wrong fence err = %v, want ErrStaleFence", err)
	}
	if _, err := m.OpenSession(ctx, OpenSessionRequest{}); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("empty fence err = %v, want ErrStaleFence", err)
	}
}

// TestFenceDisabledByDefault proves the unwired path stays simple.
func TestFenceDisabledByDefault(t *testing.T) {
	ctx := context.Background()
	if _, err := NewMemoryService().OpenSession(ctx, OpenSessionRequest{}); err != nil {
		t.Fatalf("OpenSession with no fence on unfenced service: %v", err)
	}
}
