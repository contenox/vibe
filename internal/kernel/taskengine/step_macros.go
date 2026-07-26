package taskengine

import (
	"regexp"
	"strconv"
	"strings"
)

// stepMacroEdgeCountRe matches {{edge_count:<from>-><to>}}. The captured group is
// the edge key exactly as stored in SimpleEnv.edgeCounts ("fromTaskID->toTaskID").
//
// The pattern is intentionally narrow: only {{edge_count:...}} is rewritten here.
// All other macros remain MacroEnv's job (chain-start, single pass). Two-pass
// expansion is safe because MacroEnv's `default` arm returns the original
// substring for unknown namespaces, so {{edge_count:...}} survives the first
// pass unchanged.
var stepMacroEdgeCountRe = regexp.MustCompile(`\{\{edge_count:([^}]+)\}\}`)

// expandStepMacros replaces step-time macros that cannot be resolved at
// chain-start. Currently supports:
//
//   - {{edge_count:<from>-><to>}} -> decimal count from the edgeCounts map
//     (zero if the edge has never been traversed).
//
// Cheap fast-path: returns input unchanged if the marker substring is absent.
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
