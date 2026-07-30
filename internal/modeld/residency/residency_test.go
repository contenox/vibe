package residency

import (
	"errors"
	"reflect"
	"testing"

	"github.com/contenox/contenox/libcontextasm"
)

func TestUnit_ClassForSegment_DefaultsAndExplicitTags(t *testing.T) {
	if got := ClassForSegment("system", true, ""); got != contextasm.ClassTaskPinned {
		t.Fatalf("system default = %s, want task_pinned", got.Tag())
	}
	if got := ClassForSegment("repo_map", true, ""); got != contextasm.ClassRepoMap {
		t.Fatalf("repo_map default = %s, want repo_map", got.Tag())
	}
	if got := ClassForSegment("assistant", false, ""); got != contextasm.ClassVolatile {
		t.Fatalf("assistant default = %s, want volatile", got.Tag())
	}
	if got := ClassForSegment("unknown", true, ""); got != contextasm.ClassTaskPinned {
		t.Fatalf("unknown stable default = %s, want task_pinned", got.Tag())
	}
	if got := ClassForSegment("system", true, contextasm.ClassVolatile.Tag()); got != contextasm.ClassVolatile {
		t.Fatalf("explicit cache_class should win, got %s", got.Tag())
	}
}

func TestUnit_BlocksFromManifest_SplitsAndDefaultsCacheClass(t *testing.T) {
	m := contextasm.ContextManifest{
		Segments: []contextasm.ManifestSegment{
			{
				Kind:       "system",
				Stable:     true,
				ByteStart:  0,
				ByteEnd:    6,
				TokenStart: 0,
				TokenEnd:   6,
				TokenHash:  "system-tokens",
			},
			{
				Kind:       "repo_map",
				Stable:     true,
				ByteStart:  6,
				ByteEnd:    14,
				TokenStart: 6,
				TokenEnd:   14,
				TokenHash:  "repo-tokens",
			},
			{
				Kind:      "user",
				Stable:    false,
				ByteStart: 14,
				ByteEnd:   18,
				// Volatile segment has no token range yet; ResidentTokens limits
				// planning to the already-prefilled stable prefix.
			},
		},
	}

	blocks, err := BlocksFromManifest(m, ManifestOptions{ResidentTokens: 14, BlockSize: 4, LastUsed: 7})
	if err != nil {
		t.Fatalf("BlocksFromManifest: %v", err)
	}
	if ranges := blockRanges(blocks); !reflect.DeepEqual(ranges, []Range{{0, 4}, {4, 6}, {6, 10}, {10, 14}}) {
		t.Fatalf("ranges = %+v", ranges)
	}
	if blocks[0].CacheClass != contextasm.ClassTaskPinned || blocks[2].CacheClass != contextasm.ClassRepoMap {
		t.Fatalf("cache classes = %s/%s, want task_pinned/repo_map", blocks[0].CacheClass.Tag(), blocks[2].CacheClass.Tag())
	}
	for _, b := range blocks {
		if b.LastUsed != 7 {
			t.Fatalf("LastUsed = %d, want 7", b.LastUsed)
		}
	}
}

func TestUnit_BlocksFromManifest_ReportsMissingResidentTokenRanges(t *testing.T) {
	m := contextasm.ContextManifest{
		Segments: []contextasm.ManifestSegment{
			{Kind: "system", Stable: true, ByteStart: 0, ByteEnd: 6},
		},
	}

	_, err := BlocksFromManifest(m, ManifestOptions{ResidentTokens: 6})
	var missing *MissingTokenRangesError
	if !errors.As(err, &missing) {
		t.Fatalf("BlocksFromManifest err = %v, want MissingTokenRangesError", err)
	}
	if !reflect.DeepEqual(missing.Segments, []string{"system"}) {
		t.Fatalf("missing segments = %+v", missing.Segments)
	}
}

func TestUnit_PlanHotSet_EvictsByClassThenRecency(t *testing.T) {
	blocks := []Block{
		{Range: Range{0, 10}, Kind: "system", CacheClass: contextasm.ClassTaskPinned, LastUsed: 1},
		{Range: Range{10, 20}, Kind: "repo_map", CacheClass: contextasm.ClassRepoMap, LastUsed: 1},
		{Range: Range{20, 30}, Kind: "terminal", CacheClass: contextasm.ClassVolatile, LastUsed: 1},
		{Range: Range{30, 40}, Kind: "user", CacheClass: contextasm.ClassVolatile, LastUsed: 9},
	}

	plan, err := PlanHotSet(PlanInput{Blocks: blocks, BudgetTokens: 15})
	if err != nil {
		t.Fatalf("PlanHotSet: %v", err)
	}
	if plan.OverBudget {
		t.Fatalf("plan unexpectedly over budget: %+v", plan)
	}
	if ranges := blockRanges(plan.KeepHot); !reflect.DeepEqual(ranges, []Range{{0, 10}}) {
		t.Fatalf("keep ranges = %+v, want only task-pinned block", ranges)
	}
	if ranges := blockRanges(plan.EvictCold); !reflect.DeepEqual(ranges, []Range{{10, 20}, {20, 30}, {30, 40}}) {
		t.Fatalf("evict ranges = %+v", ranges)
	}
}

func TestUnit_PlanHotSet_ProtectsSinksAndRecentWindow(t *testing.T) {
	blocks := []Block{
		{Range: Range{0, 10}, Kind: "terminal", CacheClass: contextasm.ClassVolatile},
		{Range: Range{10, 20}, Kind: "terminal", CacheClass: contextasm.ClassVolatile},
		{Range: Range{20, 30}, Kind: "terminal", CacheClass: contextasm.ClassVolatile},
	}

	plan, err := PlanHotSet(PlanInput{Blocks: blocks, BudgetTokens: 5, SinkTokens: 10, RecentTokens: 10})
	if err != nil {
		t.Fatalf("PlanHotSet: %v", err)
	}
	if !plan.OverBudget || len(plan.Diagnostics) == 0 {
		t.Fatalf("expected protected set to exceed budget with diagnostics: %+v", plan)
	}
	if ranges := blockRanges(plan.KeepHot); !reflect.DeepEqual(ranges, []Range{{0, 10}, {20, 30}}) {
		t.Fatalf("keep ranges = %+v, want sink and recent blocks", ranges)
	}
	if ranges := blockRanges(plan.EvictCold); !reflect.DeepEqual(ranges, []Range{{10, 20}}) {
		t.Fatalf("evict ranges = %+v", ranges)
	}
}

func TestUnit_PlanHotSet_StreamPolicyEvictsMiddle(t *testing.T) {
	blocks := []Block{
		{Range: Range{0, 10}, Kind: "terminal", CacheClass: contextasm.ClassVolatile},
		{Range: Range{10, 20}, Kind: "terminal", CacheClass: contextasm.ClassVolatile},
		{Range: Range{20, 30}, Kind: "terminal", CacheClass: contextasm.ClassVolatile},
		{Range: Range{30, 40}, Kind: "terminal", CacheClass: contextasm.ClassVolatile},
	}

	plan, err := PlanHotSet(PlanInput{
		Blocks:       blocks,
		BudgetTokens: 20,
		StreamPolicy: StreamPolicy{
			Enabled:      true,
			SinkTokens:   10,
			RecentTokens: 10,
		},
		Capabilities: Capabilities{RemoveMiddle: true, PositionShift: true},
	})
	if err != nil {
		t.Fatalf("PlanHotSet: %v", err)
	}
	if plan.OverBudget {
		t.Fatalf("streaming plan unexpectedly over budget: %+v", plan)
	}
	if ranges := blockRanges(plan.KeepHot); !reflect.DeepEqual(ranges, []Range{{0, 10}, {30, 40}}) {
		t.Fatalf("keep ranges = %+v, want sink and recent", ranges)
	}
	if ranges := blockRanges(plan.EvictCold); !reflect.DeepEqual(ranges, []Range{{10, 20}, {20, 30}}) {
		t.Fatalf("evict ranges = %+v, want middle", ranges)
	}
}

func TestUnit_PlanHotSet_StreamPolicyUnsupportedDegradesToNoop(t *testing.T) {
	blocks := []Block{
		{Range: Range{0, 10}, Kind: "terminal", CacheClass: contextasm.ClassVolatile},
		{Range: Range{10, 20}, Kind: "terminal", CacheClass: contextasm.ClassVolatile},
		{Range: Range{20, 30}, Kind: "terminal", CacheClass: contextasm.ClassVolatile},
	}

	plan, err := PlanHotSet(PlanInput{
		Blocks:       blocks,
		BudgetTokens: 20,
		StreamPolicy: StreamPolicy{
			Enabled:      true,
			SinkTokens:   10,
			RecentTokens: 10,
		},
		Capabilities: Capabilities{RemoveTail: true},
	})
	if err != nil {
		t.Fatalf("PlanHotSet: %v", err)
	}
	if ranges := blockRanges(plan.KeepHot); !reflect.DeepEqual(ranges, []Range{{0, 10}, {10, 20}, {20, 30}}) {
		t.Fatalf("keep ranges = %+v, want no-op retained set", ranges)
	}
	if len(plan.EvictCold) != 0 || !plan.OverBudget || len(plan.Diagnostics) == 0 {
		t.Fatalf("plan = %+v, want unsupported no-op diagnostic", plan)
	}
}

func TestUnit_PlanHotSet_EvictsLowestAttentionScoreWithinClass(t *testing.T) {
	blocks := []Block{
		{Range: Range{0, 10}, Kind: "terminal", CacheClass: contextasm.ClassVolatile, LastUsed: 1},
		{Range: Range{10, 20}, Kind: "terminal", CacheClass: contextasm.ClassVolatile, LastUsed: 9},
		{Range: Range{20, 30}, Kind: "terminal", CacheClass: contextasm.ClassVolatile, LastUsed: 1},
	}

	plan, err := PlanHotSet(PlanInput{
		Blocks:          blocks,
		BudgetTokens:    20,
		AttentionScores: []float32{0.9, 0.1, 0.5},
		Capabilities:    Capabilities{AttentionScores: true},
	})
	if err != nil {
		t.Fatalf("PlanHotSet: %v", err)
	}
	if ranges := blockRanges(plan.EvictCold); !reflect.DeepEqual(ranges, []Range{{10, 20}}) {
		t.Fatalf("evict ranges = %+v, want lowest-score block", ranges)
	}
	if ranges := blockRanges(plan.KeepHot); !reflect.DeepEqual(ranges, []Range{{0, 10}, {20, 30}}) {
		t.Fatalf("keep ranges = %+v, want higher-score blocks", ranges)
	}
}

func TestUnit_PlanHotSet_RejectsMismatchedAttentionScores(t *testing.T) {
	_, err := PlanHotSet(PlanInput{
		BudgetTokens:    10,
		Blocks:          []Block{{Range: Range{0, 10}, CacheClass: contextasm.ClassVolatile}},
		AttentionScores: []float32{0.1, 0.2},
	})
	if err == nil {
		t.Fatal("PlanHotSet accepted mismatched attention scores")
	}
}

func TestUnit_PlanHotSet_RejectsOverlappingRanges(t *testing.T) {
	_, err := PlanHotSet(PlanInput{
		BudgetTokens: 10,
		Blocks: []Block{
			{Range: Range{0, 10}, CacheClass: contextasm.ClassVolatile},
			{Range: Range{9, 12}, CacheClass: contextasm.ClassVolatile},
		},
	})
	if err == nil {
		t.Fatal("PlanHotSet accepted overlapping ranges")
	}
}

func TestUnit_DeriveEvictionBudget(t *testing.T) {
	// Block-aligned (OpenVINO-style): sizes round up to blockSize, Max = window.
	b := DeriveEvictionBudget(2048, 0, 32)
	if b.SinkTokens%32 != 0 || b.RecentTokens%32 != 0 {
		t.Fatalf("sizes not block-aligned: %+v", b)
	}
	if b.MaxTokens != 2048 || !b.Valid() {
		t.Fatalf("budget = %+v, want valid Max=2048", b)
	}
	if b.SinkTokens >= b.RecentTokens || b.RecentTokens >= b.MaxTokens {
		t.Fatalf("expected sink < recent < max: %+v", b)
	}

	// Token-granular (llama-style): no block rounding.
	tok := DeriveEvictionBudget(400, 0, 1)
	if !tok.Valid() || tok.MaxTokens != 400 {
		t.Fatalf("token budget = %+v", tok)
	}

	// Sliding-window models cap eviction policy to their actual attention span.
	swa := DeriveEvictionBudget(2048, 512, 32)
	if !swa.Valid() || swa.MaxTokens != 512 || swa.RecentTokens >= b.RecentTokens {
		t.Fatalf("SWA budget = %+v, want valid Max=512 and smaller recent window than %+v", swa, b)
	}

	// Window too small to split: everything hot, not Valid (no eviction).
	small := DeriveEvictionBudget(64, 0, 32)
	if small.Valid() || small.MaxTokens != 64 {
		t.Fatalf("small-window budget should keep all hot and be invalid: %+v", small)
	}
}

// TestUnit_DeriveEvictionBudget_BlockAlignsMaxTokens is the B-003 regression: a
// served window that is not a multiple of the block size must still yield a
// block-aligned MaxTokens, because OpenVINO GenAI rejects a
// CacheEvictionConfig.max_cache_size that is not a multiple of the block size.
// MaxTokens is aligned down so it stays within the served window.
func TestUnit_DeriveEvictionBudget_BlockAlignsMaxTokens(t *testing.T) {
	const block = 32
	// Includes the 4940-token turn from the stress run plus other non-aligned windows.
	for _, window := range []int{129, 200, 2000, 4940, 8191} {
		b := DeriveEvictionBudget(window, 0, block)
		if !b.Valid() {
			t.Fatalf("window=%d: budget not valid: %+v", window, b)
		}
		if b.MaxTokens%block != 0 {
			t.Errorf("window=%d: MaxTokens=%d not a multiple of block %d", window, b.MaxTokens, block)
		}
		if b.SinkTokens%block != 0 || b.RecentTokens%block != 0 {
			t.Errorf("window=%d: sink/recent not block-aligned: %+v", window, b)
		}
		if b.MaxTokens > window {
			t.Errorf("window=%d: MaxTokens=%d exceeds served window", window, b.MaxTokens)
		}
	}

	// A non-aligned sliding-window span must also produce an aligned MaxTokens.
	swa := DeriveEvictionBudget(8192, 500, block)
	if !swa.Valid() || swa.MaxTokens%block != 0 {
		t.Fatalf("non-aligned SWA span must yield aligned MaxTokens: %+v", swa)
	}
	if swa.MaxTokens > 500 {
		t.Errorf("SWA MaxTokens=%d exceeds the 500-token attention span", swa.MaxTokens)
	}
}

func blockRanges(blocks []Block) []Range {
	out := make([]Range, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.Range)
	}
	return out
}
