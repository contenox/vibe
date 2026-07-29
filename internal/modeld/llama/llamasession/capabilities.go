// Capability mapping kept in an untagged, cgo-free file so the backend-parity
// contract can pin it verbatim in plain CI: capability drift must fail there,
// not only in tagged native builds.
package llamasession

import "github.com/contenox/contenox/internal/modeld/residency"

// capabilitiesFor is the single source of this backend's residency capability
// truth. llama.cpp exposes sequence-level KV surgery (remove ranges, shift
// RoPE positions), so surgery is always executable; the cold store is
// reported exactly when a cold budget exists.
//
// RecomputeRange stays unreported: evictRangeLocked drops evicted tokens from
// the resident tape (they survive only inside cold blocks), so this backend
// cannot re-prefill an arbitrary evicted range from tokens. Its execution
// strategies are KV surgery and cold-KV import/export.
func capabilitiesFor(sparseAttention bool, slidingWindowAttentionTokens, coldMaxTokens int) residency.Capabilities {
	return residency.Capabilities{
		RemoveTail:                   true,
		RemoveMiddle:                 true,
		PositionShift:                true,
		SparseAttention:              sparseAttention,
		SlidingWindowAttentionTokens: slidingWindowAttentionTokens,
		ColdStore:                    coldMaxTokens > 0,
	}
}
