package taskengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MacroEnv is a transparent decorator around EnvExecutor that expands macros (toolservice, var, date, now, chain) in task templates before execution.
type MacroEnv struct {
	inner         EnvExecutor
	toolsProvider ToolsRepo
}

// NewMacroEnv wraps an existing EnvExecutor with macro expansion.
func NewMacroEnv(inner EnvExecutor, toolsProvider ToolsRepo) (EnvExecutor, error) {
	if inner == nil {
		return nil, fmt.Errorf("NewMacroEnv: inner EnvExecutor is nil")
	}
	return &MacroEnv{
		inner:         inner,
		toolsProvider: toolsProvider,
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

	// Deep-copy tasks and pointer fields so macro expansion never mutates the chain definition, which may be shared across goroutines.
	clone := *chain
	clone.Tasks = make([]TaskDefinition, len(chain.Tasks))
	copy(clone.Tasks, chain.Tasks)

	for i := range clone.Tasks {
		if clone.Tasks[i].ExecuteConfig != nil {
			ec := *clone.Tasks[i].ExecuteConfig
			clone.Tasks[i].ExecuteConfig = &ec
		}
		if clone.Tasks[i].Tools != nil {
			h := *clone.Tasks[i].Tools
			clone.Tasks[i].Tools = &h
		}
	}

	for i := range clone.Tasks {
		t := &clone.Tasks[i]

		var allowlist []string
		if t.ExecuteConfig != nil {
			allowlist = t.ExecuteConfig.Tools
		}

		var err error
		if t.PromptTemplate != "" {
			t.PromptTemplate, err = m.expandSpecialTemplates(ctx, &clone, allowlist, t.PromptTemplate, false)
			if err != nil {
				return nil, DataTypeAny, nil, fmt.Errorf("task %s: prompt_template macro error: %w", t.ID, err)
			}
		}
		if t.Print != "" {
			t.Print, err = m.expandSpecialTemplates(ctx, &clone, allowlist, t.Print, false)
			if err != nil {
				return nil, DataTypeAny, nil, fmt.Errorf("task %s: print macro error: %w", t.ID, err)
			}
		}
		if t.OutputTemplate != "" {
			t.OutputTemplate, err = m.expandSpecialTemplates(ctx, &clone, allowlist, t.OutputTemplate, false)
			if err != nil {
				return nil, DataTypeAny, nil, fmt.Errorf("task %s: output_template macro error: %w", t.ID, err)
			}
		}
		if t.SystemInstruction != "" {
			// SystemInstruction is the provider cache's stable prefix, so it expands with stablePrefix=true (wall-clock macros degrade to day granularity).
			t.SystemInstruction, err = m.expandSpecialTemplates(ctx, &clone, allowlist, t.SystemInstruction, true)
			if err != nil {
				return nil, DataTypeAny, nil, fmt.Errorf("task %s: system_instruction macro error: %w", t.ID, err)
			}

		}

		if t.ExecuteConfig != nil {
			if t.ExecuteConfig.Model != "" {
				t.ExecuteConfig.Model, err = m.expandSpecialTemplates(ctx, &clone, allowlist, t.ExecuteConfig.Model, false)
				if err != nil {
					return nil, DataTypeAny, nil, fmt.Errorf("task %s: execute_config.model macro error: %w", t.ID, err)
				}
			}
			if t.ExecuteConfig.Provider != "" {
				t.ExecuteConfig.Provider, err = m.expandSpecialTemplates(ctx, &clone, allowlist, t.ExecuteConfig.Provider, false)
				if err != nil {
					return nil, DataTypeAny, nil, fmt.Errorf("task %s: execute_config.provider macro error: %w", t.ID, err)
				}
			}
			if t.ExecuteConfig.Think != "" {
				t.ExecuteConfig.Think, err = m.expandSpecialTemplates(ctx, &clone, allowlist, t.ExecuteConfig.Think, false)
				if err != nil {
					return nil, DataTypeAny, nil, fmt.Errorf("task %s: execute_config.think macro error: %w", t.ID, err)
				}
			}
			if t.ExecuteConfig.MaxTokensTemplate != "" {
				expanded, err := m.expandSpecialTemplates(ctx, &clone, allowlist, t.ExecuteConfig.MaxTokensTemplate, false)
				if err != nil {
					return nil, DataTypeAny, nil, fmt.Errorf("task %s: execute_config.max_tokens macro error: %w", t.ID, err)
				}
				maxTokens, err := parseMacroInt("max_tokens", expanded)
				if err != nil {
					return nil, DataTypeAny, nil, fmt.Errorf("task %s: execute_config.max_tokens macro error: %w", t.ID, err)
				}
				t.ExecuteConfig.MaxTokens = maxTokens
				t.ExecuteConfig.MaxTokensTemplate = ""
			}
		}
	}

	return m.inner.ExecEnv(ctx, &clone, input, dataType)
}

var macroRe = regexp.MustCompile(`\{\{([a-zA-Z0-9_]+)(?::([^}]*))?\}\}`)

func (m *MacroEnv) expandSpecialTemplates(ctx context.Context, chain *TaskChainDefinition, allowlist []string, in string, stablePrefix bool) (string, error) {
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

		replacement, err := m.expandOne(ctx, chain, allowlist, namespace, payload, in[start:end], stablePrefix)
		if err != nil {
			return "", err
		}
		buf.WriteString(replacement)
		last = end
	}

	buf.WriteString(in[last:])
	return buf.String(), nil
}

func (m *MacroEnv) expandOne(ctx context.Context, chain *TaskChainDefinition, allowlist []string, namespace, payload, original string, stablePrefix bool) (string, error) {
	switch namespace {
	case "toolservice":
		if m.toolsProvider == nil {
			return original, nil
		}
		allowed, err := resolveToolsNames(ctx, allowlist, m.toolsProvider)
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
			return m.renderToolsAndToolsJSON(ctx, allowed)
		case "tools":
			if arg != "" {
				return m.renderToolsForToolsJSON(ctx, allowed, arg)
			}
			return m.renderToolsNamesJSON(allowed)
		default:
			return original, nil
		}
	case "var":
		name, fallback, hasFallback := splitVarPayload(payload)
		vars, err := TemplateVarsFromContext(ctx)
		if err != nil {
			if hasFallback {
				return resolveVarFallback(nil, fallback)
			}
			return "", fmt.Errorf("{{var:%s}}: %w", name, err)
		}
		if v, ok := lookupTemplateVar(vars, name); ok && v != "" {
			return v, nil
		}
		if hasFallback {
			return resolveVarFallback(vars, fallback)
		}
		return "", fmt.Errorf("template var %q is not set", name)
	case "tools":
		if m.toolsProvider == nil || payload != "" {
			return original, nil
		}
		allowed, err := resolveToolsNames(ctx, allowlist, m.toolsProvider)
		if err != nil || len(allowed) == 0 {
			return "{}", nil
		}
		return m.renderToolsAndToolsJSON(ctx, allowed)
	case "host":
		switch payload {
		case "os":
			return runtime.GOOS, nil
		case "arch":
			return runtime.GOARCH, nil
		case "":
			b, _ := json.Marshal(map[string]string{"os": runtime.GOOS, "arch": runtime.GOARCH})
			return string(b), nil
		default:
			return original, nil
		}
	case "date":
		layout := "2006-01-02"
		if payload != "" {
			layout = payload
		}
		return time.Now().Format(layout), nil
	case "now":
		layout := time.RFC3339
		switch {
		case payload != "":
			// An explicit layout is always respected, even in the stable prefix — a deliberate trade of cache reuse for precision.
			layout = payload
		case stablePrefix:
			// Provider prefix caches key on exact system-instruction bytes; the default layout degrades to day granularity here to avoid cold-starting the cache every request.
			layout = "2006-01-02"
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

func splitVarPayload(payload string) (name, fallback string, hasFallback bool) {
	parts := strings.SplitN(payload, "|", 2)
	name = strings.TrimSpace(parts[0])
	if len(parts) == 2 {
		return name, strings.TrimSpace(parts[1]), true
	}
	return name, "", false
}

func resolveVarFallback(vars map[string]string, fallback string) (string, error) {
	fallback = strings.TrimSpace(fallback)
	if strings.HasPrefix(fallback, "var:") {
		name := strings.TrimSpace(strings.TrimPrefix(fallback, "var:"))
		if v, ok := lookupTemplateVar(vars, name); ok && v != "" {
			return v, nil
		}
		return "", fmt.Errorf("template fallback var %q is not set", name)
	}
	return fallback, nil
}

func lookupTemplateVar(vars map[string]string, name string) (string, bool) {
	if vars == nil {
		return "", false
	}
	name = strings.TrimSpace(name)
	if v, ok := vars[name]; ok {
		return v, true
	}
	switch {
	case strings.Contains(name, "-"):
		v, ok := vars[strings.ReplaceAll(name, "-", "_")]
		return v, ok
	case strings.Contains(name, "_"):
		v, ok := vars[strings.ReplaceAll(name, "_", "-")]
		return v, ok
	default:
		return "", false
	}
}

func parseMacroInt(field, value string) (*int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return nil, fmt.Errorf("%s must expand to an integer, got %q", field, value)
	}
	if n < 0 {
		return nil, fmt.Errorf("%s must be non-negative, got %d", field, n)
	}
	return &n, nil
}

func sortedCopy(names []string) []string {
	out := make([]string, len(names))
	copy(out, names)
	sort.Strings(out)
	return out
}

func (m *MacroEnv) renderToolsNamesJSON(names []string) (string, error) {
	b, err := json.Marshal(sortedCopy(names))
	if err != nil {
		return "", fmt.Errorf("failed to marshal tools names: %w", err)
	}
	return string(b), nil
}

func (m *MacroEnv) renderToolsAndToolsJSON(ctx context.Context, names []string) (string, error) {
	result := make(map[string][]string, len(names))
	for _, name := range names {
		tools, err := m.toolsProvider.GetToolsForToolsByName(ctx, name)
		if err != nil || len(tools) == 0 {
			continue
		}
		fnNames := make([]string, 0, len(tools))
		for _, t := range tools {
			fnNames = append(fnNames, t.Function.Name)
		}
		// Pin inner arrays too: map keys sort automatically but per-tools lists don't, and must stay stable across requests.
		sort.Strings(fnNames)
		result[name] = fnNames
	}

	b, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("failed to marshal tools+tools: %w", err)
	}
	return string(b), nil
}

func (m *MacroEnv) renderToolsForToolsJSON(ctx context.Context, allowed []string, toolsName string) (string, error) {
	permitted := false
	for _, a := range allowed {
		if a == toolsName {
			permitted = true
			break
		}
	}
	if !permitted {
		b, _ := json.Marshal([]string{})
		return string(b), nil
	}
	tools, err := m.toolsProvider.GetToolsForToolsByName(ctx, toolsName)
	if err != nil {
		return "", fmt.Errorf("failed to get tools for tools %s: %w", toolsName, err)
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Function.Name)
	}
	// Stable prompt bytes regardless of registry enumeration order.
	sort.Strings(names)
	b, err := json.Marshal(names)
	if err != nil {
		return "", fmt.Errorf("failed to marshal tools for tools %s: %w", toolsName, err)
	}
	return string(b), nil
}
