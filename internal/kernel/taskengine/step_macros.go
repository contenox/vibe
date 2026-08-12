package taskengine

import (
	"regexp"
	"strconv"
	"strings"
)

var stepMacroEdgeCountRe = regexp.MustCompile(`\{\{edge_count:([^}]+)\}\}`)

func expandStepMacros(in string, edgeCounts map[string]int) string {
	if in == "" || !strings.Contains(in, "{{edge_count:") {
		return in
	}
	return stepMacroEdgeCountRe.ReplaceAllStringFunc(in, func(match string) string {
		sub := stepMacroEdgeCountRe.FindStringSubmatch(match)
		if len(sub) != 2 {
			return match
		}
		edge := strings.TrimSpace(sub[1])
		return strconv.Itoa(edgeCounts[edge])
	})
}
