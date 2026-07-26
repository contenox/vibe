package contextasm

import (
	"sort"
	"strings"
)

// SegmentKind classifies a context segment by how stable it is across turns.
// Lower kinds are more stable and render first so backend prefix caches can
// reuse their KV; kinds at or after KindDiff are volatile and render last.
type SegmentKind int

const (
	KindSystem    SegmentKind = iota // system / developer instructions
	KindTools                        // tool schemas
	KindRepoRules                    // AGENTS.md / project conventions
	KindRepoMap                      // repo / symbol map
	KindPinned                       // pinned files
	KindDiff                         // current diff / working changes (volatile)
	KindTerminal                     // terminal / test output (volatile)
	KindUserTurn                     // the user's message (volatile)
)

const volatileFrom = KindDiff

// CacheClass is the coding-aware retention priority of a segment, coarser than
// SegmentKind. It is the seam a budget-aware admission/eviction policy uses to
// decide what to keep warm when context exceeds the window: drop higher
// (more-evictable) classes first, pin lower ones. Wiring a producer of rich
// segments + the drop policy is gated on the T3 context planner (#7); this type
// is the foundation it builds on.
type CacheClass int

const (
	// ClassTaskPinned is the task-defining core (system/developer instructions,
	// tool schemas, repo conventions): pinned hardest, evicted last.
	ClassTaskPinned CacheClass = iota
	// ClassRepoMap is workspace structure (repo/symbol map, pinned files).
	ClassRepoMap
	// ClassVolatile is churning material (diff, terminal output, the user turn):
	// admitted only when likely reused, evicted first.
	ClassVolatile
)

// CacheClass maps a segment kind to its retention class.
func (k SegmentKind) CacheClass() CacheClass {
	switch k {
	case KindSystem, KindTools, KindRepoRules:
		return ClassTaskPinned
	case KindRepoMap, KindPinned:
		return ClassRepoMap
	default: // KindDiff, KindTerminal, KindUserTurn
		return ClassVolatile
	}
}

// Tag returns the stable string form used in manifests.
func (c CacheClass) Tag() string {
	switch c {
	case ClassTaskPinned:
		return "task_pinned"
	case ClassRepoMap:
		return "repo_map"
	case ClassVolatile:
		return "volatile"
	default:
		return "unknown"
	}
}

// MoreEvictableThan reports whether c should be dropped before other when trimming
// context to fit a budget (higher class = lower priority = evicted first).
func (c CacheClass) MoreEvictableThan(other CacheClass) bool { return c > other }

// Segment is one deterministic, hashable piece of workspace context.
type Segment struct {
	Kind    SegmentKind
	Content string
}

// ManifestIdentity is the backend/profile/runtime identity attached to a
// manifest built from assembled context segments.
type ManifestIdentity struct {
	ProfileID            string
	Backend              string
	BackendVersion       string
	ModelDigest          string
	PromptFormat         string
	PromptTemplateDigest string
	RuntimeDigest        string
	AddBOS               bool
}

// AssembleContext renders segments into a single prompt whose stable prefix is
// byte-identical across turns when the stable segments are unchanged.
func AssembleContext(segs []Segment) (prompt string, stablePrefixHash string) {
	prompt, manifest := AssembleManifest(segs, ManifestIdentity{})
	return prompt, manifest.StableByteHash
}

// AssembleManifest renders segments canonically and returns a manifest over the
// rendered logical context. Backends that render through an opaque model-native
// chat template may still use this as cache identity, but must not claim exact
// template-token segment ranges until they can prove boundaries under that
// template.
func AssembleManifest(segs []Segment, id ManifestIdentity) (prompt string, manifest ContextManifest) {
	sorted := make([]Segment, len(segs))
	copy(sorted, segs)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Kind < sorted[j].Kind })

	var b strings.Builder
	stableLen := 0
	manifestSegments := make([]ManifestSegment, 0, len(sorted))
	for _, s := range sorted {
		text := renderSegment(s)
		start := b.Len()
		b.WriteString(text)
		end := b.Len()
		stable := s.Kind < volatileFrom
		if stable {
			stableLen = end
		}
		manifestSegments = append(manifestSegments, ManifestSegment{
			Kind:      s.Kind.Tag(),
			Stable:    stable,
			ByteStart: start,
			ByteEnd:   end,
		})
	}

	full := b.String()
	manifest, err := BuildSplitManifest(full[:stableLen], full[stableLen:], manifestSegments, id)
	if err != nil {
		return full, ContextManifest{}
	}
	return full, manifest
}

func renderSegment(s Segment) string {
	return "<|seg:" + s.Kind.Tag() + "|>\n" + s.Content + "\n"
}

// Tag returns the stable string form used in manifests.
func (k SegmentKind) Tag() string {
	switch k {
	case KindSystem:
		return "system"
	case KindTools:
		return "tools"
	case KindRepoRules:
		return "repo_rules"
	case KindRepoMap:
		return "repo_map"
	case KindPinned:
		return "pinned"
	case KindDiff:
		return "diff"
	case KindTerminal:
		return "terminal"
	case KindUserTurn:
		return "user"
	default:
		return "unknown"
	}
}
