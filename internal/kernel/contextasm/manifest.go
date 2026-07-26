package contextasm

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var ErrManifestMismatch = errors.New("contextasm: context manifest mismatch")

// ManifestMismatchError carries the exact reason a suffix/prefix could not be
// safely paired with resident KV.
type ManifestMismatchError struct {
	Reason string
}

func (e *ManifestMismatchError) Error() string {
	if e.Reason == "" {
		return ErrManifestMismatch.Error()
	}
	return fmt.Sprintf("%s: %s", ErrManifestMismatch, e.Reason)
}

func (e *ManifestMismatchError) Is(target error) bool {
	return target == ErrManifestMismatch
}

func NewManifestMismatchError(reason string) error {
	reason = trimManifestMismatchPrefix(reason)
	return &ManifestMismatchError{Reason: reason}
}

func trimManifestMismatchPrefix(reason string) string {
	reason = strings.TrimSpace(reason)
	prefix := ErrManifestMismatch.Error()
	for {
		if reason == prefix {
			return ""
		}
		next, ok := strings.CutPrefix(reason, prefix+": ")
		if !ok {
			return reason
		}
		reason = strings.TrimSpace(next)
	}
}

// ManifestSegment identifies one rendered prompt segment. Byte hashes are
// available before tokenization; token hashes are filled only when a backend can
// tokenize under the actual model/profile.
type ManifestSegment struct {
	Kind   string `json:"kind"`
	Stable bool   `json:"stable"`
	// CacheClass is the coding-aware retention priority (see contextasm.CacheClass):
	// "task_pinned" / "repo_map" / "volatile". Drives budget-aware admission/eviction.
	CacheClass string `json:"cache_class,omitempty"`
	// Invalidation is an optional hint for when this segment's KV must be dropped
	// (e.g. "on_edit", "on_turn"); empty until a producer sets it (gated on #7).
	Invalidation string `json:"invalidation,omitempty"`
	ByteStart    int    `json:"byte_start"`
	ByteEnd      int    `json:"byte_end"`
	ByteHash     string `json:"byte_hash"`
	TokenStart   int    `json:"token_start,omitempty"`
	TokenEnd     int    `json:"token_end,omitempty"`
	TokenHash    string `json:"token_hash,omitempty"`
	// ToolCallsJSON is a raw JSON array of tool_calls for role=="assistant" turns
	// that triggered a tool invocation. Nil/empty means a plain text assistant turn.
	ToolCallsJSON string `json:"tool_calls_json,omitempty"`
	// ToolCallID is the tool call ID for role=="tool" result turns.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ContextManifest is the cache identity. A warm hit is valid only when the
// runtime identity is compatible and the token prefix still matches.
type ContextManifest struct {
	ProfileID            string            `json:"profile_id,omitempty"`
	Backend              string            `json:"backend"`
	BackendVersion       string            `json:"backend_version,omitempty"`
	ModelDigest          string            `json:"model_digest,omitempty"`
	PromptFormat         string            `json:"prompt_format"`
	PromptTemplateDigest string            `json:"prompt_template_digest"`
	RuntimeDigest        string            `json:"runtime_digest"`
	AddBOS               bool              `json:"add_bos"`
	StableBytes          int               `json:"stable_bytes"`
	TotalBytes           int               `json:"total_bytes"`
	StableByteHash       string            `json:"stable_byte_hash"`
	StableTokenHash      string            `json:"stable_token_hash,omitempty"`
	VolatileTokenHash    string            `json:"volatile_token_hash,omitempty"`
	Segments             []ManifestSegment `json:"segments,omitempty"`
}

// BuildSplitManifest constructs a manifest for text that has already been split
// into stable and volatile parts by a backend-specific prompt planner. It owns
// the shared manifest invariants only: byte ranges, byte hashes, cache classes,
// stable/total byte counts, and opaque runtime identity. It deliberately does
// not render prompts, tokenize text, or know anything about a backend.
func BuildSplitManifest(stableText, volatileText string, segments []ManifestSegment, id ManifestIdentity) (ContextManifest, error) {
	stableBytes := len(stableText)
	totalBytes := stableBytes + len(volatileText)
	outSegments := append([]ManifestSegment(nil), segments...)
	if len(outSegments) > 0 {
		if err := normalizeSplitSegments(stableText+volatileText, stableBytes, totalBytes, outSegments); err != nil {
			return ContextManifest{}, err
		}
	}
	return ContextManifest{
		ProfileID:            id.ProfileID,
		Backend:              id.Backend,
		BackendVersion:       id.BackendVersion,
		ModelDigest:          id.ModelDigest,
		PromptFormat:         id.PromptFormat,
		PromptTemplateDigest: id.PromptTemplateDigest,
		RuntimeDigest:        id.RuntimeDigest,
		AddBOS:               id.AddBOS,
		StableBytes:          stableBytes,
		TotalBytes:           totalBytes,
		StableByteHash:       HashString(stableText),
		Segments:             outSegments,
	}, nil
}

func normalizeSplitSegments(fullText string, stableBytes, totalBytes int, segments []ManifestSegment) error {
	stableCursor := 0
	volatileCursor := stableBytes
	seenVolatile := false
	for i := range segments {
		seg := &segments[i]
		if seg.ByteStart < 0 || seg.ByteEnd < seg.ByteStart || seg.ByteEnd > totalBytes {
			return NewManifestMismatchError(fmt.Sprintf("segment %q byte range is outside manifest text", seg.Kind))
		}
		if seg.Stable {
			if seenVolatile {
				return NewManifestMismatchError("stable segment appears after volatile segment")
			}
			if seg.ByteStart != stableCursor || seg.ByteEnd > stableBytes {
				return NewManifestMismatchError("stable segment byte ranges are not contiguous")
			}
			stableCursor = seg.ByteEnd
		} else {
			seenVolatile = true
			if seg.ByteStart != volatileCursor || seg.ByteEnd < stableBytes {
				return NewManifestMismatchError("volatile segment byte ranges are not contiguous")
			}
			volatileCursor = seg.ByteEnd
		}
		text := fullText[seg.ByteStart:seg.ByteEnd]
		wantHash := HashString(text)
		if seg.ByteHash != "" && seg.ByteHash != wantHash {
			return NewManifestMismatchError(fmt.Sprintf("segment %q byte hash does not match text", seg.Kind))
		}
		seg.ByteHash = wantHash
		if seg.CacheClass == "" {
			seg.CacheClass = defaultCacheClass(seg.Kind, seg.Stable)
		}
	}
	if stableCursor != stableBytes {
		return NewManifestMismatchError("stable segments do not cover stable text")
	}
	if volatileCursor != totalBytes {
		return NewManifestMismatchError("volatile segments do not cover volatile text")
	}
	return nil
}

func defaultCacheClass(kind string, stable bool) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "system", "tools", "repo_rules":
		return ClassTaskPinned.Tag()
	case "repo_map", "pinned":
		return ClassRepoMap.Tag()
	case "diff", "terminal", "user", "assistant", "tool", "assistant_prompt":
		return ClassVolatile.Tag()
	default:
		if stable {
			return ClassTaskPinned.Tag()
		}
		return ClassVolatile.Tag()
	}
}

// TokenizeFunc tokenizes text under the exact backend model/profile. addSpecial
// must match the backend call site, because BOS policy is part of cache identity.
type TokenizeFunc func(text string, addSpecial bool) ([]int, error)

func (m ContextManifest) isZero() bool {
	return m.Backend == "" &&
		m.ProfileID == "" &&
		m.ModelDigest == "" &&
		m.PromptFormat == "" &&
		m.PromptTemplateDigest == "" &&
		m.RuntimeDigest == "" &&
		m.StableByteHash == ""
}

// IsZero reports whether the caller supplied no manifest.
func (m ContextManifest) IsZero() bool {
	return m.isZero()
}

func (m ContextManifest) compatibleRuntime(next ContextManifest) (bool, string) {
	if m.isZero() || next.isZero() {
		return true, ""
	}
	checks := []struct {
		name string
		a    string
		b    string
	}{
		{"profile_id", m.ProfileID, next.ProfileID},
		{"backend", m.Backend, next.Backend},
		{"backend_version", m.BackendVersion, next.BackendVersion},
		{"model_digest", m.ModelDigest, next.ModelDigest},
		{"prompt_format", m.PromptFormat, next.PromptFormat},
		{"prompt_template_digest", m.PromptTemplateDigest, next.PromptTemplateDigest},
		{"runtime_digest", m.RuntimeDigest, next.RuntimeDigest},
	}
	for _, c := range checks {
		if c.a != c.b {
			return false, fmt.Sprintf("%s changed", c.name)
		}
	}
	if m.AddBOS != next.AddBOS {
		return false, "bos policy changed"
	}
	return true, ""
}

// CompatibleRuntime reports whether resident KV created for m may be considered
// for reuse by next. StableByteHash is deliberately excluded: if stable text
// changes under the same runtime identity, token LCP reuse is still correct.
func (m ContextManifest) CompatibleRuntime(next ContextManifest) (bool, string) {
	return m.compatibleRuntime(next)
}

func (m ContextManifest) withStableTokenHash(tokenHash string) ContextManifest {
	m.StableTokenHash = tokenHash
	return m
}

// WithStableTokenHash returns a copy of the manifest with the backend-resolved
// stable token hash attached.
func (m ContextManifest) WithStableTokenHash(tokenHash string) ContextManifest {
	return m.withStableTokenHash(tokenHash)
}

// WithStableTokenization fills token ranges/hashes for stable segments using the
// actual backend tokenizer.
func (m ContextManifest) WithStableTokenization(stableText string, tokens []int, tokenize TokenizeFunc, addBOS bool) (ContextManifest, error) {
	zero := m.isZero()
	out := m.withStableTokenHash(HashTokenIDs(tokens))
	if out.StableBytes == 0 {
		out.StableBytes = len(stableText)
	}
	if zero && len(out.Segments) == 0 {
		return out, nil
	}
	if out.StableBytes != len(stableText) {
		return ContextManifest{}, NewManifestMismatchError("stable byte length changed before tokenization")
	}
	if tokenize == nil {
		return ContextManifest{}, NewManifestMismatchError("stable token range population requires tokenizer")
	}
	prevByteEnd := 0
	prevTokenEnd := 0
	for i := range out.Segments {
		seg := &out.Segments[i]
		if !seg.Stable {
			continue
		}
		if seg.ByteStart != prevByteEnd || seg.ByteEnd < seg.ByteStart || seg.ByteEnd > len(stableText) {
			return ContextManifest{}, NewManifestMismatchError("stable segment byte ranges are not contiguous")
		}
		cumulative, err := tokenize(stableText[:seg.ByteEnd], addBOS)
		if err != nil {
			return ContextManifest{}, fmt.Errorf("contextasm manifest tokenize stable segment %q: %w", seg.Kind, err)
		}
		tokenEnd, ok := exactTokenPrefixLen(tokens, cumulative)
		if !ok {
			return ContextManifest{}, NewManifestMismatchError("stable segment boundary is not token-aligned")
		}
		if tokenEnd < prevTokenEnd {
			return ContextManifest{}, NewManifestMismatchError("stable segment token ranges are not monotonic")
		}
		seg.TokenStart = prevTokenEnd
		seg.TokenEnd = tokenEnd
		seg.TokenHash = HashTokenIDs(tokens[prevTokenEnd:tokenEnd])
		prevByteEnd = seg.ByteEnd
		prevTokenEnd = tokenEnd
	}
	if prevByteEnd != len(stableText) {
		return ContextManifest{}, NewManifestMismatchError("stable segments do not cover stable prefix")
	}
	if prevTokenEnd != len(tokens) {
		return ContextManifest{}, NewManifestMismatchError("stable segment token ranges do not cover stable prefix")
	}
	return out, nil
}

// WithVolatileTokenization merges stable token data from the resident manifest
// and fills token ranges/hashes for volatile suffix segments.
func (m ContextManifest) WithVolatileTokenization(stable ContextManifest, prefixTokens int, suffixText string, tokens []int, tokenize TokenizeFunc) (ContextManifest, error) {
	if m.isZero() && stable.isZero() && len(m.Segments) == 0 {
		out := m
		out.StableTokenHash = stable.StableTokenHash
		out.VolatileTokenHash = HashTokenIDs(tokens)
		return out, nil
	}
	if ok, reason := stable.compatibleRuntime(m); !ok {
		return ContextManifest{}, NewManifestMismatchError(reason)
	}
	if !stable.isZero() && !m.isZero() && stable.StableByteHash != m.StableByteHash {
		return ContextManifest{}, NewManifestMismatchError("stable prefix changed before suffix tokenization")
	}
	if m.StableBytes < 0 || m.TotalBytes < m.StableBytes || m.TotalBytes-m.StableBytes != len(suffixText) {
		return ContextManifest{}, NewManifestMismatchError("volatile byte length changed before tokenization")
	}
	if tokenize == nil {
		return ContextManifest{}, NewManifestMismatchError("volatile token range population requires tokenizer")
	}

	out := m
	out.StableTokenHash = stable.StableTokenHash
	out.VolatileTokenHash = HashTokenIDs(tokens)
	mergeStableSegmentTokens(&out, stable)
	for _, seg := range out.Segments {
		if seg.Stable && seg.TokenHash == "" {
			return ContextManifest{}, NewManifestMismatchError("stable segment token range missing from resident manifest")
		}
	}

	prevByteEnd := m.StableBytes
	prevTokenEnd := 0
	for i := range out.Segments {
		seg := &out.Segments[i]
		if seg.Stable {
			continue
		}
		if seg.ByteStart != prevByteEnd || seg.ByteEnd < seg.ByteStart || seg.ByteEnd > m.TotalBytes {
			return ContextManifest{}, NewManifestMismatchError("volatile segment byte ranges are not contiguous")
		}
		localEnd := seg.ByteEnd - m.StableBytes
		cumulative, err := tokenize(suffixText[:localEnd], false)
		if err != nil {
			return ContextManifest{}, fmt.Errorf("contextasm manifest tokenize volatile segment %q: %w", seg.Kind, err)
		}
		tokenEnd, ok := exactTokenPrefixLen(tokens, cumulative)
		if !ok {
			return ContextManifest{}, NewManifestMismatchError("volatile segment boundary is not token-aligned")
		}
		if tokenEnd < prevTokenEnd {
			return ContextManifest{}, NewManifestMismatchError("volatile segment token ranges are not monotonic")
		}
		seg.TokenStart = prefixTokens + prevTokenEnd
		seg.TokenEnd = prefixTokens + tokenEnd
		seg.TokenHash = HashTokenIDs(tokens[prevTokenEnd:tokenEnd])
		prevByteEnd = seg.ByteEnd
		prevTokenEnd = tokenEnd
	}
	if prevByteEnd != m.TotalBytes {
		return ContextManifest{}, NewManifestMismatchError("volatile segments do not cover suffix")
	}
	if prevTokenEnd != len(tokens) {
		return ContextManifest{}, NewManifestMismatchError("volatile segment token ranges do not cover suffix")
	}
	return out, nil
}

// ValidateSplitTokenization proves that tokenizing stable+suffix as a cold full
// prompt is exactly equivalent to the warm path's stable tokens plus suffix
// tokens.
func (m ContextManifest) ValidateSplitTokenization(stableText, suffixText string, stableTokens, suffixTokens []int, tokenize TokenizeFunc, addBOS bool) error {
	if tokenize == nil {
		return NewManifestMismatchError("split tokenization validation requires tokenizer")
	}
	full, err := tokenize(stableText+suffixText, addBOS)
	if err != nil {
		return fmt.Errorf("contextasm manifest tokenize full prompt: %w", err)
	}
	joined := make([]int, 0, len(stableTokens)+len(suffixTokens))
	joined = append(joined, stableTokens...)
	joined = append(joined, suffixTokens...)
	if !tokenIDsEqual(full, joined) {
		return NewManifestMismatchError("stable/suffix tokenization differs from cold full prompt")
	}
	return nil
}

func mergeStableSegmentTokens(out *ContextManifest, stable ContextManifest) {
	stableByIdentity := map[ManifestSegment]ManifestSegment{}
	for _, seg := range stable.Segments {
		if !seg.Stable {
			continue
		}
		key := seg
		key.TokenStart = 0
		key.TokenEnd = 0
		key.TokenHash = ""
		stableByIdentity[key] = seg
	}
	for i := range out.Segments {
		seg := &out.Segments[i]
		if !seg.Stable {
			continue
		}
		key := *seg
		key.TokenStart = 0
		key.TokenEnd = 0
		key.TokenHash = ""
		if filled, ok := stableByIdentity[key]; ok {
			seg.TokenStart = filled.TokenStart
			seg.TokenEnd = filled.TokenEnd
			seg.TokenHash = filled.TokenHash
		}
	}
}

func tokenIDsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func exactTokenPrefixLen(full, prefix []int) (int, bool) {
	if len(prefix) > len(full) {
		return 0, false
	}
	for i := range prefix {
		if full[i] != prefix[i] {
			return 0, false
		}
	}
	return len(prefix), true
}

func (m ContextManifest) digest() string {
	if m.isZero() {
		return ""
	}
	b, _ := json.Marshal(m)
	return HashBytes(b)
}

// Digest returns a stable hash of the full manifest.
func (m ContextManifest) Digest() string {
	return m.digest()
}

func HashString(s string) string {
	return HashBytes([]byte(s))
}

func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// HashTokenIDs hashes token IDs under an explicit little-endian encoding so the
// value is stable across 32-bit/64-bit Go platforms.
func HashTokenIDs(tokens []int) string {
	var tmp [8]byte
	h := sha256.New()
	for _, tok := range tokens {
		binary.LittleEndian.PutUint64(tmp[:], uint64(tok))
		_, _ = h.Write(tmp[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}
