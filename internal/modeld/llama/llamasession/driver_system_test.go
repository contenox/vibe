//go:build llamanode && llamacpp_direct

package llamasession

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/modeld/llama"
)

// TestSystem_LlamaSession_PrefillSuffixOverflowParksToCold proves the residency
// driver end-to-end on the real backend: a turn that would overflow the hot
// window parks the resident volatile context to the cold store and succeeds
// (resident stays <= numCtx) instead of returning ErrContextOverflow. The
// control (no cold budget) overflows on the same input, isolating the driver as
// the cause.
//
// Shape: a large first volatile turn fills the window (one evictable block), then
// a second turn overflows so the driver must park the first block to fit.
func TestSystem_LlamaSession_PrefillSuffixOverflowParksToCold(t *testing.T) {
	modelPath := os.Getenv("CONTENOX_LLAMA_TINY_GGUF")
	requireTinyGGUF(t, modelPath)

	const numCtx = 64
	stable := "system\nYou are concise.\n"
	bigTurn := "user\nalpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi rho sigma tau upsilon.\n"
	smallTurn := "user\none two three four five six seven eight nine ten eleven twelve.\n"

	// run feeds the big turn then the overflowing small turn, returning whether
	// the no-cold control absorbed the sequence and the largest hot resident
	// observed. Tiny test model tokenizers differ enough that the control can
	// overflow on either volatile turn; both are valid "not absorbed" outcomes.
	run := func(t *testing.T, plannerCtx int) (absorbed bool, maxResident int) {
		t.Helper()
		sess, err := New(modelPath, llama.Config{
			NumCtx:                  numCtx,
			PlannerEffectiveContext: plannerCtx,
			NumBatch:                32,
			NumThreads:              1,
			DisableBOS:              true,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		if _, err := sess.EnsurePrefix(ctx, llama.PrefixInput{Text: stable, Manifest: tinyManifest(stable, bigTurn)}); err != nil {
			t.Fatalf("EnsurePrefix: %v", err)
		}
		if _, err := sess.PrefillSuffix(ctx, llama.SuffixInput{Text: bigTurn, Manifest: tinyManifest(stable, bigTurn)}); err != nil {
			if plannerCtx <= numCtx && errors.Is(err, llama.ErrContextOverflow) {
				return false, maxResident
			}
			t.Fatalf("PrefillSuffix(big): %v", err)
		}
		afterBig := sess.ExplainContext().ResidentTokens
		maxResident = afterBig
		t.Logf("plannerCtx=%d: resident after big turn = %d (numCtx=%d)", plannerCtx, afterBig, numCtx)
		if afterBig > numCtx {
			t.Fatalf("big turn already exceeded numCtx (%d > %d); pick a smaller bigTurn", afterBig, numCtx)
		}

		_, err = sess.PrefillSuffix(ctx, llama.SuffixInput{Text: smallTurn, Manifest: tinyManifest(stable, smallTurn)})
		if err != nil {
			if errors.Is(err, llama.ErrContextOverflow) {
				return false, maxResident
			}
			t.Fatalf("PrefillSuffix(small): unexpected error: %v", err)
		}
		if r := sess.ExplainContext().ResidentTokens; r > maxResident {
			maxResident = r
		}
		return true, maxResident
	}

	// Driver on: the overflowing turn parks the big block to cold and is absorbed.
	absorbed, maxResident := run(t, 384)
	if !absorbed {
		t.Fatal("with a cold budget the driver should have parked the resident block to absorb the overflowing turn")
	}
	if maxResident > numCtx {
		t.Fatalf("hot resident %d exceeded numCtx %d despite parking", maxResident, numCtx)
	}

	// Control: no cold budget -> the same overflowing turn must error.
	ctrlAbsorbed, _ := run(t, numCtx)
	if ctrlAbsorbed {
		t.Fatal("control without a cold budget unexpectedly absorbed the overflowing turn; not exercising overflow")
	}
}
