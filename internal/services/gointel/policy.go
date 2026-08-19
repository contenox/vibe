package gointel

import (
	"context"
	"strconv"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

// Policy keys, written under [tools_policies.native-go] and reaching a call
// only through ToolsArgsFromContext — never from the model's arguments.
const (
	policyMaxReferences  = "_max_references"
	policyMaxSymbols     = "_max_symbols"
	policyMaxDiagnostics = "_max_diagnostics"
)

// limits are one call's effective result ceilings. Each defaults to the
// package's own hard cap, so a policy can only tighten a result below what the
// toolset already allows — never widen one past it.
type limits struct {
	references  int
	symbols     int
	diagnostics int
}

// limitsFrom keys on name, the toolset's registration key: that is also the key
// the policy block and the HITL rules are written against.
func limitsFrom(ctx context.Context, name string) limits {
	args := taskengine.ToolsArgsFromContext(ctx, name)
	return limits{
		references:  policyInt(args, policyMaxReferences, maxRefCap, maxRefCap),
		symbols:     policyInt(args, policyMaxSymbols, maxSymbolCap, maxSymbolCap),
		diagnostics: policyInt(args, policyMaxDiagnostics, maxDiagCap, maxDiagCap),
	}
}

func policyInt(args map[string]string, key string, def, max int) int {
	raw, ok := args[key]
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	if n < 1 {
		return 1
	}
	if n > max {
		return max
	}
	return n
}

// clampMax folds the model's requested max into the chain's ceiling; an unset
// max is left unset so the index applies def, unless policy cut below def.
func clampMax(requested, ceiling, def int) int {
	if requested <= 0 {
		if def <= ceiling {
			return requested
		}
		return ceiling
	}
	if requested > ceiling {
		return ceiling
	}
	return requested
}
