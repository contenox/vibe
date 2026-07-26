package localtools

// hardening.go implements the model-diversity hardening the local tools were
// missing, per docs/development/blueprints/tool-hardening.md. Three recs live
// here:
//
//   - Rec 5 (fatal-vs-recoverable severity): a terse, greppable severity marker
//     appended to error text so a model can decide whether a retry-with-correction
//     is worth attempting. Not a type system — error-string craft, as the rec
//     directs — kept deliberately distinct from toolguidance's "[harness] " prefix
//     so the two never collide.
//   - Rec 7 (did-you-mean on match failures): sibling-filename fuzzy suggestions on
//     a missing path, and nearest-line suggestions on a sed no-match. Both obey the
//     fuzzy law (tool-hardening.md): tools may search fuzzily and TELL, they must
//     never fuzzily ACT. Everything here only SUGGESTS.
//   - Rec 4 support (never truncate silently): streamRange is the bounded,
//     size-cap-agnostic line reader that lets read_file page a file larger than the
//     read cap and name the exact next line to resume from.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"syscall"
)

// --- Rec 5: fatal-vs-recoverable severity convention ---
//
// The convention is a suffix on the error text, consistent across every local
// tool so a model (or a human grepping a transcript) can key on it:
//
//   - severityRecoverable — the default for everything a correction can fix:
//     not-found, boundary/escape refusals, denied paths, size/output caps, binary
//     refusals, missing/unknown args, read-before-write denials, no-match.
//   - "(fatal: <reason>)" — the environment is broken and no parameter change
//     helps: disk full, spool file unwritable. Only these are marked fatal
//     (gemini-cli's own bar), applied at the source via fatalf.
const (
	severityRecoverable = "(recoverable: adjust parameters and retry)"
	severityFatalToken  = "(fatal:"
)

// hasSeverityMarker reports whether s already carries a severity marker, so
// markSeverity never double-tags and never downgrades a fatal to recoverable.
func hasSeverityMarker(s string) bool {
	return strings.Contains(s, severityRecoverable) || strings.Contains(s, severityFatalToken)
}

// markSeverity tags err recoverable-by-correction unless it is already tagged.
// The wrap preserves the error chain (errors.Is still works) while appending the
// marker to the rendered text.
func markSeverity(err error) error {
	if err == nil {
		return nil
	}
	if hasSeverityMarker(err.Error()) {
		return err
	}
	return fmt.Errorf("%w %s", err, severityRecoverable)
}

// recoverablef builds a recoverable-by-correction error with the marker suffix.
func recoverablef(format string, a ...any) error {
	return errors.New(fmt.Sprintf(format, a...) + " " + severityRecoverable)
}

// fatalf builds a fatal error: "<msg> (fatal: <reason>)". Reserved for genuinely
// unrecoverable conditions (disk full, spool unwritable).
func fatalf(reason, format string, a ...any) error {
	return fmt.Errorf("%s (fatal: %s)", fmt.Sprintf(format, a...), reason)
}

// isDiskFull reports whether err is (or wraps) a no-space-left-on-device
// condition — the canonical fatal I/O failure.
func isDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}

// --- Rec 7: did-you-mean over sibling filenames ---

// maxSuggestions caps how many "did you mean" candidates are surfaced, per the
// blueprint (cap 5). Enough to be useful, few enough to stay terse.
const maxSuggestions = 5

// didYouMean renders a " Did you mean: a, b, c?" clause for a missing entry named
// `missing` inside `dir`, or "" when nothing in dir resembles it. Pure suggestion
// (the fuzzy law): the caller never acts on the result.
func didYouMean(dir, missing string) string {
	s := suggestSiblings(dir, missing, maxSuggestions)
	if len(s) == 0 {
		return ""
	}
	return " Did you mean: " + strings.Join(s, ", ") + "?"
}

// suggestSiblings returns up to `limit` entries in dir whose names resemble
// `missing`: case-insensitive substring matches first (either direction), then
// small edit-distance matches (Levenshtein within editThreshold). Returns nil
// when dir is unreadable or nothing resembles `missing`.
func suggestSiblings(dir, missing string, limit int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	lowMiss := strings.ToLower(missing)
	type cand struct {
		name string
		rank int // lower is better
	}
	var subs, dists []cand
	maxDist := editThreshold(missing)
	for _, e := range entries {
		name := e.Name()
		if name == missing {
			continue
		}
		low := strings.ToLower(name)
		switch {
		case lowMiss != "" && (strings.Contains(low, lowMiss) || strings.Contains(lowMiss, low)):
			subs = append(subs, cand{name, len(name)})
		default:
			if d := levenshtein(low, lowMiss); d <= maxDist {
				dists = append(dists, cand{name, d})
			}
		}
	}
	sort.SliceStable(subs, func(i, j int) bool { return subs[i].rank < subs[j].rank })
	sort.SliceStable(dists, func(i, j int) bool { return dists[i].rank < dists[j].rank })
	out := make([]string, 0, limit)
	for _, c := range append(subs, dists...) {
		if len(out) >= limit {
			break
		}
		out = append(out, c.name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// editThreshold scales the allowed edit distance with name length: tight for
// short names (a 1-char typo), a little looser for long ones, never sloppy.
func editThreshold(s string) int {
	switch n := len([]rune(s)); {
	case n <= 4:
		return 1
	case n <= 8:
		return 2
	default:
		return 3
	}
}

// levenshtein is the standard edit distance between two strings (rune-based, so
// multibyte names compare correctly). Bounded work: O(len(a)*len(b)).
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// --- Rec 7: nearest-line suggestion for sed no-match ---

// suggestNearestLinesMaxScan bounds how many lines suggestNearestLines will
// scan, keeping the best-window search cheap on a large file.
const suggestNearestLinesMaxScan = 5000

// suggestLineCompareLen caps the number of characters compared per line so the
// similarity scan stays cheap on files with very long lines.
const suggestLineCompareLen = 256

// suggestNearestLines returns the window of `content` most similar to `pattern`,
// with ±contextLines of surrounding lines, rendered as "N: text" lines (1-based).
// aider's best-window pattern: slide a window the size of the pattern's line
// count across the file, score each by similarity, keep the best. SUGGEST-only —
// the caller never applies it (the fuzzy law). Returns "" when content is empty.
func suggestNearestLines(content, pattern string, contextLines int) string {
	if strings.TrimSpace(content) == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	scan := len(lines)
	if scan > suggestNearestLinesMaxScan {
		scan = suggestNearestLinesMaxScan
	}
	plines := strings.Split(pattern, "\n")
	win := len(plines)
	if win < 1 {
		win = 1
	}
	if win > scan {
		win = scan
	}
	target := clampCompare(strings.Join(plines, "\n"))

	bestStart, bestScore := 0, -1.0
	for i := 0; i+win <= scan; i++ {
		cand := clampCompare(strings.Join(lines[i:i+win], "\n"))
		score := similarity(cand, target)
		if score > bestScore {
			bestScore, bestStart = score, i
		}
	}

	lo := bestStart - contextLines
	if lo < 0 {
		lo = 0
	}
	hi := bestStart + win - 1 + contextLines
	if hi >= len(lines) {
		hi = len(lines) - 1
	}
	var b strings.Builder
	for i := lo; i <= hi; i++ {
		if i > lo {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d: %s", i+1, lines[i])
	}
	return b.String()
}

func clampCompare(s string) string {
	if len(s) > suggestLineCompareLen {
		return s[:suggestLineCompareLen]
	}
	return s
}

// similarity is a normalized edit-distance ratio in [0,1]: 1.0 is identical.
func similarity(a, b string) float64 {
	if a == "" && b == "" {
		return 1.0
	}
	maxLen := len([]rune(a))
	if n := len([]rune(b)); n > maxLen {
		maxLen = n
	}
	if maxLen == 0 {
		return 1.0
	}
	return 1.0 - float64(levenshtein(a, b))/float64(maxLen)
}

// --- Rec 4 support: bounded streaming line reader ---

// streamRange reads lines [start,end] (1-based inclusive) from r, honoring the
// same line model as strings.Split(content,"\n"): a trailing newline yields a
// final empty line and an empty input is a single empty line. The returned text
// is the selected lines joined by "\n" (no trailing newline appended). Collection
// stops early when adding the next line would push the returned bytes past
// byteBudget (byteBudget<=0 disables the cap); the first in-range line is always
// returned so a paging loop always makes progress.
//
// Returns lastLine (1-based number of the last line returned, 0 if none) and
// nextLine (the 1-based line to resume from, or 0 when the file ended within the
// requested range so there is nothing left to page).
func streamRange(r io.Reader, start, end int, byteBudget int64) (text string, lastLine, nextLine int, err error) {
	if start < 1 {
		start = 1
	}
	br := bufio.NewReaderSize(r, 64*1024)
	var b strings.Builder
	var used int64
	lineNo := 0
	collected := 0
	truncated := false
	stoppedAtEnd := false
	eof := false

	for {
		chunk, rerr := br.ReadString('\n')
		var (
			hasLine bool
			seg     string
		)
		if len(chunk) > 0 {
			hasLine = true
			seg = strings.TrimSuffix(chunk, "\n")
		} else if rerr != nil {
			// EOF with an empty final read: strings.Split emits one trailing empty
			// segment (covers both the empty file and a file ending in '\n').
			hasLine = true
			seg = ""
		}

		if hasLine {
			lineNo++
			if lineNo >= start && lineNo <= end {
				sep := int64(0)
				if collected > 0 {
					sep = 1 // newline separator
				}
				need := sep + int64(len(seg))
				if byteBudget > 0 && used+need > byteBudget {
					if collected == 0 {
						// First in-range line alone exceeds the budget: return a
						// byte-truncated prefix so output stays bounded even for a file
						// that is one enormous line. Line-based paging cannot resume
						// mid-line, so this is a bounded best-effort for that edge case.
						if room := byteBudget - used; room > 0 {
							b.WriteString(seg[:int(room)])
							used += room
							lastLine = lineNo
							collected++
						}
					}
					truncated = true
					break
				}
				if collected > 0 {
					b.WriteByte('\n')
					used++
				}
				b.WriteString(seg)
				used += int64(len(seg))
				lastLine = lineNo
				collected++
			}
			if lineNo >= end {
				stoppedAtEnd = true
				break
			}
		}

		if rerr != nil {
			if rerr != io.EOF {
				return b.String(), lastLine, 0, rerr
			}
			eof = true
			break
		}
	}

	switch {
	case truncated:
		nextLine = lastLine + 1
	case stoppedAtEnd && !eof:
		nextLine = end + 1
	default: // reached EOF
		nextLine = 0
	}
	return b.String(), lastLine, nextLine, nil
}
