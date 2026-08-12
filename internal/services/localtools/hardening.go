package localtools

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

const (
	severityRecoverable = "(recoverable: adjust parameters and retry)"
	severityFatalToken  = "(fatal:"
)

func hasSeverityMarker(s string) bool {
	return strings.Contains(s, severityRecoverable) || strings.Contains(s, severityFatalToken)
}

func markSeverity(err error) error {
	if err == nil {
		return nil
	}
	if hasSeverityMarker(err.Error()) {
		return err
	}
	return fmt.Errorf("%w %s", err, severityRecoverable)
}

func recoverablef(format string, a ...any) error {
	return errors.New(fmt.Sprintf(format, a...) + " " + severityRecoverable)
}

func fatalf(reason, format string, a ...any) error {
	return fmt.Errorf("%s (fatal: %s)", fmt.Sprintf(format, a...), reason)
}

func isDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}

const maxSuggestions = 5

func didYouMean(dir, missing string) string {
	s := suggestSiblings(dir, missing, maxSuggestions)
	if len(s) == 0 {
		return ""
	}
	return " Did you mean: " + strings.Join(s, ", ") + "?"
}

func suggestSiblings(dir, missing string, limit int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	lowMiss := strings.ToLower(missing)
	type cand struct {
		name string
		rank int
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

const suggestNearestLinesMaxScan = 5000

const suggestLineCompareLen = 256

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
			// EOF with an empty final read: strings.Split emits one trailing empty segment (covers both an empty file and a file ending in '\n').
			hasLine = true
			seg = ""
		}

		if hasLine {
			lineNo++
			if lineNo >= start && lineNo <= end {
				sep := int64(0)
				if collected > 0 {
					sep = 1
				}
				need := sep + int64(len(seg))
				if byteBudget > 0 && used+need > byteBudget {
					if collected == 0 {
						// First in-range line alone exceeds the budget: truncated to a byte prefix so output stays bounded; line-based paging cannot resume mid-line, so this is best-effort.
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
