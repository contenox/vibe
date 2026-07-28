package picker

import "unicode"

// Fuzzy scoring constants. Ratios matter more than the absolute scale: a
// match beats a gap, and a boundary bonus is worth half a match. Adds a
// basename bonus and a small exact-case bonus to the usual fzf/fzy shape.
const (
	scoreMatch = 16 // what every matched rune is worth before bonuses

	// Opening a gap costs more than widening one.
	scoreGapStart  = -3
	scoreGapExtend = -1

	bonusSlash    = 10 // rune starts a path segment (follows '/', or is first)
	bonusBoundary = 8  // rune follows a non-alphanumeric word separator
	bonusCamel    = 7  // case boundary (lower-or-digit followed by upper)

	// bonusConsecutive is exactly -(scoreGapStart + scoreGapExtend): worth
	// precisely what the avoided gap would cost, never enough to beat a
	// real word boundary.
	bonusConsecutive = 4

	bonusBasename  = 4 // per rune matched at or after the last '/'
	bonusCaseMatch = 1 // matching the query's case exactly

	// bonusFirstCharMultiplier weights the first query rune's bonus: where
	// a match starts is the strongest signal of intent.
	bonusFirstCharMultiplier = 2

	// maxDPCells bounds one FuzzyScore call; beyond it a greedy pass
	// approximates the DP.
	maxDPCells = 1 << 16
)

// negInf marks an unreachable DP cell.
const negInf = -(1 << 30)

// FuzzyScore scores query against candidate and reports whether it matched
// (the same subsequence relation as [Rank]'s RankSubsequence tier, case
// insensitive). Higher is better; scores are comparable only against the
// same query. The score is the best alignment found by dynamic programming,
// rewarding segment/word/case boundaries, contiguous runs, and basename
// matches, while penalizing gaps. A gap before the first match is free. An
// empty query matches everything with score 0.
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

	// scoreAt: what q[i] matched at c[j] is worth.
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

	// d[j]: best score for q[0..i] with q[i] matched at c[j]. gap[t]: best
	// score for q[0..i] with a gap through t (the affine-gap running max,
	// keeping this linear rather than quadratic).
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
			// Extend the run ending at t-1, or open a gap after it.
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

// fuzzyGreedy is the bounded fallback for a pathologically large pair: the
// leftmost occurrence of every query rune. ok agrees with the DP; the score
// is an approximation.
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

// boundaryBonus classifies position j from the rune before it. first (j==0)
// also counts as a segment start.
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
