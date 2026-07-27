package llmresolver_test

import (
	"fmt"
	"testing"

	"github.com/contenox/beam/internal/kernel/llmresolver"
	libmodelprovider "github.com/contenox/beam/internal/models/modelrepo"
)

func stickyCandidates(n int) []libmodelprovider.Provider {
	out := make([]libmodelprovider.Provider, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &libmodelprovider.MockProvider{
			ID:          fmt.Sprintf("p%d", i),
			Name:        fmt.Sprintf("model-%d", i),
			CanChatFlag: true,
			Backends:    []string{fmt.Sprintf("p%d-b0", i), fmt.Sprintf("p%d-b1", i)},
		})
	}
	return out
}

// TestUnit_StickyOrRandom_SameKeySamePick: the whole point of the policy — one
// session key always lands on the same provider+backend while the candidate
// set is stable.
func TestUnit_StickyOrRandom_SameKeySamePick(t *testing.T) {
	candidates := stickyCandidates(4)
	policy := llmresolver.StickyOrRandom("session-abc")

	p0, b0, err := policy(candidates)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 0; i < 20; i++ {
		p, b, err := llmresolver.StickyOrRandom("session-abc")(candidates)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.GetID() != p0.GetID() || b != b0 {
			t.Fatalf("pick moved for a stable candidate set: had %s/%s, got %s/%s", p0.GetID(), b0, p.GetID(), b)
		}
	}
}

// TestUnit_StickyOrRandom_KeysSpread: distinct keys must not all collapse onto
// one backend.
func TestUnit_StickyOrRandom_KeysSpread(t *testing.T) {
	candidates := stickyCandidates(4) // 8 backends total
	picked := map[string]bool{}
	for i := 0; i < 200; i++ {
		_, b, err := llmresolver.StickyOrRandom(fmt.Sprintf("session-%d", i))(candidates)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		picked[b] = true
	}
	if len(picked) < 4 {
		t.Fatalf("200 distinct keys landed on only %d backends — distribution is broken: %v", len(picked), picked)
	}
}

// TestUnit_StickyOrRandom_CandidateChangeDegradesGracefully: rendezvous
// hashing's contract — removing one provider only moves the sessions that were
// pinned to it; everyone else keeps their pick.
func TestUnit_StickyOrRandom_CandidateChangeDegradesGracefully(t *testing.T) {
	full := stickyCandidates(4)
	removedID := "p2"
	reduced := make([]libmodelprovider.Provider, 0, len(full)-1)
	for _, p := range full {
		if p.GetID() != removedID {
			reduced = append(reduced, p)
		}
	}

	moved, stable, onRemoved := 0, 0, 0
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("session-%d", i)
		pFull, bFull, err := llmresolver.StickyOrRandom(key)(full)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		pRed, bRed, err := llmresolver.StickyOrRandom(key)(reduced)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pFull.GetID() == removedID {
			onRemoved++
			continue // these sessions must move; anywhere in the reduced set is fine
		}
		if pFull.GetID() == pRed.GetID() && bFull == bRed {
			stable++
		} else {
			moved++
		}
	}
	if onRemoved == 0 {
		t.Fatal("test is vacuous: no session was pinned to the removed provider")
	}
	if moved != 0 {
		t.Fatalf("%d sessions not pinned to the removed provider moved anyway (stable=%d)", moved, stable)
	}
}

// TestUnit_StickyOrRandom_EmptyKeyAndEmptySet: empty key behaves like Randomly
// (still resolves), and an empty candidate set is the usual typed error.
func TestUnit_StickyOrRandom_EmptyKeyAndEmptySet(t *testing.T) {
	candidates := stickyCandidates(2)
	p, b, err := llmresolver.StickyOrRandom("")(candidates)
	if err != nil {
		t.Fatalf("empty key must behave like Randomly, got error: %v", err)
	}
	if p == nil || b == "" {
		t.Fatal("empty key must still pick a provider and backend")
	}

	if _, _, err := llmresolver.StickyOrRandom("k")(nil); err == nil {
		t.Fatal("empty candidate set must error")
	}
}
