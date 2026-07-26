package agentservice

import (
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
)

func TestUnit_StampTurnProvenance_StampsOnlyUnstampedMessages(t *testing.T) {
	msgs := []taskengine.Message{
		{ID: "old", Role: "user", Content: "earlier turn", RequestID: "req-old", ChainRef: "old-chain.json"},
		{ID: "u1", Role: "user", Content: "this turn"},
		{ID: "a1", Role: "assistant", Content: "reply"},
	}

	stampTurnProvenance(msgs, "req-new", "default-chain.json")

	if msgs[0].RequestID != "req-old" || msgs[0].ChainRef != "old-chain.json" {
		t.Fatalf("prior-turn provenance must not be overwritten, got %q/%q", msgs[0].RequestID, msgs[0].ChainRef)
	}
	for _, i := range []int{1, 2} {
		if msgs[i].RequestID != "req-new" {
			t.Fatalf("message %d: expected requestID req-new, got %q", i, msgs[i].RequestID)
		}
		if msgs[i].ChainRef != "default-chain.json" {
			t.Fatalf("message %d: expected chainRef default-chain.json, got %q", i, msgs[i].ChainRef)
		}
	}
}

func TestUnit_StampTurnProvenance_EmptyChainRefStillStampsRequest(t *testing.T) {
	msgs := []taskengine.Message{{ID: "u1", Role: "user", Content: "hi"}}
	stampTurnProvenance(msgs, "req-1", "")
	if msgs[0].RequestID != "req-1" {
		t.Fatalf("expected requestID stamped, got %q", msgs[0].RequestID)
	}
	if msgs[0].ChainRef != "" {
		t.Fatalf("expected empty chainRef, got %q", msgs[0].ChainRef)
	}
}
