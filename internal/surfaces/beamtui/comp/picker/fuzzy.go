package picker

import "unicode"

// Fuzzy scoring constants. The scale is arbitrary but the RATIOS are the
// design: a match is worth much more than a gap costs, so a candidate that
// contains the query's runes at all beats one that nearly does; a boundary
// bonus is worth half a match, so structure moves a candidate up without ever
// outweighing an extra matched rune.
//
// The shape is the one fzf and fzy converged on independently, with two
// additions the blueprint's file list needs: a per-rune bonus for matching
// inside the BASENAME (an @-mention is nearly always aimed at a filename, not
// at the directory that holds it) and a small bonus for matching the query's
// case exactly (so a typed "TW" prefers TextWidth over textwidth without ever
// refusing the lowercase one).
const (
	// scoreMatch is what every matched rune is worth before bonuses.
	scoreMatch = 16

	// scoreGapStart / scoreGapExtend price a run of skipped runes: opening a
	// gap costs more than widening one, which is what makes "picker.go" beat
	// "p...i...c...k...e...r.go" for the query "picker" instead of the two
	// tying on match count.
	scoreGapStart  = -3
	scoreGapExtend = -1

	// bonusSlash is the strongest structural signal: the rune starts a path
	// segment (it follows '/', or it is the first rune of the candidate).
	// Segment starts are what a human means when they type the first letters
	// of a directory or a filename.
	bonusSlash = 10

	// bonusBoundary is a word start inside a segment: the rune follows a
	// non-alphanumeric — '_', '-', '.', ' ' and anything else that is not a
	// letter or digit.
	bonusBoundary = 8

	// bonusCamel is a case boundary: a lower-or-digit followed by an upper.
	bonusCamel = 7

	// bonusConsecutive is what an immediately-adjacent match earns. It is
	// exactly -(scoreGapStart + scoreGapExtend), so a contiguous run is worth
	// precisely what the gap it avoided would have cost — contiguity is
	// rewarded without being able to beat a genuine word boundary.
	bonusConsecutive = 4

	// bonusBasename is added per rune matched at or after the last '/'. It is
	// deliberately small: it tips ties toward the filename without letting a
	// deep path's basename outrank a shallow exact structural match.
	bonusBasename = 4

	// bonusCaseMatch rewards matching the query's case exactly.
	bonusCaseMatch = 1

	// bonusFirstCharMultiplier weights the FIRST query rune's structural
	// bonus. Where the query starts is the strongest evidence of intent
	// available: "kbd" aimed at keybindings.go starts at a segment start,
	// while the same runes land mid-word in workbench_dashboard.go, and
	// without this weighting the second one's consecutive "kb" wins.
	bonusFirstCharMultiplier = 2

	// maxDPCells bounds one FuzzyScore call. Beyond it the optimal alignment
	// is approximated by a single greedy pass, which agrees with the DP on
	// WHETHER the query matches and only on how well. Filter runs this over
	// every fuzzy-tier candidate on a debounced keystroke, so the worst case
	// has to be bounded by something other than optimism about path lengths.
	maxDPCells = 1 << 16
)

// negInf marks an unreachable DP cell. It is far below any reachable score
// yet nowhere near overflowing when a gap penalty is added to it.
const negInf = -(1 << 30)

// FuzzyScore scores query against candidate and reports whether it matched at
// all. Higher is better; scores are comparable only between candidates
// scored against the SAME query, and carry no meaning on their own.
//
// A match means the query's runes appear in candidate in order, with gaps
// allowed — the same relation [Rank]'s RankSubsequence tier tests, so the two
// never disagree about whether something matched, only about how to order the
// things that did. Matching is case-insensitive.
//
// The score is the best alignment's, not the first one found: the runes are
// aligned by dynamic programming over (query x candidate), so a query whose
// runes appear several times in the candidate is scored by the placement a
// human would have picked. What the scoring rewards, in order of weight:
//
//   - a rune starting a path segment (after '/', or at the start)
//   - a rune starting a word (after '_', '-', '.', ' ', or any non-alphanumeric)
//   - a rune at a case boundary ("textWidth" -> the 'W')
//   - a rune immediately after the previous match (a contiguous run)
//   - a rune in the basename rather than in a parent directory
//   - matching the query's case exactly
//
// and what it penalises is gaps, more for opening one than for widening it.
// The first query rune's structural bonus counts double, because where a
// match STARTS is the clearest evidence of what the human meant.
//
// A gap BEFORE the first match is free, deliberately: a candidate deep in the
// tree must not lose to a shallow one for being deep, or "kbd" would rank a
// stray root-level file over internal/…/keybindings.go.
//
// An empty query matches everything with score 0, mirroring [Rank]'s
// empty-query rule.
func FuzzyScore(query, candidate string) (int, bool) {
	if query == "" {
		return 0, true
	}
	q := []rune(query)
	c := []rune(candidate)
	if len(q) > len(c) {
		return 0, false
	}

	ql := make([]rune, len(q))
	for i, r := range q {
		ql[i] = unicode.ToLower(r)
	}
	cl := make([]rune, len(c))
	bonus := make([]int, len(c))
	basename := 0
	var prev rune
	for j, r := range c {
		cl[j] = unicode.ToLower(r)
		bonus[j] = boundaryBonus(prev, r, j == 0)
		if r == '/' {
			basename = j + 1
		}
		prev = r
	}

	if len(q)*len(c) > maxDPCells {
		return fuzzyGreedy(q, ql, c, cl, bonus, basename)
	}

	// scoreAt is one cell of the match matrix: what q[i] matched at c[j] is
	// worth, given whether it continues a contiguous run.
	scoreAt := func(i, j int, consecutive bool) int {
		b := bonus[j]
		if consecutive && bonusConsecutive > b {
			b = bonusConsecutive
		}
		if i == 0 {
			b *= bonusFirstCharMultiplier
		}
		s := scoreMatch + b
		if j >= basename {
			s += bonusBasename
		}
		if c[j] == q[i] {
			s += bonusCaseMatch
		}
		return s
	}

	// d[j] is the best total score for q[0..i] with q[i] matched at c[j].
	// gap[t] is the best total for q[0..i] whose last match sits at some
	// k <= t-1, already carrying the penalty for the gap spanning k+1..t —
	// the standard affine-gap running maximum, which is what keeps this
	// linear in the candidate rather than quadratic.
	d := make([]int, len(c))
	next := make([]int, len(c))
	gap := make([]int, len(c))

	for j := range d {
		d[j] = negInf
		if cl[j] == ql[0] {
			d[j] = scoreAt(0, j, false)
		}
	}

	for i := 1; i < len(q); i++ {
		best := negInf
		for t := 0; t < len(c); t++ {
			// gap[t]: extend the run that ended at t-1, or open a new gap
			// after a match at t-1.
			g := negInf
			if t > 0 {
				if best != negInf {
					g = best + scoreGapExtend
				}
				if d[t-1] != negInf && d[t-1]+scoreGapStart > g {
					g = d[t-1] + scoreGapStart
				}
			}
			gap[t] = g
			best = g
		}
		for j := range next {
			next[j] = negInf
			if cl[j] != ql[i] || j == 0 {
				continue
			}
			if v := d[j-1]; v != negInf {
				next[j] = v + scoreAt(i, j, true)
			}
			if v := gap[j-1]; v != negInf {
				if s := v + scoreAt(i, j, false); s > next[j] {
					next[j] = s
				}
			}
		}
		d, next = next, d
	}

	out := negInf
	for _, v := range d {
		if v > out {
			out = v
		}
	}
	if out == negInf {
		return 0, false
	}
	return out, true
}

// fuzzyGreedy is the bounded fallback for a pathologically large pair: it
// takes the leftmost occurrence of every query rune in one pass. Leftmost
// matching is COMPLETE for subsequence detection, so the ok it returns is the
// same one the DP would; only the score is an approximation, and only for
// candidates far longer than any path a picker was ever handed.
func fuzzyGreedy(q, ql, c, cl []rune, bonus []int, basename int) (int, bool) {
	total := 0
	j := 0
	prevMatch := -2
	for i := range ql {
		for j < len(cl) && cl[j] != ql[i] {
			j++
		}
		if j == len(cl) {
			return 0, false
		}
		b := bonus[j]
		if j == prevMatch+1 && bonusConsecutive > b {
			b = bonusConsecutive
		}
		if i == 0 {
			b *= bonusFirstCharMultiplier
		} else if g := j - prevMatch - 1; g > 0 {
			total += scoreGapStart + (g-1)*scoreGapExtend
		}
		total += scoreMatch + b
		if j >= basename {
			total += bonusBasename
		}
		if c[j] == q[i] {
			total += bonusCaseMatch
		}
		prevMatch = j
		j++
	}
	return total, true
}

// boundaryBonus classifies position j from the rune before it: a segment
// start, a word start, a case boundary, or nothing. first marks j == 0, which
// counts as a segment start — the beginning of a bare name is as strong a
// signal as the beginning of one inside a path.
func boundaryBonus(prev, cur rune, first bool) int {
	if first || prev == '/' {
		return bonusSlash
	}
	if !isWordRune(prev) {
		return bonusBoundary
	}
	if !unicode.IsUpper(prev) && unicode.IsUpper(cur) {
		return bonusCamel
	}
	return 0
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}
