package openvino

import (
	"context"
	"fmt"
	"sort"

	"github.com/contenox/contenox/internal/modeld/openvino/ovsession"
	"github.com/contenox/contenox/internal/modeld/residency"
	"github.com/contenox/contenox/libcontextasm"
	"github.com/contenox/contenox/libtransport"
)

type genaiColdKVBackend interface {
	SupportsColdKV() bool
	ExportColdKV(context.Context, ovsession.ColdKVRange) ([]byte, error)
	ImportColdKV(context.Context, ovsession.ColdKVRange, []byte) error
}

type openvinoColdBlock struct {
	Range        residency.Range
	Tokens       []int
	PrefixTokens []int
	TokenHash    string
	KV           []byte
	CacheClass   contextasm.CacheClass
	LastUsed     int64
}

// EvictRange moves a logical token range from OpenVINO hot KV to the host cold
// store when the backend exposes KV import/export.
func (s *genaiSession) EvictRange(ctx context.Context, r residency.Range) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closedErrLocked()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.evictRangeLocked(ctx, r)
}

// evictRangeLocked is EvictRange's body without the lock, so the overflow driver
// can park ranges from inside an already-locked prefill path (sync.Mutex is not
// reentrant).
func (s *genaiSession) evictRangeLocked(ctx context.Context, r residency.Range) error {
	if !s.coldEnabledLocked() {
		return fmt.Errorf("%w: openvino evict range [%d,%d) requires a cold KV backend", transport.ErrUnsupportedFeature, r.Start, r.End)
	}
	n := len(s.resident)
	if r.Start < 0 || r.End > n || r.Start >= r.End {
		return fmt.Errorf("openvino: evict range [%d,%d) outside resident [0,%d)", r.Start, r.End, n)
	}
	block, err := s.exportColdBlockLocked(ctx, r.Start, r.End)
	if err != nil {
		return err
	}
	oldPrefix := s.prefixLen
	newResident := append(append([]int(nil), s.resident[:r.Start]...), s.resident[r.End:]...)
	newPrefixLen := min(r.Start, oldPrefix) + max(0, oldPrefix-r.End)
	if err := s.prefillResidentLocked(ctx, newResident, fmt.Sprintf("openvino prefill after evict range [%d,%d)", r.Start, r.End)); err != nil {
		return err
	}
	s.resident = newResident
	s.prefixLen = newPrefixLen
	if r.Start < oldPrefix {
		s.prefixText = ""
	}
	if block != nil {
		s.storeColdBlockLocked(block)
	}
	s.updateResidencyPlanLocked(true)
	return nil
}

// AdmitRange restores a previously evicted OpenVINO cold KV block at the hot
// tail. Shifted admits are imported into destination prefix-cache blocks with
// native RoPE key rotation in the backend.
func (s *genaiSession) AdmitRange(ctx context.Context, r residency.Range) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closedErrLocked()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.admitRangeLocked(ctx, r)
}

func (s *genaiSession) coldKVBackend() genaiColdKVBackend {
	backend, ok := s.backend.(genaiColdKVBackend)
	if !ok || !backend.SupportsColdKV() {
		return nil
	}
	return backend
}

func (s *genaiSession) coldEnabledLocked() bool {
	return s.coldMaxTokens > 0 && s.coldKVLossless && s.coldKVBackend() != nil
}

func (s *genaiSession) clearColdStoreLocked() {
	s.coldTokens = 0
	s.coldClock = 0
	s.coldBlocks = nil
	s.coldRangeKey = nil
}

func (s *genaiSession) exportColdBlockLocked(ctx context.Context, a, b int) (*openvinoColdBlock, error) {
	if !s.coldEnabledLocked() {
		return nil, nil
	}
	if b <= a {
		return nil, nil
	}
	tokens := append([]int(nil), s.resident[a:b]...)
	prefixTokens := append([]int(nil), s.resident...)
	if len(tokens) > s.coldMaxTokens {
		return nil, nil
	}
	tokenHash := contextasm.HashTokenIDs(tokens)
	r := residency.Range{Start: a, End: b}
	kv, err := s.coldKVBackend().ExportColdKV(ctx, ovsession.ColdKVRange{
		Start:        a,
		End:          b,
		Tokens:       append([]int(nil), tokens...),
		PrefixTokens: prefixTokens,
		TokenHash:    tokenHash,
	})
	if err != nil {
		return nil, s.backendErrorLocked(fmt.Sprintf("openvino export cold kv [%d,%d)", a, b), err)
	}
	if len(kv) == 0 {
		return nil, fmt.Errorf("%w: openvino export cold kv [%d,%d) returned empty data", transport.ErrSessionFatal, a, b)
	}
	return &openvinoColdBlock{
		Range:        r,
		Tokens:       tokens,
		PrefixTokens: prefixTokens,
		TokenHash:    tokenHash,
		KV:           kv,
		CacheClass:   s.cacheClassForRangeLocked(r),
	}, nil
}

func (s *genaiSession) storeColdBlockLocked(block *openvinoColdBlock) {
	if block == nil || len(block.Tokens) == 0 || len(block.KV) == 0 || !s.coldEnabledLocked() {
		return
	}
	if s.coldBlocks == nil {
		s.coldBlocks = map[string]*openvinoColdBlock{}
		s.coldRangeKey = map[string]string{}
	}
	key := openvinoColdBlockKey(block.Range, block.TokenHash)
	if old := s.coldBlocks[key]; old != nil {
		s.coldTokens -= len(old.Tokens)
	}
	s.coldClock++
	block.LastUsed = s.coldClock
	s.coldBlocks[key] = block
	s.coldRangeKey[openvinoColdRangeKey(block.Range)] = key
	s.coldTokens += len(block.Tokens)
	s.trimColdStoreLocked(key)
}

func (s *genaiSession) trimColdStoreLocked(protectedKey string) {
	for s.coldTokens > s.coldMaxTokens {
		victimKey := s.coldVictimLocked(protectedKey)
		if victimKey == "" {
			return
		}
		s.deleteColdBlockLocked(victimKey)
	}
}

func (s *genaiSession) coldVictimLocked(protectedKey string) string {
	type candidate struct {
		key string
		b   *openvinoColdBlock
	}
	candidates := make([]candidate, 0, len(s.coldBlocks))
	for key, block := range s.coldBlocks {
		if key == protectedKey {
			continue
		}
		candidates = append(candidates, candidate{key: key, b: block})
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i].b, candidates[j].b
		if a.CacheClass != b.CacheClass {
			return a.CacheClass.MoreEvictableThan(b.CacheClass)
		}
		if a.LastUsed != b.LastUsed {
			return a.LastUsed < b.LastUsed
		}
		if a.Range.Start != b.Range.Start {
			return a.Range.Start < b.Range.Start
		}
		return a.Range.End < b.Range.End
	})
	return candidates[0].key
}

func (s *genaiSession) coldBlockSnapshotsLocked() []transport.ColdKVBlock {
	if len(s.coldBlocks) == 0 {
		return nil
	}
	keys := make([]string, 0, len(s.coldBlocks))
	for key := range s.coldBlocks {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]transport.ColdKVBlock, 0, len(keys))
	for _, key := range keys {
		block := s.coldBlocks[key]
		if block == nil {
			continue
		}
		out = append(out, transport.ColdKVBlock{
			Start:        block.Range.Start,
			End:          block.Range.End,
			Tokens:       append([]int(nil), block.Tokens...),
			PrefixTokens: append([]int(nil), block.PrefixTokens...),
			TokenHash:    block.TokenHash,
			KV:           append([]byte(nil), block.KV...),
			CacheClass:   block.CacheClass.Tag(),
			LastUsed:     block.LastUsed,
		})
	}
	return out
}

func (s *genaiSession) restoreColdBlocksLocked(snaps []transport.ColdKVBlock) error {
	s.clearColdStoreLocked()
	if len(snaps) == 0 {
		return nil
	}
	if !s.coldEnabledLocked() {
		return fmt.Errorf("%w: openvino restore snapshot contains cold KV blocks but cold store is unavailable", transport.ErrUnsupportedFeature)
	}
	for _, snap := range snaps {
		block, err := openvinoColdBlockFromSnapshot(snap)
		if err != nil {
			return err
		}
		if len(block.Tokens) > s.coldMaxTokens || s.coldTokens+len(block.Tokens) > s.coldMaxTokens {
			return transport.ErrContextOverflow
		}
		s.restoreColdBlockLocked(block)
	}
	return nil
}

func (s *genaiSession) restoreColdBlockLocked(block *openvinoColdBlock) {
	if s.coldBlocks == nil {
		s.coldBlocks = map[string]*openvinoColdBlock{}
		s.coldRangeKey = map[string]string{}
	}
	key := openvinoColdBlockKey(block.Range, block.TokenHash)
	if old := s.coldBlocks[key]; old != nil {
		s.coldTokens -= len(old.Tokens)
	}
	s.coldBlocks[key] = block
	s.coldRangeKey[openvinoColdRangeKey(block.Range)] = key
	s.coldTokens += len(block.Tokens)
	if block.LastUsed > s.coldClock {
		s.coldClock = block.LastUsed
	}
}

func openvinoColdBlockFromSnapshot(snap transport.ColdKVBlock) (*openvinoColdBlock, error) {
	if snap.End <= snap.Start {
		return nil, fmt.Errorf("openvino restore invalid cold KV range [%d,%d)", snap.Start, snap.End)
	}
	if len(snap.Tokens) != snap.End-snap.Start {
		return nil, fmt.Errorf("openvino restore invalid cold KV token count: range=[%d,%d) tokens=%d", snap.Start, snap.End, len(snap.Tokens))
	}
	if len(snap.KV) == 0 {
		return nil, fmt.Errorf("openvino restore invalid cold KV block [%d,%d): empty state", snap.Start, snap.End)
	}
	hash := contextasm.HashTokenIDs(snap.Tokens)
	if snap.TokenHash != "" && snap.TokenHash != hash {
		return nil, fmt.Errorf("openvino restore invalid cold KV block [%d,%d): token hash mismatch", snap.Start, snap.End)
	}
	class := contextasm.ClassVolatile
	if snap.CacheClass != "" {
		parsed, ok := residency.ParseCacheClass(snap.CacheClass)
		if !ok {
			return nil, fmt.Errorf("openvino restore invalid cold KV cache class %q", snap.CacheClass)
		}
		class = parsed
	}
	return &openvinoColdBlock{
		Range:        residency.Range{Start: snap.Start, End: snap.End},
		Tokens:       append([]int(nil), snap.Tokens...),
		PrefixTokens: append([]int(nil), snap.PrefixTokens...),
		TokenHash:    hash,
		KV:           append([]byte(nil), snap.KV...),
		CacheClass:   class,
		LastUsed:     snap.LastUsed,
	}, nil
}

func (s *genaiSession) admitRangeLocked(ctx context.Context, r residency.Range) error {
	if !s.coldEnabledLocked() {
		return fmt.Errorf("%w: openvino admit range [%d,%d) requires a cold KV backend", transport.ErrUnsupportedFeature, r.Start, r.End)
	}
	key := s.coldRangeKey[openvinoColdRangeKey(r)]
	block := s.coldBlocks[key]
	if block == nil {
		return fmt.Errorf("%w: openvino no cold KV block for range [%d,%d)", transport.ErrUnsupportedFeature, r.Start, r.End)
	}
	if len(s.resident)+len(block.Tokens) > s.numCtx {
		return transport.ErrContextOverflow
	}

	destStart := len(s.resident)
	newResident := append(append([]int(nil), s.resident...), block.Tokens...)
	if err := s.coldKVBackend().ImportColdKV(ctx, ovsession.ColdKVRange{
		Start:        block.Range.Start,
		End:          block.Range.End,
		DestStart:    destStart,
		Tokens:       append([]int(nil), block.Tokens...),
		PrefixTokens: append([]int(nil), newResident...),
		TokenHash:    block.TokenHash,
	}, block.KV); err != nil {
		return s.backendErrorLocked(fmt.Sprintf("openvino import cold kv [%d,%d) -> %d", r.Start, r.End, destStart), err)
	}
	s.resident = newResident
	s.deleteColdBlockLocked(key)
	s.updateResidencyPlanLocked(true)
	return nil
}

func (s *genaiSession) deleteColdBlockLocked(key string) {
	block := s.coldBlocks[key]
	if block == nil {
		return
	}
	delete(s.coldBlocks, key)
	delete(s.coldRangeKey, openvinoColdRangeKey(block.Range))
	s.coldTokens -= len(block.Tokens)
	if s.coldTokens < 0 {
		s.coldTokens = 0
	}
}

func (s *genaiSession) cacheClassForRangeLocked(r residency.Range) contextasm.CacheClass {
	class := contextasm.ClassVolatile
	for _, block := range s.residencyPlan.KeepHot {
		if block.Range.Start < r.End && r.Start < block.Range.End {
			if class.MoreEvictableThan(block.CacheClass) {
				class = block.CacheClass
			}
		}
	}
	for _, block := range s.residencyPlan.EvictCold {
		if block.Range.Start < r.End && r.Start < block.Range.End {
			if class.MoreEvictableThan(block.CacheClass) {
				class = block.CacheClass
			}
		}
	}
	return class
}

func openvinoColdBlockKey(r residency.Range, hash string) string {
	return fmt.Sprintf("%d:%d:%s", r.Start, r.End, hash)
}

func openvinoColdRangeKey(r residency.Range) string {
	return fmt.Sprintf("%d:%d", r.Start, r.End)
}
