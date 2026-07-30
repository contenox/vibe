// Package residency contains modeld's backend-neutral KV residency policy.
//
// It deliberately owns only logical decisions: which token ranges should remain
// hot under a derived budget, and which ranges may be moved cold. Backend
// adapters execute those decisions only when their engine exposes the necessary
// KV controls.
package residency

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/contenox/contenox/libcontextasm"
)

// Range is a half-open token range [Start, End).
type Range struct {
	Start int
	End   int
}

func (r Range) Len() int {
	if r.End <= r.Start {
		return 0
	}
	return r.End - r.Start
}

func (r Range) valid() bool {
	return r.Start >= 0 && r.End >= r.Start
}

func (r Range) overlaps(other Range) bool {
	return r.Start < other.End && other.Start < r.End
}

func (r Range) clipped(limit int) Range {
	if limit <= 0 || r.End <= limit {
		return r
	}
	r.End = limit
	if r.Start > r.End {
		r.Start = r.End
	}
	return r
}

// BlockFlags are policy hints applied above CacheClass. Sinks and recent-window
// blocks are protected because sparse/streaming attention requires them hot.
type BlockFlags uint16

const (
	FlagPinned BlockFlags = 1 << iota
	FlagSink
	FlagRecent
	FlagRetrieved
)

func (f BlockFlags) Has(want BlockFlags) bool { return f&want != 0 }

// Block is the planner's unit of residency. Ranges must not overlap.
type Block struct {
	Range        Range
	Kind         string
	Stable       bool
	CacheClass   contextasm.CacheClass
	TokenHash    string
	LastUsed     int64
	Flags        BlockFlags
	Segment      int
	SplitOrdinal int
}

func (b Block) protected() bool {
	return b.Flags.Has(FlagPinned|FlagSink|FlagRecent) || b.CacheClass == contextasm.ClassTaskPinned
}

// ManifestOptions controls block construction from a transport manifest.
type ManifestOptions struct {
	// ResidentTokens limits the manifest to the token range currently resident.
	// This lets callers run the planner after EnsurePrefix before volatile
	// segments have token ranges.
	ResidentTokens int
	// BlockSize splits large manifest segments into uniform logical blocks.
	// A non-positive value keeps each segment as one block.
	BlockSize int
	// LastUsed is copied onto every generated block. Callers that track richer
	// access recency can rewrite LastUsed before planning.
	LastUsed int64
	// RequireComplete reports missing ranges for every non-empty segment. Leave
	// false for prefix-only planning after EnsurePrefix, where volatile suffix
	// ranges have not been tokenized yet.
	RequireComplete bool
}

// MissingTokenRangesError reports non-empty manifest segments that are within
// the resident region but have not yet been backend-tokenized.
type MissingTokenRangesError struct {
	Segments []string
}

func (e *MissingTokenRangesError) Error() string {
	if len(e.Segments) == 0 {
		return "residency: manifest segment token ranges are missing"
	}
	return fmt.Sprintf("residency: manifest segment token ranges are missing for %s", strings.Join(e.Segments, ","))
}

// BlocksFromManifest converts backend-tokenized manifest segments into logical
// residency blocks. Missing CacheClass tags are normalized from kind/stability.
func BlocksFromManifest(m contextasm.ContextManifest, opts ManifestOptions) ([]Block, error) {
	if opts.ResidentTokens < 0 {
		return nil, fmt.Errorf("residency: resident token count must be non-negative")
	}
	blocks := make([]Block, 0, len(m.Segments))
	var missing []string
	for i, seg := range m.Segments {
		if seg.TokenStart == 0 && seg.TokenEnd == 0 {
			if seg.ByteEnd > seg.ByteStart && (opts.RequireComplete || seg.Stable) {
				missing = append(missing, seg.Kind)
			}
			continue
		}
		r := Range{Start: seg.TokenStart, End: seg.TokenEnd}
		if !r.valid() {
			return nil, fmt.Errorf("residency: invalid token range for segment %q: [%d,%d)", seg.Kind, seg.TokenStart, seg.TokenEnd)
		}
		r = r.clipped(opts.ResidentTokens)
		if r.Len() == 0 {
			continue
		}
		base := Block{
			Range:      r,
			Kind:       seg.Kind,
			Stable:     seg.Stable,
			CacheClass: ClassForSegment(seg.Kind, seg.Stable, seg.CacheClass),
			TokenHash:  seg.TokenHash,
			LastUsed:   opts.LastUsed,
			Segment:    i,
		}
		blocks = append(blocks, splitBlock(base, opts.BlockSize)...)
	}
	if len(missing) > 0 {
		return blocks, &MissingTokenRangesError{Segments: missing}
	}
	if err := validateBlocks(blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

func splitBlock(b Block, blockSize int) []Block {
	if blockSize <= 0 || b.Range.Len() <= blockSize {
		return []Block{b}
	}
	out := make([]Block, 0, (b.Range.Len()+blockSize-1)/blockSize)
	ordinal := 0
	for start := b.Range.Start; start < b.Range.End; start += blockSize {
		end := start + blockSize
		if end > b.Range.End {
			end = b.Range.End
		}
		next := b
		next.Range = Range{Start: start, End: end}
		next.SplitOrdinal = ordinal
		out = append(out, next)
		ordinal++
	}
	return out
}

// ClassForSegment returns the explicit manifest cache class when valid, else a
// conservative default derived from segment kind and stable/volatile placement.
func ClassForSegment(kind string, stable bool, explicit string) contextasm.CacheClass {
	if class, ok := ParseCacheClass(explicit); ok {
		return class
	}
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "system", "tools", "repo_rules":
		return contextasm.ClassTaskPinned
	case "repo_map", "pinned":
		return contextasm.ClassRepoMap
	case "diff", "terminal", "user", "assistant", "tool", "assistant_prompt":
		return contextasm.ClassVolatile
	default:
		if stable {
			return contextasm.ClassTaskPinned
		}
		return contextasm.ClassVolatile
	}
}

// ParseCacheClass parses the stable manifest cache-class tag.
func ParseCacheClass(tag string) (contextasm.CacheClass, bool) {
	switch strings.ToLower(strings.TrimSpace(tag)) {
	case contextasm.ClassTaskPinned.Tag():
		return contextasm.ClassTaskPinned, true
	case contextasm.ClassRepoMap.Tag():
		return contextasm.ClassRepoMap, true
	case contextasm.ClassVolatile.Tag():
		return contextasm.ClassVolatile, true
	default:
		return contextasm.ClassVolatile, false
	}
}

// PlanInput is the pure policy input. BudgetTokens must be derived by capacity
// planning, not chosen here.
type PlanInput struct {
	Blocks       []Block
	BudgetTokens int
	// SinkTokens and RecentTokens mark blocks overlapping those token spans as
	// protected. The spans are coarse by design; block splitting controls
	// precision.
	SinkTokens   int
	RecentTokens int
	StreamPolicy StreamPolicy
	Capabilities Capabilities
	// AttentionScores optionally supplies one score per input block. Higher
	// means more important. When present, eviction within each CacheClass drops
	// lower-score blocks first; when absent, ordering remains recency-based.
	AttentionScores []float32
}

type StreamPolicy struct {
	Enabled      bool
	SinkTokens   int
	RecentTokens int
}

// Plan is the planner output: KeepHot plus EvictCold partitions the input
// blocks. HotTokens can exceed BudgetTokens only when protected blocks alone do.
type Plan struct {
	BudgetTokens    int
	TotalTokens     int
	HotTokens       int
	ProtectedTokens int
	OverBudget      bool
	KeepHot         []Block
	EvictCold       []Block
	Diagnostics     []string
}

// PlanHotSet produces the hot/cold partition for a token budget.
func PlanHotSet(in PlanInput) (Plan, error) {
	if in.BudgetTokens < 0 {
		return Plan{}, fmt.Errorf("residency: budget tokens must be non-negative")
	}
	blocks := append([]Block(nil), in.Blocks...)
	if err := validateBlocks(blocks); err != nil {
		return Plan{}, err
	}
	if len(in.AttentionScores) > 0 && len(in.AttentionScores) != len(blocks) {
		return Plan{}, fmt.Errorf("residency: attention score count %d does not match block count %d", len(in.AttentionScores), len(blocks))
	}
	sinkTokens, recentTokens := in.SinkTokens, in.RecentTokens
	if in.StreamPolicy.Enabled {
		sinkTokens = in.StreamPolicy.SinkTokens
		recentTokens = in.StreamPolicy.RecentTokens
	}
	markPolicyFlags(blocks, sinkTokens, recentTokens)

	plan := Plan{BudgetTokens: in.BudgetTokens}
	hot := append([]Block(nil), blocks...)
	for _, b := range hot {
		n := b.Range.Len()
		plan.TotalTokens += n
		if b.protected() {
			plan.ProtectedTokens += n
		}
	}

	if in.StreamPolicy.Enabled {
		return planStreamingHotSet(plan, hot, in), nil
	}

	candidates := make([]Block, 0, len(hot))
	for _, b := range hot {
		if !b.protected() {
			candidates = append(candidates, b)
		}
	}
	sortEvictionCandidates(candidates, scoreByRange(blocks, in.AttentionScores))

	hotTokens := plan.TotalTokens
	evicted := map[Range]bool{}
	for _, b := range candidates {
		if hotTokens <= in.BudgetTokens {
			break
		}
		evicted[b.Range] = true
		hotTokens -= b.Range.Len()
		plan.EvictCold = append(plan.EvictCold, b)
	}
	for _, b := range hot {
		if !evicted[b.Range] {
			plan.KeepHot = append(plan.KeepHot, b)
		}
	}
	sortBlocksByRange(plan.KeepHot)
	sortBlocksByRange(plan.EvictCold)
	plan.HotTokens = hotTokens
	if plan.HotTokens > plan.BudgetTokens {
		plan.OverBudget = true
		plan.Diagnostics = append(plan.Diagnostics, fmt.Sprintf(
			"protected hot set requires %d tokens but budget is %d",
			plan.HotTokens,
			plan.BudgetTokens,
		))
	}
	return plan, nil
}

func planStreamingHotSet(plan Plan, hot []Block, in PlanInput) Plan {
	if !in.Capabilities.RemoveMiddle || !in.Capabilities.PositionShift {
		plan.KeepHot = hot
		sortBlocksByRange(plan.KeepHot)
		plan.HotTokens = plan.TotalTokens
		if plan.HotTokens > plan.BudgetTokens {
			plan.OverBudget = true
		}
		plan.Diagnostics = append(plan.Diagnostics, "stream policy unsupported by backend capabilities")
		return plan
	}
	evicted := map[Range]bool{}
	hotTokens := plan.TotalTokens
	for _, b := range hot {
		if b.protected() {
			continue
		}
		evicted[b.Range] = true
		hotTokens -= b.Range.Len()
		plan.EvictCold = append(plan.EvictCold, b)
	}
	for _, b := range hot {
		if !evicted[b.Range] {
			plan.KeepHot = append(plan.KeepHot, b)
		}
	}
	sortBlocksByRange(plan.KeepHot)
	sortBlocksByRange(plan.EvictCold)
	plan.HotTokens = hotTokens
	if plan.HotTokens > plan.BudgetTokens {
		plan.OverBudget = true
		plan.Diagnostics = append(plan.Diagnostics, fmt.Sprintf(
			"stream hot set requires %d tokens but budget is %d",
			plan.HotTokens,
			plan.BudgetTokens,
		))
	}
	return plan
}

func scoreByRange(blocks []Block, scores []float32) map[Range]float32 {
	if len(scores) == 0 {
		return nil
	}
	out := make(map[Range]float32, len(blocks))
	for i, b := range blocks {
		out[b.Range] = scores[i]
	}
	return out
}

func sortEvictionCandidates(candidates []Block, scores map[Range]float32) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.CacheClass != b.CacheClass {
			return a.CacheClass.MoreEvictableThan(b.CacheClass)
		}
		if scores != nil {
			as, bs := scores[a.Range], scores[b.Range]
			if as != bs {
				return as < bs
			}
		}
		if a.LastUsed != b.LastUsed {
			return a.LastUsed < b.LastUsed
		}
		if a.Range.Start != b.Range.Start {
			return a.Range.Start < b.Range.Start
		}
		return a.Range.End < b.Range.End
	})
}

func markPolicyFlags(blocks []Block, sinkTokens, recentTokens int) {
	total := 0
	for _, b := range blocks {
		if b.Range.End > total {
			total = b.Range.End
		}
	}
	sink := Range{Start: 0, End: max(sinkTokens, 0)}
	recent := Range{Start: max(total-max(recentTokens, 0), 0), End: total}
	for i := range blocks {
		if sink.Len() > 0 && blocks[i].Range.overlaps(sink) {
			blocks[i].Flags |= FlagSink
		}
		if recent.Len() > 0 && blocks[i].Range.overlaps(recent) {
			blocks[i].Flags |= FlagRecent
		}
	}
}

func validateBlocks(blocks []Block) error {
	sortBlocksByRange(blocks)
	var prev *Block
	for i := range blocks {
		if !blocks[i].Range.valid() {
			return fmt.Errorf("residency: invalid block range [%d,%d)", blocks[i].Range.Start, blocks[i].Range.End)
		}
		if blocks[i].Range.Len() == 0 {
			continue
		}
		if prev != nil && prev.Range.overlaps(blocks[i].Range) {
			return fmt.Errorf("residency: overlapping block ranges [%d,%d) and [%d,%d)",
				prev.Range.Start, prev.Range.End, blocks[i].Range.Start, blocks[i].Range.End)
		}
		prev = &blocks[i]
	}
	return nil
}

func sortBlocksByRange(blocks []Block) {
	sort.SliceStable(blocks, func(i, j int) bool {
		if blocks[i].Range.Start != blocks[j].Range.Start {
			return blocks[i].Range.Start < blocks[j].Range.Start
		}
		return blocks[i].Range.End < blocks[j].Range.End
	})
}

// EvictionBudget is the sink/recent/max split a backend uses to bound its hot KV
// while letting generation continue past the physical window. Both adapters
// derive it the same way so llama (imperative slide) and OpenVINO (declarative
// CacheEvictionConfig) enforce one policy.
type EvictionBudget struct {
	SinkTokens   int // always-hot leading tokens (attention sinks)
	RecentTokens int // always-hot trailing window
	MaxTokens    int // hot budget; eviction keeps physical KV within this
}

// Valid reports whether the split can drive an eviction config: non-zero sizes
// with an evictable middle (Max > Sink + Recent).
func (b EvictionBudget) Valid() bool {
	return b.SinkTokens > 0 && b.RecentTokens > 0 && b.MaxTokens > b.SinkTokens+b.RecentTokens
}

// DeriveEvictionBudget splits a served window into attention sinks, a recent
// window, and the total hot budget. It is eviction-algorithm policy
// (attention sinks plus a recent window), not hardware sizing: ~1/16 of the effective eviction window
// as sinks, ~1/4 as the recent window, Max = that window. Sliding-window models
// cap the eviction window at their model-native attention span because older
// windowed-layer KV cannot be useful hot context. blockSize aligns sizes for
// block-based caches (OpenVINO); pass <=1 for token-granular backends (llama).
// Windows too small to split keep everything hot (Valid() is false -> no eviction).
func DeriveEvictionBudget(windowTokens, slidingWindowTokens, blockSize int) EvictionBudget {
	if slidingWindowTokens > 0 && (windowTokens <= 0 || slidingWindowTokens < windowTokens) {
		windowTokens = slidingWindowTokens
	}
	if blockSize < 1 {
		blockSize = 1
	}
	if windowTokens < 4*blockSize {
		return EvictionBudget{RecentTokens: windowTokens, MaxTokens: windowTokens}
	}
	sinks := alignUp(windowTokens/16, blockSize)
	recent := alignUp(windowTokens/4, blockSize)
	// MaxTokens feeds block-based caches (OpenVINO's CacheEvictionConfig.max_cache_size),
	// which reject a value that is not a multiple of the block size. Align down so the
	// hot budget stays within the served/physical window; the guard below keeps it above
	// the sink+recent floor. sinks, recent, and blockSize are all block multiples, so the
	// guarded value is aligned too.
	maxTokens := alignDown(windowTokens, blockSize)
	if maxTokens <= sinks+recent {
		maxTokens = sinks + recent + blockSize
	}
	return EvictionBudget{SinkTokens: sinks, RecentTokens: recent, MaxTokens: maxTokens}
}

func alignUp(n, block int) int {
	if block <= 1 {
		return max(n, 1)
	}
	return max(((n+block-1)/block)*block, block)
}

func alignDown(n, block int) int {
	if block <= 1 {
		return n
	}
	return (n / block) * block
}

// Capabilities describes what a backend adapter can actually execute.
type Capabilities struct {
	RemoveTail                   bool
	RemoveMiddle                 bool
	PositionShift                bool
	SparseAttention              bool
	SlidingWindowAttentionTokens int
	ColdStore                    bool
	RecomputeRange               bool
	AttentionScores              bool
}

// Controller is the optional engine-facing seam. It is intentionally not part
// of runtime/transport.Session.
type Controller interface {
	Capabilities() Capabilities
}

// Executor is implemented only by adapters that can mutate physical KV ranges.
type Executor interface {
	Controller
	EvictRange(ctx context.Context, r Range) error
	AdmitRange(ctx context.Context, r Range) error
}

// AttentionScorer is an optional backend seam for attention-aware eviction.
// Backends that cannot expose scores leave Capabilities.AttentionScores false
// and the planner falls back to recency/class ordering.
type AttentionScorer interface {
	BlockAttentionScores(ctx context.Context, ranges []Range) ([]float32, error)
}
