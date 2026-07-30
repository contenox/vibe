//go:build llamanode && llamacpp_direct

package llamasession

import (
	"context"
	"fmt"
	"sort"

	"github.com/contenox/contenox/internal/modeld/llama"
	"github.com/contenox/contenox/internal/modeld/residency"
	"github.com/contenox/contenox/libcontextasm"
)

type coldBlock struct {
	Range      residency.Range
	Tokens     []int
	KV         []byte
	TokenHash  string
	CacheClass contextasm.CacheClass
	LastUsed   int64
}

const coldScratchSeqID = 1

func (s *session) coldEnabledLocked() bool {
	return s.coldMaxTokens > 0
}

func (s *session) clearColdStoreLocked() {
	s.coldTokens = 0
	s.coldClock = 0
	s.coldBlocks = nil
	s.coldRangeKey = nil
}

func (s *session) exportColdBlockLocked(a, b int) (*coldBlock, error) {
	if !s.coldEnabledLocked() {
		return nil, nil
	}
	if b <= a {
		return nil, nil
	}
	tokens := append([]int(nil), s.resident[a:b]...)
	if len(tokens) > s.coldMaxTokens {
		return nil, nil
	}
	if err := s.clearColdScratchLocked(); err != nil {
		return nil, err
	}
	if err := s.copyKVSeq(0, coldScratchSeqID, a, b); err != nil {
		return nil, err
	}
	kv, err := s.lctx.StateSeqGetData(coldScratchSeqID)
	cleanupErr := s.clearColdScratchLocked()
	if err != nil {
		if cleanupErr != nil {
			return nil, cleanupErr
		}
		return nil, fmt.Errorf("%w: llamasession export cold kv [%d,%d): %v", llama.ErrSessionFatal, a, b, err)
	}
	if cleanupErr != nil {
		return nil, cleanupErr
	}

	return &coldBlock{
		Range:      residency.Range{Start: a, End: b},
		Tokens:     tokens,
		KV:         kv,
		TokenHash:  contextasm.HashTokenIDs(tokens),
		CacheClass: s.cacheClassForRangeLocked(residency.Range{Start: a, End: b}),
	}, nil
}

func (s *session) storeColdBlockLocked(block *coldBlock) {
	if block == nil || len(block.Tokens) == 0 || len(block.KV) == 0 || !s.coldEnabledLocked() {
		return
	}
	if s.coldBlocks == nil {
		s.coldBlocks = map[string]*coldBlock{}
		s.coldRangeKey = map[string]string{}
	}
	key := coldBlockKey(block.Range, block.TokenHash)
	if old := s.coldBlocks[key]; old != nil {
		s.coldTokens -= len(old.Tokens)
	}
	s.coldClock++
	block.LastUsed = s.coldClock
	s.coldBlocks[key] = block
	s.coldRangeKey[coldRangeKey(block.Range)] = key
	s.coldTokens += len(block.Tokens)
	s.trimColdStoreLocked(key)
}

func (s *session) trimColdStoreLocked(protectedKey string) {
	for s.coldTokens > s.coldMaxTokens {
		victimKey := s.coldVictimLocked(protectedKey)
		if victimKey == "" {
			return
		}
		s.deleteColdBlockLocked(victimKey)
	}
}

func (s *session) coldVictimLocked(protectedKey string) string {
	type candidate struct {
		key string
		b   *coldBlock
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

func (s *session) coldBlockSnapshotsLocked() []llama.ColdKVBlock {
	if len(s.coldBlocks) == 0 {
		return nil
	}
	keys := make([]string, 0, len(s.coldBlocks))
	for key := range s.coldBlocks {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]llama.ColdKVBlock, 0, len(keys))
	for _, key := range keys {
		block := s.coldBlocks[key]
		if block == nil {
			continue
		}
		out = append(out, llama.ColdKVBlock{
			Start:      block.Range.Start,
			End:        block.Range.End,
			Tokens:     append([]int(nil), block.Tokens...),
			TokenHash:  block.TokenHash,
			KV:         append([]byte(nil), block.KV...),
			CacheClass: block.CacheClass.Tag(),
			LastUsed:   block.LastUsed,
		})
	}
	return out
}

func (s *session) restoreColdBlocksLocked(snaps []llama.ColdKVBlock) error {
	s.clearColdStoreLocked()
	if len(snaps) == 0 {
		return nil
	}
	if !s.coldEnabledLocked() {
		return fmt.Errorf("%w: llamasession restore snapshot contains cold KV blocks but cold store is unavailable", llama.ErrUnsupportedFeature)
	}
	for _, snap := range snaps {
		block, err := coldBlockFromSnapshot(snap)
		if err != nil {
			return err
		}
		if len(block.Tokens) > s.coldMaxTokens || s.coldTokens+len(block.Tokens) > s.coldMaxTokens {
			return llama.NewContextOverflowError("restore cold kv", s.coldTokens, len(block.Tokens), s.coldMaxTokens)
		}
		s.restoreColdBlockLocked(block)
	}
	return nil
}

func (s *session) restoreColdBlockLocked(block *coldBlock) {
	if s.coldBlocks == nil {
		s.coldBlocks = map[string]*coldBlock{}
		s.coldRangeKey = map[string]string{}
	}
	key := coldBlockKey(block.Range, block.TokenHash)
	if old := s.coldBlocks[key]; old != nil {
		s.coldTokens -= len(old.Tokens)
	}
	s.coldBlocks[key] = block
	s.coldRangeKey[coldRangeKey(block.Range)] = key
	s.coldTokens += len(block.Tokens)
	if block.LastUsed > s.coldClock {
		s.coldClock = block.LastUsed
	}
}

func coldBlockFromSnapshot(snap llama.ColdKVBlock) (*coldBlock, error) {
	if snap.End <= snap.Start {
		return nil, fmt.Errorf("llamasession restore invalid cold KV range [%d,%d)", snap.Start, snap.End)
	}
	if len(snap.Tokens) != snap.End-snap.Start {
		return nil, fmt.Errorf("llamasession restore invalid cold KV token count: range=[%d,%d) tokens=%d", snap.Start, snap.End, len(snap.Tokens))
	}
	if len(snap.KV) == 0 {
		return nil, fmt.Errorf("llamasession restore invalid cold KV block [%d,%d): empty state", snap.Start, snap.End)
	}
	hash := contextasm.HashTokenIDs(snap.Tokens)
	if snap.TokenHash != "" && snap.TokenHash != hash {
		return nil, fmt.Errorf("llamasession restore invalid cold KV block [%d,%d): token hash mismatch", snap.Start, snap.End)
	}
	class := contextasm.ClassVolatile
	if snap.CacheClass != "" {
		parsed, ok := residency.ParseCacheClass(snap.CacheClass)
		if !ok {
			return nil, fmt.Errorf("llamasession restore invalid cold KV cache class %q", snap.CacheClass)
		}
		class = parsed
	}
	return &coldBlock{
		Range:      residency.Range{Start: snap.Start, End: snap.End},
		Tokens:     append([]int(nil), snap.Tokens...),
		KV:         append([]byte(nil), snap.KV...),
		TokenHash:  hash,
		CacheClass: class,
		LastUsed:   snap.LastUsed,
	}, nil
}

func (s *session) admitRangeLocked(ctx context.Context, r residency.Range) error {
	if !s.coldEnabledLocked() {
		return fmt.Errorf("%w: llamasession admit range [%d,%d) requires a cold KV store", llama.ErrUnsupportedFeature, r.Start, r.End)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	key := s.coldRangeKey[coldRangeKey(r)]
	block := s.coldBlocks[key]
	if block == nil {
		return fmt.Errorf("%w: llamasession no cold KV block for range [%d,%d)", llama.ErrUnsupportedFeature, r.Start, r.End)
	}
	if len(s.resident)+len(block.Tokens) > s.numCtx {
		return llama.NewContextOverflowError("admit", len(s.resident), len(block.Tokens), s.numCtx)
	}
	if err := s.clearColdScratchLocked(); err != nil {
		return err
	}
	if err := s.lctx.StateSeqSetData(coldScratchSeqID, block.KV); err != nil {
		_ = s.clearColdScratchLocked()
		s.closeLocked()
		return fmt.Errorf("%w: llamasession restore cold kv [%d,%d): %v", llama.ErrSessionFatal, r.Start, r.End, err)
	}
	destStart := len(s.resident)
	destEnd := destStart + len(block.Tokens)
	if delta := destStart - block.Range.Start; delta != 0 {
		if err := s.addKVSeq(coldScratchSeqID, block.Range.Start, block.Range.End, delta); err != nil {
			_ = s.clearColdScratchLocked()
			return err
		}
	}
	if err := s.copyKVSeq(coldScratchSeqID, 0, destStart, destEnd); err != nil {
		_ = s.clearColdScratchLocked()
		return err
	}
	if err := s.clearColdScratchLocked(); err != nil {
		return err
	}
	s.resident = append(s.resident, block.Tokens...)
	s.deleteColdBlockLocked(key)
	return nil
}

func (s *session) clearColdScratchLocked() error {
	if s.lctx == nil {
		s.closeLocked()
		return fmt.Errorf("%w: llamasession context is nil during cold kv scratch cleanup", llama.ErrSessionFatal)
	}
	if !s.lctx.MemorySeqRemove(coldScratchSeqID, -1, -1) {
		s.closeLocked()
		return fmt.Errorf("%w: llamasession cold kv scratch cleanup failed seq=%d", llama.ErrSessionFatal, coldScratchSeqID)
	}
	return nil
}

func (s *session) deleteColdBlockLocked(key string) {
	block := s.coldBlocks[key]
	if block == nil {
		return
	}
	delete(s.coldBlocks, key)
	delete(s.coldRangeKey, coldRangeKey(block.Range))
	s.coldTokens -= len(block.Tokens)
	if s.coldTokens < 0 {
		s.coldTokens = 0
	}
}

func (s *session) cacheClassForRangeLocked(r residency.Range) contextasm.CacheClass {
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

func coldBlockKey(r residency.Range, hash string) string {
	return fmt.Sprintf("%d:%d:%s", r.Start, r.End, hash)
}

func coldRangeKey(r residency.Range) string {
	return fmt.Sprintf("%d:%d", r.Start, r.End)
}
