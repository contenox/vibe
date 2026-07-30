package sessiontest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/modeld/residency"
	"github.com/contenox/contenox/libtransport"
)

// TailColdRoundTripCase supplies backend-specific session construction and
// prompt/decode setup for the shared cold-residency round-trip contract.
type TailColdRoundTripCase struct {
	Open   func(testing.TB) transport.Session
	Build  func(testing.TB, context.Context, transport.Session)
	Decode func(context.Context, transport.Session) (string, error)

	Width           int
	EmptyDecodeSkip string
}

// RunTailColdRoundTrip proves the shared engine primitive needed for cold
// residency: a backend can evict a resident tail range, admit it back, and keep
// deterministic decode output unchanged.
func RunTailColdRoundTrip(t testing.TB, ctx context.Context, tc TailColdRoundTripCase) {
	t.Helper()
	if tc.Open == nil {
		t.Fatal("sessiontest: Open is nil")
	}
	if tc.Build == nil {
		t.Fatal("sessiontest: Build is nil")
	}
	if tc.Decode == nil {
		t.Fatal("sessiontest: Decode is nil")
	}
	width := tc.Width
	if width <= 0 {
		width = 4
	}

	ref := tc.Open(t)
	defer ref.Close()
	tc.Build(t, ctx, ref)

	snap, err := ref.Snapshot(ctx)
	if err != nil {
		t.Fatalf("reference Snapshot: %v", err)
	}
	want, err := tc.Decode(ctx, ref)
	if err != nil {
		t.Fatalf("reference Decode: %v", err)
	}
	if want == "" {
		msg := tc.EmptyDecodeSkip
		if msg == "" {
			msg = "model produced no visible token for the reference continuation"
		}
		t.Skip(msg)
	}

	probe := tc.Open(t)
	defer probe.Close()
	if err := probe.Restore(ctx, snap); err != nil {
		t.Fatalf("probe Restore: %v", err)
	}
	exec, ok := probe.(residency.Executor)
	if !ok {
		t.Fatalf("probe session %T does not implement residency.Executor", probe)
	}
	caps := exec.Capabilities()
	if !caps.ColdStore || !caps.RemoveTail {
		t.Fatalf("expected cold tail residency support, got capabilities %+v", caps)
	}

	before := probe.ExplainContext().ResidentTokens
	if before <= width {
		t.Skipf("not enough resident tokens to evict tail: resident=%d width=%d", before, width)
	}
	r := residency.Range{Start: before - width, End: before}
	if err := exec.EvictRange(ctx, r); err != nil {
		t.Fatalf("EvictRange %+v: %v", r, err)
	}
	if got := probe.ExplainContext().ResidentTokens; got != before-width {
		t.Fatalf("resident after evict = %d, want %d", got, before-width)
	}

	coldSnap, err := probe.Snapshot(ctx)
	if err != nil {
		t.Fatalf("cold Snapshot: %v", err)
	}
	if len(coldSnap.ColdKVBlocks) == 0 {
		t.Fatalf("cold Snapshot captured no cold KV blocks")
	}

	restored := tc.Open(t)
	defer restored.Close()
	if err := restored.Restore(ctx, coldSnap); err != nil {
		t.Fatalf("cold Restore: %v", err)
	}
	exec, ok = restored.(residency.Executor)
	if !ok {
		t.Fatalf("restored session %T does not implement residency.Executor", restored)
	}
	if err := exec.AdmitRange(ctx, r); err != nil {
		t.Fatalf("AdmitRange %+v: %v", r, err)
	}
	if got := restored.ExplainContext().ResidentTokens; got != before {
		t.Fatalf("resident after admit = %d, want restored %d", got, before)
	}

	got, err := tc.Decode(ctx, restored)
	if err != nil {
		t.Fatalf("post-round-trip Decode: %v", err)
	}
	if got != want {
		t.Fatalf("evict/admit tail round trip changed deterministic decode:\n  reference  = %q\n  round-trip = %q", want, got)
	}
}

// DecodeText drains transport.Decode and concatenates text/thinking chunks.
func DecodeText(ctx context.Context, sess transport.Session, cfg transport.DecodeConfig) (string, error) {
	chunks, err := sess.Decode(ctx, cfg)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			return "", chunk.Error
		}
		b.WriteString(chunk.Text)
		b.WriteString(chunk.Thinking)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("decode context: %w", err)
	}
	return b.String(), nil
}
