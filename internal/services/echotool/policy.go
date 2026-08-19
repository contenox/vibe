package echotool

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

// policyMaxEchoBytes is written under [tools_policies.native-echo] and reaches a
// call only through ToolsArgsFromContext — never from the model's arguments.
const policyMaxEchoBytes = "_max_echo_bytes"

// Bounds on the policy value itself: echo hands back what it was given, so an
// uncapped one is a context amplifier and a zero one would silence the tool.
const (
	defaultMaxEchoBytes = 32 << 10
	minMaxEchoBytes     = 64
	maxMaxEchoBytes     = 1 << 20
)

// limit is one call's effective ceiling: the package default unless the chain's
// tools policy moved it.
type limit struct {
	maxBytes int
}

// limitFrom keys on name, the toolset's registration key: that is also the key
// the policy block and the HITL rules are written against.
func limitFrom(ctx context.Context, name string) limit {
	args := taskengine.ToolsArgsFromContext(ctx, name)
	return limit{maxBytes: policyInt(args, policyMaxEchoBytes, defaultMaxEchoBytes, minMaxEchoBytes, maxMaxEchoBytes)}
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

// clip cuts at a rune boundary and never cuts silently: the trailing marker is
// the only thing that tells the model the echo is short of what it was given.
func (l limit) clip(s string) string {
	if len(s) <= l.maxBytes {
		return s
	}
	cut := 0
	for i := range s {
		if i > l.maxBytes {
			break
		}
		cut = i
	}
	return s[:cut] + fmt.Sprintf("\n… (+%d bytes not echoed; raise tools_policies.%s.%s to echo more)",
		len(s)-cut, ToolsProviderName, policyMaxEchoBytes)
}
