package echotool

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

// tools implements taskengine.ToolsRepo. It holds no client, no root and no
// connection: there is nothing for a declaration to scope but the name itself.
type tools struct {
	// name is the toolset key this repo is registered under, which is also the
	// key its tools-policy block and its HITL rules are written against.
	name string
}

// NewTools returns the echo ToolsRepo under ToolsProviderName.
func NewTools() taskengine.ToolsRepo {
	return NewToolsWith(ToolsProviderName)
}

// NewToolsWith is NewTools under an explicit toolset name, for a surface that
// registers it under another scoped key.
func NewToolsWith(name string) taskengine.ToolsRepo {
	if name == "" {
		name = ToolsProviderName
	}
	return &tools{name: name}
}

func (h *tools) Exec(ctx context.Context, _ time.Time, input any, _ bool, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if call == nil {
		return nil, taskengine.DataTypeAny, errors.New("echo: tools required")
	}
	toolName := call.ToolName
	if toolName == "" {
		toolName = call.Name
	}
	// The provider name resolves as well: this toolset has exactly one tool, and
	// a declarative `tools` task may name only the provider.
	if toolName != ToolEcho && toolName != h.name {
		return nil, taskengine.DataTypeAny, fmt.Errorf("echo: unknown tool %q; this toolset provides %s %s",
			echoName(toolName), strings.Join(toolNames, ", "), severityRecoverable)
	}

	lim := limitFrom(ctx, h.name)

	switch v := input.(type) {
	case taskengine.ChatHistory:
		return appendEcho(v, lim), taskengine.DataTypeChatHistory, nil
	case *taskengine.ChatHistory:
		if v == nil {
			return nothingToEcho, taskengine.DataTypeString, nil
		}
		// Returned by value: DataTypeChatHistory consumers assert the value type.
		return appendEcho(*v, lim), taskengine.DataTypeChatHistory, nil
	case string:
		return lim.clip(v), taskengine.DataTypeString, nil
	case map[string]any:
		return echoArgs(callArgs(v, call), lim)
	case nil:
		return echoArgs(callArgs(nil, call), lim)
	default:
		return lim.clip(render(input)), taskengine.DataTypeString, nil
	}
}

// Supports reports the scoped toolset key alone — an unscoped tool name here would enter a MultiRepo allowlist universe as its own entry, separately addressable and surviving "!native-echo"; dispatch keys on the toolset name, not the tool name.
func (h *tools) Supports(context.Context) ([]string, error) {
	return []string{h.name}, nil
}

func echoArgs(args map[string]any, lim limit) (any, taskengine.DataType, error) {
	if err := rejectUnknownArgs(ToolEcho, args, "input"); err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	raw, ok := args["input"]
	if !ok {
		return nothingToEcho, taskengine.DataTypeString, nil
	}
	return lim.clip(render(raw)), taskengine.DataTypeString, nil
}

// appendEcho quotes the last user message back as an assistant turn. The
// messages are copied first: appending in place can write into the caller's
// backing array.
func appendEcho(history taskengine.ChatHistory, lim limit) taskengine.ChatHistory {
	content := nothingToEcho
	for i := len(history.Messages) - 1; i >= 0; i-- {
		if history.Messages[i].Role == "user" && history.Messages[i].Content != "" {
			content = history.Messages[i].Content
			break
		}
	}
	out := history
	out.Messages = make([]taskengine.Message, len(history.Messages), len(history.Messages)+1)
	copy(out.Messages, history.Messages)
	out.Messages = append(out.Messages, taskengine.Message{
		Role:      "assistant",
		Content:   lim.clip("Echo: " + content),
		Timestamp: time.Now().UTC(),
	})
	return out
}

// render keeps a string verbatim; anything else is rendered the way the engine
// would render it, so what a chain sees does not depend on the argument's type.
func render(raw any) string {
	if s, ok := raw.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", raw)
}

// callArgs assembles the argument map from the chain input or, for declarative
// `tools` tasks that carry arguments on the call itself, from ToolsCall.Args.
func callArgs(input map[string]any, call *taskengine.ToolsCall) map[string]any {
	if len(input) > 0 {
		return input
	}
	if len(call.Args) > 0 {
		out := make(map[string]any, len(call.Args))
		for k, v := range call.Args {
			out[k] = v
		}
		return out
	}
	return map[string]any{}
}

func rejectUnknownArgs(toolName string, args map[string]any, allowed ...string) error {
	if len(args) == 0 {
		return nil
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	var unknown []string
	for key := range args {
		if _, ok := allowedSet[key]; !ok {
			// The key is model-supplied too, so it is clamped like every echoed argument.
			unknown = append(unknown, echoName(key))
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	sort.Strings(allowed)
	return recoverablef("%s: unknown argument(s): %s (allowed: %s)",
		toolName, strings.Join(unknown, ", "), strings.Join(allowed, ", "))
}

var _ taskengine.ToolsRepo = (*tools)(nil)
