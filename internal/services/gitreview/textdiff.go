package gitreview

import "strings"

// Line kinds. The contract fixes exactly these three spellings; a renderer
// switches on them and never re-parses a leading '+'/'-'/' ' character.
const (
	LineContext = "ctx"
	LineAdd     = "add"
	LineDel     = "del"
)

const (
	hunkContext = 3

	maxEditDistance = 6000

	maxHunkLines = 20000
)

// Line is one rendered diff line: Text never carries its newline, and NoNewline marks a line as the last of its side with no terminating newline (git's "\ No newline at end of file").
type Line struct {
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	NoNewline bool   `json:"noNewline,omitempty"`
}

// Hunk is one contiguous change region in git's 1-based coordinates (OldStart/OldLines for the `from` side, NewStart/NewLines for the `to` side); a pure insertion reports OldLines 0 with OldStart naming the preceding line, as `@@ -0,0 +1,3 @@` does.
type Hunk struct {
	OldStart int    `json:"oldStart"`
	OldLines int    `json:"oldLines"`
	NewStart int    `json:"newStart"`
	NewLines int    `json:"newLines"`
	Lines    []Line `json:"lines"`
}

type editOp struct {
	kind string
	text string
}

func splitLines(s string) (lines []string, trailingNewline bool) {
	if s == "" {
		return nil, false
	}
	trailingNewline = strings.HasSuffix(s, "\n")
	if trailingNewline {
		s = s[:len(s)-1]
	}
	return strings.Split(s, "\n"), trailingNewline
}

func joinLines(lines []string, trailingNewline bool) string {
	if len(lines) == 0 {
		return ""
	}
	s := strings.Join(lines, "\n")
	if trailingNewline {
		s += "\n"
	}
	return s
}

const noNewlineMark = "\x00"

func decompose(content string) []string {
	lines, trailing := splitLines(content)
	if len(lines) > 0 && !trailing {
		out := make([]string, len(lines))
		copy(out, lines)
		out[len(out)-1] += noNewlineMark
		return out
	}
	return lines
}

func recompose(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	trailing := true
	if strings.HasSuffix(lines[len(lines)-1], noNewlineMark) {
		out := make([]string, len(lines))
		copy(out, lines)
		out[len(out)-1] = strings.TrimSuffix(out[len(out)-1], noNewlineMark)
		lines = out
		trailing = false
	}
	return joinLines(lines, trailing)
}

func markLine(kind, text string) Line {
	if strings.HasSuffix(text, noNewlineMark) {
		return Line{Kind: kind, Text: strings.TrimSuffix(text, noNewlineMark), NoNewline: true}
	}
	return Line{Kind: kind, Text: text}
}

func unmarkLine(l Line) string {
	if l.NoNewline {
		return l.Text + noNewlineMark
	}
	return l.Text
}

func diffHunks(from, to string) (hunks []Hunk, truncated bool) {
	return diffLineVectors(decompose(from), decompose(to))
}

func diffLineVectors(oldLines, newLines []string) (hunks []Hunk, truncated bool) {
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}

	middle := myersOps(oldLines[prefix:len(oldLines)-suffix], newLines[prefix:len(newLines)-suffix])
	if middle == nil {
		return nil, true
	}

	ops := make([]editOp, 0, prefix+len(middle)+suffix)
	for i := 0; i < prefix; i++ {
		ops = append(ops, editOp{kind: LineContext, text: oldLines[i]})
	}
	ops = append(ops, middle...)
	for i := len(oldLines) - suffix; i < len(oldLines); i++ {
		ops = append(ops, editOp{kind: LineContext, text: oldLines[i]})
	}
	return groupHunks(ops)
}

func myersOps(a, b []string) []editOp {
	n, m := len(a), len(b)
	switch {
	case n == 0 && m == 0:
		return []editOp{}
	case n == 0:
		ops := make([]editOp, 0, m)
		for _, line := range b {
			ops = append(ops, editOp{kind: LineAdd, text: line})
		}
		return ops
	case m == 0:
		ops := make([]editOp, 0, n)
		for _, line := range a {
			ops = append(ops, editOp{kind: LineDel, text: line})
		}
		return ops
	}

	maxD := n + m
	if maxD > maxEditDistance {
		maxD = maxEditDistance
	}
	offset := n + m
	v := make([]int, 2*(n+m)+1)
	// trace[d] is V before step d, windowed to k in [-d, d]: this keeps the backtrack table O(D^2) instead of O(D*(n+m)).
	trace := make([][]int, 0, maxD+1)

	for d := 0; d <= maxD; d++ {
		window := make([]int, 2*d+1)
		copy(window, v[offset-d:offset+d+1])
		trace = append(trace, window)

		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				return myersBacktrack(a, b, trace, d)
			}
		}
	}
	// Exceeded maxEditDistance: nil is the "too different to render" signal every caller checks for.
	return nil
}

func myersBacktrack(a, b []string, trace [][]int, d int) []editOp {
	ops := make([]editOp, 0, len(a)+len(b))
	prepend := func(kind, text string) {
		ops = append(ops, editOp{kind: kind, text: text})
	}

	x, y := len(a), len(b)
	for ; d > 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[d+k-1] < v[d+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[d+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			prepend(LineContext, a[x])
		}
		switch {
		case x > prevX:
			x--
			prepend(LineDel, a[x])
		case y > prevY:
			y--
			prepend(LineAdd, b[y])
		}
	}
	for x > 0 && y > 0 {
		x--
		y--
		prepend(LineContext, a[x])
	}

	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
	}
	return ops
}

func groupHunks(ops []editOp) (hunks []Hunk, truncated bool) {
	include := make([]bool, len(ops))
	for i, op := range ops {
		if op.kind == LineContext {
			continue
		}
		lo := i - hunkContext
		if lo < 0 {
			lo = 0
		}
		hi := i + hunkContext + 1
		if hi > len(ops) {
			hi = len(ops)
		}
		for k := lo; k < hi; k++ {
			include[k] = true
		}
	}

	hunks = []Hunk{}
	var cur Hunk
	open := false
	emitted := 0
	oldN, newN := 1, 1

	flush := func() {
		if !open {
			return
		}
		// git's convention for an empty side: the start names the line the change follows (0 at the head of the file).
		if cur.OldLines == 0 {
			cur.OldStart--
		}
		if cur.NewLines == 0 {
			cur.NewStart--
		}
		hunks = append(hunks, cur)
		cur = Hunk{}
		open = false
	}

	for i, op := range ops {
		if !include[i] {
			flush()
			oldN++
			newN++
			continue
		}
		if emitted >= maxHunkLines {
			flush()
			return hunks, true
		}
		if !open {
			cur = Hunk{OldStart: oldN, NewStart: newN, Lines: []Line{}}
			open = true
		}
		cur.Lines = append(cur.Lines, markLine(op.kind, op.text))
		emitted++
		switch op.kind {
		case LineContext:
			cur.OldLines++
			cur.NewLines++
			oldN++
			newN++
		case LineDel:
			cur.OldLines++
			oldN++
		case LineAdd:
			cur.NewLines++
			newN++
		}
	}
	flush()
	return hunks, false
}

func applyLineVectorHunk(fromLines []string, h Hunk) (lines []string, ok bool) {
	at := h.OldStart - 1
	if h.OldLines == 0 {
		at = h.OldStart
	}
	end := at + h.OldLines
	if at < 0 || end > len(fromLines) || at > end {
		return nil, false
	}

	replacement := make([]string, 0, len(h.Lines))
	verify := make([]string, 0, h.OldLines)
	for _, l := range h.Lines {
		text := unmarkLine(l)
		switch l.Kind {
		case LineContext:
			replacement = append(replacement, text)
			verify = append(verify, text)
		case LineAdd:
			replacement = append(replacement, text)
		case LineDel:
			verify = append(verify, text)
		default:
			return nil, false
		}
	}
	if len(verify) != h.OldLines {
		return nil, false
	}
	// Verified line by line before any mutation: a shifted apply must refuse, never silently corrupt content.
	for i, want := range verify {
		if fromLines[at+i] != want {
			return nil, false
		}
	}

	out := make([]string, 0, len(fromLines)-h.OldLines+len(replacement))
	out = append(out, fromLines[:at]...)
	out = append(out, replacement...)
	out = append(out, fromLines[end:]...)
	return out, true
}

func applyHunk(from string, h Hunk) (string, bool) {
	lines, ok := applyLineVectorHunk(decompose(from), h)
	if !ok {
		return "", false
	}
	return recompose(lines), true
}
