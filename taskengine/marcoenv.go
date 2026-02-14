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
//   - {{var:<name>}}                    -> value from context template vars (set by caller via WithTemplateVars; engine never reads env)
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

	// Expand macros in all relevant string fields of each task.
	for i := range clone.Tasks {
		t := &clone.Tasks[i]

		var err error
		if t.PromptTemplate != "" {
			t.PromptTemplate, err = m.expandSpecialTemplates(ctx, &clone, t.PromptTemplate)
			if err != nil {
				return nil, DataTypeAny, nil, fmt.Errorf("task %s: prompt_template macro error: %w", t.ID, err)
			}
		}
		if t.Print != "" {
			t.Print, err = m.expandSpecialTemplates(ctx, &clone, t.Print)
			if err != nil {
				return nil, DataTypeAny, nil, fmt.Errorf("task %s: print macro error: %w", t.ID, err)
			}
		}
		if t.OutputTemplate != "" {
			t.OutputTemplate, err = m.expandSpecialTemplates(ctx, &clone, t.OutputTemplate)
			if err != nil {
				return nil, DataTypeAny, nil, fmt.Errorf("task %s: output_template macro error: %w", t.ID, err)
			}
		}
		if t.SystemInstruction != "" {
			t.SystemInstruction, err = m.expandSpecialTemplates(ctx, &clone, t.SystemInstruction)
			if err != nil {
				return nil, DataTypeAny, nil, fmt.Errorf("task %s: system_instruction macro error: %w", t.ID, err)
			}
		}
	}

	// Delegate to the real EnvExecutor with the rewritten chain.
	return m.inner.ExecEnv(ctx, &clone, input, dataType)
}

// unified macro: {{namespace}} or {{namespace:payload}}
var macroRe = regexp.MustCompile(`\{\{([a-zA-Z0-9_]+)(?::([^}]*))?\}\}`)

func (m *MacroEnv) expandSpecialTemplates(ctx context.Context, chain *TaskChainDefinition, in string) (string, error) {
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

		replacement, err := m.expandOne(ctx, chain, namespace, payload, in[start:end])
		if err != nil {
			return "", err
		}
		buf.WriteString(replacement)
		last = end
	}

	buf.WriteString(in[last:])
	return buf.String(), nil
}

func (m *MacroEnv) expandOne(ctx context.Context, chain *TaskChainDefinition, namespace, payload, original string) (string, error) {
	switch namespace {
	case "hookservice":
		if m.hookProvider == nil {
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
			return m.renderHooksAndToolsJSON(ctx)
		case "hooks":
			return m.renderHookNamesJSON(ctx)
		case "tools":
			if arg == "" {
				return "", fmt.Errorf("hookservice:tools requires a hook name argument")
			}
			return m.renderToolsForHookJSON(ctx, arg)
		default:
			return original, nil
		}
	case "var":
		vars := TemplateVarsFromContext(ctx)
		if vars == nil {
			return "", nil
		}
		if v, ok := vars[payload]; ok {
			return v, nil
		}
		return "", nil // missing key -> empty string
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

func (m *MacroEnv) renderHookNamesJSON(ctx context.Context) (string, error) {
	names, err := m.hookProvider.Supports(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to list hooks: %w", err)
	}
	b, err := json.Marshal(names)
	if err != nil {
		return "", fmt.Errorf("failed to marshal hook names: %w", err)
	}
	return string(b), nil
}

func (m *MacroEnv) renderHooksAndToolsJSON(ctx context.Context) (string, error) {
	names, err := m.hookProvider.Supports(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to list hooks: %w", err)
	}

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

func (m *MacroEnv) renderToolsForHookJSON(ctx context.Context, hookName string) (string, error) {
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
