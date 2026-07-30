// Package benchreport is the common local-node benchmark report: one JSON shape
// emitted across every backend/model/hardware profile so runtime latency and
// warm-reuse claims stay honest. It drives the backend-neutral
// runtime/transport.Session contract (EnsurePrefix / PrefillSuffix / Decode), so
// the same harness measures llama.cpp and OpenVINO. The shape follows
// docs/development/blueprints/local-coding-node-goals.md ("Required benchmark report").
package benchreport

// Report is the full local-node benchmark report. Sections that a given run does
// not exercise are left zero-valued; `Notes` records why.
type Report struct {
	Model          string `json:"model"`
	Backend        string `json:"backend"`
	Mode           string `json:"mode"`              // e.g. "live_prefix_reuse"
	Profile        string `json:"profile,omitempty"` // profile id / manifest digest
	BackendVersion string `json:"backend_version,omitempty"`

	ColdFullPrefill   ColdPrefill  `json:"cold_full_prefill"`
	WarmSamePrefix    WarmSame     `json:"warm_same_prefix"`
	WarmChangedSuffix WarmChanged  `json:"warm_changed_suffix"`
	EditedStable      EditedStable `json:"edited_stable_segment"`
	Decode            Decode       `json:"decode"`
	SuffixTTFTCurve   []SuffixTTFT `json:"suffix_ttft_curve,omitempty"`
	FailureCases      FailureCases `json:"failure_cases"`

	Notes []string `json:"notes,omitempty"`
}

// ColdPrefill measures a fresh prefill of the full stable prefix.
type ColdPrefill struct {
	Tokens    int     `json:"tokens"`
	MS        float64 `json:"ms"`
	PromptTPS float64 `json:"prompt_tps"`
	TTFTMS    float64 `json:"ttft_ms"`
}

// WarmSame measures re-issuing the identical stable prefix on a live session: a
// high HitRate means the resident KV was reused rather than recomputed.
type WarmSame struct {
	CachedTokens int     `json:"cached_tokens"`
	NewTokens    int     `json:"new_tokens"`
	MS           float64 `json:"ms"`
	TTFTMS       float64 `json:"ttft_ms"`
	HitRate      float64 `json:"hit_rate"`
}

// WarmChanged measures keeping the stable prefix warm while re-prefilling a
// changed volatile suffix. OutputEqualsCold proves warm reuse did not corrupt
// the result versus a cold run of the same inputs.
type WarmChanged struct {
	CachedTokens     int     `json:"cached_tokens"`
	SuffixTokens     int     `json:"suffix_tokens"`
	MS               float64 `json:"ms"`
	TTFTMS           float64 `json:"ttft_ms"`
	OutputEqualsCold bool    `json:"output_equals_cold"`
}

// EditedStable proves a changed stable segment is a precise cache miss (the edited
// prefix is recomputed, not falsely reused) and still equals a cold run.
type EditedStable struct {
	ExpectedCacheMiss bool `json:"expected_cache_miss"`
	ActualCacheMiss   bool `json:"actual_cache_miss"`
	OutputEqualsCold  bool `json:"output_equals_cold"`
}

// Decode measures generation throughput.
type Decode struct {
	OutputTokens int     `json:"output_tokens"`
	TokensPerSec float64 `json:"tokens_per_sec"`
}

// SuffixTTFT is one point on the headline curve: TTFT as the changed suffix grows.
type SuffixTTFT struct {
	SuffixTokens int     `json:"suffix_tokens"`
	TTFTMS       float64 `json:"ttft_ms"`
}

// FailureCases records that error paths behave (true = handled as expected).
type FailureCases struct {
	OverContext  bool `json:"over_context"`
	CancelDecode bool `json:"cancel_decode"`
}
