package taskengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// MacroEnv is a transparent decorator around EnvExecutor that expands
// special macros in task templates before execution. Supported macros:
//
//   - {{hookservice:list}}              -> JSON map of hook name -> tool names
//   - {{hookservice:hooks}}             -> JSON array of hook names
//   - {{hookservice:tools <hook_name>}} -> JSON array of tool names for that hook
//   - {{var:<name>}}                    -> value from context template vars (set by caller via WithTemplateVars; engine never reads env); errors if key is missing
//   - {{now}} or {{now:<layout>}}       -> current time (default RFC3339; layout e.g. 2006-01-02)
//   - {{chain:id}}                      -> chain ID of the chain being executed
//
// The engine does not expand any env:VAR-style macro; var:* is populated only by the caller.
type MacroEnv struct {
	inner        EnvExecutor
	hookProvider HookRepo
}

// NewMacroEnv wraps an existing EnvExecutor with macro expansion.
func NewMacroEnv(inner EnvExecutor, hookProvider HookRepo) (EnvExecutor, error) {
	if inner == nil {
		return nil, fmt.Errorf("NewMacroEnv: inner EnvExecutor is nil")
	}
	return &MacroEnv{
		inner:        inner,
		hookProvider: hookProvider,
	}, nil
}

func (m *MacroEnv) ExecEnv(
	ctx context.Context,
	chain *TaskChainDefinition,
	input any,
	dataType DataType,
) (any, DataType, []CapturedStateUnit, error) {
	if chain == nil {
		return nil, DataTypeAny, nil, fmt.Errorf("chain is nil")
	}

	// Shallow copy the chain, deep copy tasks so we don't mutate the original.
	clone := *chain
	clone.Tasks = make([]TaskDefinition, len(chain.Tasks))
	copy(clone.Tasks, chain.Tasks)

	// deep-copy pointer fields so macro expansion never mutates the
	// globally-cached chain definition that may be shared across goroutines.
	for i := range clone.Tasks {
		if clone.Tasks[i].ExecuteConfig != nil {
			ec := *clone.Tasks[i].ExecuteConfig
			clone.Tasks[i].ExecuteConfig = &ec
		}
		if clone.Tasks[i].Hook != nil {
			h := *clone.Tasks[i].Hook
			clone.Tasks[i].Hook = &h
		}
	}

	// Expand macros in all relevant string fields of each task.
	for i := range clone.Tasks {
		t := &clone.Tasks[i]

		// Determine the allowlist for this specific task.
		var allowlist []string
		if t.ExecuteConfig != nil {
			allowlist = t.ExecuteConfig.Hooks
		}

		var err error
		if t.PromptTemplate != "" {
			t.PromptTemplate, err = m.expandSpecialTemplates(ctx, &clone, allowlist, t.PromptTemplate)
			if err != nil {
				return nil, DataTypeAny, nil, fmt.Errorf("task %s: prompt_template macro error: %w", t.ID, err)
			}
		}
		if t.Print != "" {
			t.Print, err = m.expandSpecialTemplates(ctx, &clone, allowlist, t.Print)
			if err != nil {
				return nil, DataTypeAny, nil, fmt.Errorf("task %s: print macro error: %w", t.ID, err)
			}
		}
		if t.OutputTemplate != "" {
			t.OutputTemplate, err = m.expandSpecialTemplates(ctx, &clone, allowlist, t.OutputTemplate)
			if err != nil {
				return nil, DataTypeAny, nil, fmt.Errorf("task %s: output_template macro error: %w", t.ID, err)
			}
		}
		if t.SystemInstruction != "" {
			t.SystemInstruction, err = m.expandSpecialTemplates(ctx, &clone, allowlist, t.SystemInstruction)
			if err != nil {
				return nil, DataTypeAny, nil, fmt.Errorf("task %s: system_instruction macro error: %w", t.ID, err)
			}

			// Auto-append tools summary if tools are available and not already mentioned
			if len(allowlist) > 0 && !strings.Contains(t.SystemInstruction, "Available tools") && !strings.Contains(t.SystemInstruction, "tool") {
				allowed, _ := resolveHookNames(ctx, allowlist, m.hookProvider)
				if len(allowed) > 0 {
					summary, _ := m.renderHooksAndToolsJSON(ctx, allowed)
					if summary != "" {
						t.SystemInstruction += "\n\nAvailable tools (hook -> function names):\n" + summary
					}
				}
			}
		}

		// Expand {{var:*}} in execute_config model/provider so chains can use
		// {{var:model}} and {{var:provider}} without callers doing manual string replacement.
		if t.ExecuteConfig != nil {
			if t.ExecuteConfig.Model != "" {
				t.ExecuteConfig.Model, err = m.expandSpecialTemplates(ctx, &clone, allowlist, t.ExecuteConfig.Model)
				if err != nil {
					return nil, DataTypeAny, nil, fmt.Errorf("task %s: execute_config.model macro error: %w", t.ID, err)
				}
			}
			if t.ExecuteConfig.Provider != "" {
				t.ExecuteConfig.Provider, err = m.expandSpecialTemplates(ctx, &clone, allowlist, t.ExecuteConfig.Provider)
				if err != nil {
					return nil, DataTypeAny, nil, fmt.Errorf("task %s: execute_config.provider macro error: %w", t.ID, err)
				}
			}
		}
	}

	// Delegate to the real EnvExecutor with the rewritten chain.
	return m.inner.ExecEnv(ctx, &clone, input, dataType)
}

// unified macro: {{namespace}} or {{namespace:payload}}
var macroRe = regexp.MustCompile(`\{\{([a-zA-Z0-9_]+)(?::([^}]*))?\}\}`)

func (m *MacroEnv) expandSpecialTemplates(ctx context.Context, chain *TaskChainDefinition, allowlist []string, in string) (string, error) {
	matches := macroRe.FindAllStringSubmatchIndex(in, -1)
	if len(matches) == 0 {
		return in, nil
	}

	var buf bytes.Buffer
	last := 0

	for _, loc := range matches {
		start, end := loc[0], loc[1]
		nsStart, nsEnd := loc[2], loc[3]
		payloadStart, payloadEnd := loc[4], loc[5]

		buf.WriteString(in[last:start])

		namespace := in[nsStart:nsEnd]
		var payload string
		if payloadStart != -1 && payloadEnd != -1 {
			payload = strings.TrimSpace(in[payloadStart:payloadEnd])
		}

		replacement, err := m.expandOne(ctx, chain, allowlist, namespace, payload, in[start:end])
		if err != nil {
			return "", err
		}
		buf.WriteString(replacement)
		last = end
	}

	buf.WriteString(in[last:])
	return buf.String(), nil
}

func (m *MacroEnv) expandOne(ctx context.Context, chain *TaskChainDefinition, allowlist []string, namespace, payload, original string) (string, error) {
	switch namespace {
	case "hookservice":
		if m.hookProvider == nil {
			return original, nil
		}
		allowed, err := resolveHookNames(ctx, allowlist, m.hookProvider)
		if err != nil {
			return original, nil
		}
		parts := strings.SplitN(payload, " ", 2)
		cmd := strings.TrimSpace(parts[0])
		var arg string
		if len(parts) > 1 {
			arg = strings.TrimSpace(parts[1])
		}
		switch cmd {
		case "list":
			return m.renderHooksAndToolsJSON(ctx, allowed)
		case "hooks":
			return m.renderHookNamesJSON(allowed)
		case "tools":
			if arg == "" {
				return "", fmt.Errorf("hookservice:tools requires a hook name argument")
			}
			return m.renderToolsForHookJSON(ctx, allowed, arg)
		default:
			return original, nil
		}
	case "var":
		vars, err := TemplateVarsFromContext(ctx)
		if err != nil {
			return "", fmt.Errorf("{{var:%s}}: %w", payload, err)
		}
		if v, ok := vars[payload]; ok {
			return v, nil
		}
		return "", fmt.Errorf("template var %q is not set", payload)
	case "now":
		layout := time.RFC3339
		if payload != "" {
			layout = payload
		}
		return time.Now().Format(layout), nil
	case "chain":
		if chain == nil {
			return "", nil
		}
		switch payload {
		case "id":
			return chain.ID, nil
		default:
			return original, nil
		}
	default:
		return original, nil
	}
}

func (m *MacroEnv) renderHookNamesJSON(names []string) (string, error) {
	b, err := json.Marshal(names)
	if err != nil {
		return "", fmt.Errorf("failed to marshal hook names: %w", err)
	}
	return string(b), nil
}

func (m *MacroEnv) renderHooksAndToolsJSON(ctx context.Context, names []string) (string, error) {
	result := make(map[string][]string, len(names))
	for _, name := range names {
		tools, err := m.hookProvider.GetToolsForHookByName(ctx, name)
		if err != nil {
			// Skip broken hooks; you can also choose to fail hard here.
			continue
		}
		fnNames := make([]string, 0, len(tools))
		for _, t := range tools {
			fnNames = append(fnNames, t.Function.Name)
		}
		result[name] = fnNames
	}

	b, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("failed to marshal hooks+tools: %w", err)
	}
	return string(b), nil
}

func (m *MacroEnv) renderToolsForHookJSON(ctx context.Context, allowed []string, hookName string) (string, error) {
	// Respect the allowlist: only expose tools if the hook is allowed.
	permitted := false
	for _, a := range allowed {
		if a == hookName {
			permitted = true
			break
		}
	}
	if !permitted {
		b, _ := json.Marshal([]string{})
		return string(b), nil
	}
	tools, err := m.hookProvider.GetToolsForHookByName(ctx, hookName)
	if err != nil {
		return "", fmt.Errorf("failed to get tools for hook %s: %w", hookName, err)
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Function.Name)
	}
	b, err := json.Marshal(names)
	if err != nil {
		return "", fmt.Errorf("failed to marshal tools for hook %s: %w", hookName, err)
	}
	return string(b), nil
}
