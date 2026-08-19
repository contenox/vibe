package searchtool

import (
	"context"
	"strconv"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

// Policy keys, written under [tools_policies.native-workspace] and reaching a
// call only through ToolsArgsFromContext — never from the model's arguments.
const (
	policyMaxTopK         = "_max_top_k"
	policyMaxResultTokens = "_max_result_tokens"
)

// Hard bounds on the policy values themselves, so a mistyped or hostile
// configuration cannot uncap the result payload or drive it to zero.
const (
	topKCeilingMin = 1
	topKCeilingMax = 100

	resultTokenBudgetMin = 200
	resultTokenBudgetMax = 20_000
)

// limits are one call's effective ceilings: the package defaults unless the
// chain's tools policy moved them.
type limits struct {
	topKMax           int
	resultTokenBudget int
}

func (l limits) runeBudget() int { return l.resultTokenBudget * runesPerToken }

// limitsFrom keys on name, the toolset's registration key: that is also the key
// the policy block and the HITL rules are written against.
func limitsFrom(ctx context.Context, name string) limits {
	args := taskengine.ToolsArgsFromContext(ctx, name)
	return limits{
		topKMax:           policyInt(args, policyMaxTopK, topKMax, topKCeilingMin, topKCeilingMax),
		resultTokenBudget: policyInt(args, policyMaxResultTokens, resultTokenBudget, resultTokenBudgetMin, resultTokenBudgetMax),
	}
}

func policyInt(args map[string]string, key string, def, min, max int) int {
	raw, ok := args[key]
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}
