package picker

import (
	"strings"
	"testing"
)

// repoCorpus is the realistic path fixture the ranking table runs against: a
// slice of this repository, because ranking is only ever wrong in a way you
// can see on real paths. Made-up labels ("aaa/bbb.go") rank perfectly under
// any scorer and prove nothing.
func repoCorpus() []Item {
	paths := []string{
		"internal/surfaces/beamtui/app/keybindings.go",
		"internal/surfaces/beamtui/app/workbench_dashboard.go",
		"internal/surfaces/beamtui/comp/composer/composer.go",
		"internal/surfaces/beamtui/comp/composer/composer_test.go",
		"internal/surfaces/beamtui/comp/picker/picker.go",
		"internal/surfaces/beamtui/comp/fileaddr/fileaddr.go",
		"internal/surfaces/beamtui/textwidth/textwidth.go",
		"internal/surfaces/beamtui/textwidth/textwidth_test.go",
		"internal/services/vfs/vfs.go",
		"docs/development/blueprints/beam-tui.md",
		"README.md",
	}
	items := make([]Item, 0, len(paths))
	for _, p := range paths {
		items = append(items, Item{ID: p, Label: p})
	}
	return items
}

// TestUnit_PickerFuzzyScore_AgreesWithTheSubsequenceTier: FuzzyScore and Rank
// must never disagree about WHETHER something matched. Filter uses the first
// to order what the second admitted, so a scorer that refused a label the
// tier accepted would silently sort a matched item to an arbitrary place.
func TestUnit_PickerFuzzyScore_AgreesWithTheSubsequenceTier(t *testing.T) {
	queries := []string{"", "p", "pick", "comp", "icp", "kbd", "tw", "zzz", "gp", "picker.go"}
	labels := []string{
		"picker.go", "internal/comp/picker.go", "internal/comp/comp.go",
		"session-alpha", "c/o/m/p.go", "README.md", "",
	}
	for _, q := range queries {
		for _, l := range labels {
			_, tierOK := Rank(q, l)
			_, fuzzyOK := FuzzyScore(q, l)
			if tierOK != fuzzyOK {
				t.Errorf("Rank(%q, %q) matched=%v but FuzzyScore matched=%v", q, l, tierOK, fuzzyOK)
			}
		}
	}
}

// TestUnit_PickerFuzzyScore_Preferences pins the scorer's judgement as
// ORDERED PAIRS rather than absolute numbers: the constants are free to be
// retuned, the preferences are not.
func TestUnit_PickerFuzzyScore_Preferences(t *testing.T) {
	cases := []struct{ name, query, better, worse string }{
		{
			"a segment start beats the same runes mid-word",
			"pick", "internal/comp/picker.go", "internal/comp/unpicked.go",
		},
		{
			"the basename beats a parent directory",
			"pick", "app/picker.go", "picker/app.go",
		},
		{
			"contiguous beats gapped when neither gap lands on a boundary",
			"pick", "picker.go", "pixcxkxexr.go",
		},
		{
			"a word boundary beats a bare gap",
			"tw", "text_width.go", "textawidth.go",
		},
		{
			"a case boundary is a boundary",
			"tw", "TextWidth.go", "textawidth.go",
		},
		{
			"matching the query's case is worth something",
			"tw", "textwidth.go", "TEXTWIDTH.go",
		},
		{
			// The blueprint's own example: the first rune landing at the start
			// of the basename outweighs the consecutive \"kb\" run and the
			// underscore boundary the other one collects.
			"kbd finds keybindings, not workbench_dashboard",
			"kbd",
			"internal/surfaces/beamtui/app/keybindings.go",
			"internal/surfaces/beamtui/app/workbench_dashboard.go",
		},
		{
			"a shorter gap beats a longer one",
			"ab", "axb.go", "axxxxxxxxb.go",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hi, okHi := FuzzyScore(tc.query, tc.better)
			lo, okLo := FuzzyScore(tc.query, tc.worse)
			if !okHi || !okLo {
				t.Fatalf("both candidates must match: %q=%v %q=%v", tc.better, okHi, tc.worse, okLo)
			}
			if hi <= lo {
				t.Fatalf("FuzzyScore(%q):\n  %q = %d\n  %q = %d\nwant the first strictly higher",
					tc.query, tc.better, hi, tc.worse, lo)
			}
		})
	}
}

// TestUnit_PickerFuzzyScore_ScoresTheBestAlignment: a greedy left-to-right
// matcher takes the first occurrence of every rune, which for a path is
// usually the WRONG one — the query's runes almost always appear somewhere in
// the leading directories. The scorer must find the placement a human meant.
func TestUnit_PickerFuzzyScore_ScoresTheBestAlignment(t *testing.T) {
	// Leftmost matching would spend "p" on "comp" and "i" on "beamtui",
	// scattering the match across the directory prefix; the best alignment
	// puts all six runes contiguously in the basename.
	const label = "internal/surfaces/beamtui/comp/picker/picker.go"
	got, ok := FuzzyScore("picker", label)
	if !ok {
		t.Fatal("picker did not match its own path")
	}
	// The same six contiguous runes at the same kind of boundary, with no
	// directory prefix at all, must score identically: a leading gap is free.
	bare, ok := FuzzyScore("picker", "picker.go")
	if !ok {
		t.Fatal("picker did not match picker.go")
	}
	if got != bare {
		t.Fatalf("deep path scored %d, bare name scored %d — the leading gap is not free", got, bare)
	}
}

func TestUnit_PickerFuzzyScore_Edges(t *testing.T) {
	if s, ok := FuzzyScore("", "anything"); !ok || s != 0 {
		t.Fatalf("FuzzyScore(\"\", ...) = (%d, %v), want (0, true)", s, ok)
	}
	if _, ok := FuzzyScore("toolong", "abc"); ok {
		t.Fatal("a query longer than the candidate matched")
	}
	if _, ok := FuzzyScore("a", ""); ok {
		t.Fatal("a non-empty query matched the empty candidate")
	}
	if _, ok := FuzzyScore("PICK", "internal/picker.go"); !ok {
		t.Fatal("matching is not case-insensitive")
	}
	if _, ok := FuzzyScore("kp", "picker.go"); ok {
		t.Fatal("out-of-order runes matched")
	}
	// Multi-byte runes are runes, not bytes: a byte-wise scorer would find
	// the query inside the UTF-8 encoding of an unrelated character.
	if s, ok := FuzzyScore("日本", "docs/日本語/readme.md"); !ok || s <= 0 {
		t.Fatalf("FuzzyScore over multi-byte runes = (%d, %v), want a match", s, ok)
	}
	if _, ok := FuzzyScore("本日", "docs/日本語/readme.md"); ok {
		t.Fatal("reversed multi-byte runes matched")
	}
}

// TestUnit_PickerFuzzyScore_BoundedOnHugeCandidates: past the DP's cell
// budget the scorer falls back to a single greedy pass, which must still
// agree about whether the query matched — the half Filter's correctness
// depends on — and must return promptly.
func TestUnit_PickerFuzzyScore_BoundedOnHugeCandidates(t *testing.T) {
	huge := strings.Repeat("a/b/", 20000) + "picker.go"
	if len(huge) <= maxDPCells {
		t.Fatalf("fixture is %d runes, not past the %d-cell budget", len(huge), maxDPCells)
	}
	s, ok := FuzzyScore("pick", huge)
	if !ok || s <= 0 {
		t.Fatalf("FuzzyScore over a huge candidate = (%d, %v), want a match", s, ok)
	}
	if _, ok := FuzzyScore("zzq", huge); ok {
		t.Fatal("a query absent from a huge candidate matched")
	}
}

// TestUnit_PickerFilter_FuzzyTierRanking is the ranking property the rebuild
// exists for, run end-to-end through Filter over real repository paths.
func TestUnit_PickerFilter_FuzzyTierRanking(t *testing.T) {
	cases := []struct {
		name  string
		query string
		// want is the expected HEAD of the ranked list; the tail is left
		// unpinned so adding a file to the corpus does not rewrite the test.
		want []string
	}{
		{
			// Both candidates are subsequence matches — the tiers cannot tell
			// them apart, so this is the fuzzy scorer's decision alone.
			name:  "kbd",
			query: "kbd",
			want: []string{
				"internal/surfaces/beamtui/app/keybindings.go",
				"internal/surfaces/beamtui/app/workbench_dashboard.go",
			},
		},
		{
			name:  "compo",
			query: "compo",
			want: []string{
				"internal/surfaces/beamtui/comp/composer/composer.go",
				"internal/surfaces/beamtui/comp/composer/composer_test.go",
			},
		},
		{
			name:  "tw",
			query: "tw",
			want: []string{
				"internal/surfaces/beamtui/textwidth/textwidth.go",
				"internal/surfaces/beamtui/textwidth/textwidth_test.go",
			},
		},
		{
			// Nothing here is a prefix or a substring, so every survivor is in
			// the fuzzy tier and the scorer alone produces this order:
			// composer's runes sit at word boundaries inside the basename,
			// workbench_dashboard's are scattered across the whole path.
			name:  "an acronym only the fuzzy tier can resolve",
			query: "cmpsr",
			want: []string{
				"internal/surfaces/beamtui/comp/composer/composer.go",
				"internal/surfaces/beamtui/comp/composer/composer_test.go",
				"internal/surfaces/beamtui/app/workbench_dashboard.go",
			},
		},
		{
			name:  "txwd",
			query: "txwd",
			want: []string{
				"internal/surfaces/beamtui/textwidth/textwidth.go",
				"internal/surfaces/beamtui/textwidth/textwidth_test.go",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Filter(repoCorpus(), tc.query)
			var labels []string
			for _, it := range got {
				labels = append(labels, it.Label)
			}
			if len(labels) < len(tc.want) {
				t.Fatalf("Filter(%q) returned %v, want at least %v", tc.query, labels, tc.want)
			}
			for i, w := range tc.want {
				if labels[i] != w {
					t.Fatalf("Filter(%q) position %d = %q, want %q\nfull order: %s",
						tc.query, i, labels[i], w, strings.Join(labels, "\n            "))
				}
			}
		})
	}
}

// TestUnit_PickerFilter_FuzzyTierIsStableAndDeterministic: two items that
// score identically keep their input order, and the same input always
// produces the same output — a picker whose rows shuffle between identical
// keystrokes is unusable however good its ranking is.
func TestUnit_PickerFilter_FuzzyTierIsStableAndDeterministic(t *testing.T) {
	items := []Item{
		{ID: "first", Label: "a/x/y/z.go"},
		{ID: "second", Label: "a/x/y/z.go"},
		{ID: "third", Label: "a/x/y/z.go"},
	}
	first := Filter(items, "axz")
	if len(first) != 3 {
		t.Fatalf("got %d items, want 3", len(first))
	}
	if first[0].Rank != RankSubsequence {
		t.Fatalf("fixture is tier %d, want the fuzzy tier %d", first[0].Rank, RankSubsequence)
	}
	for i, want := range []string{"first", "second", "third"} {
		if first[i].ID != want {
			t.Fatalf("position %d = %q, want %q (equal scores must keep input order)", i, first[i].ID, want)
		}
	}
	for run := 0; run < 5; run++ {
		again := Filter(repoCorpus(), "kbd")
		for i := range again {
			if again[i].Label != Filter(repoCorpus(), "kbd")[i].Label {
				t.Fatal("two identical Filter calls returned different orders")
			}
		}
	}
}
