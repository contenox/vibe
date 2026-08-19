package localtools

import (
	"errors"
	"io"
	"os"
	"sort"
	"strings"
)

func sniffFilePrefix(absPath string) ([]byte, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, sniffBinaryBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}

func sniffBinaryFile(absPath string) (bool, error) {
	prefix, err := sniffFilePrefix(absPath)
	if err != nil {
		return false, err
	}
	return isBinarySample(prefix), nil
}

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
