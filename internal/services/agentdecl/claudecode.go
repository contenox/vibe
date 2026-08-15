package agentdecl

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const specDateClaudeCode = "2026-08-14"

// ErrNoFrontmatter reports a source file that is not an agent definition.
type ErrNoFrontmatter struct{ Path string }

func (e *ErrNoFrontmatter) Error() string {
	return fmt.Sprintf("agentdecl: %s has no YAML frontmatter block", e.Path)
}

// ErrMissingField reports a required field the source omitted.
type ErrMissingField struct {
	Path  string
	Field string
}

func (e *ErrMissingField) Error() string {
	return fmt.Sprintf("agentdecl: %s is missing required field %q", e.Path, e.Field)
}

// ParseClaudeCode parses a .claude/agents definition. Frontmatter keys it does
// not recognize are reported through AgentIR.Unmapped rather than rejected, so
// a newer Claude Code release does not break the importer.
func ParseClaudeCode(path string, data []byte, cfg Config) (*AgentIR, error) {
	front, body, ok := splitFrontmatter(data)
	if !ok {
		return nil, &ErrNoFrontmatter{Path: path}
	}

	fields := map[string]any{}
	if err := yaml.Unmarshal(front, &fields); err != nil {
		return nil, fmt.Errorf("agentdecl: parse %s frontmatter: %w", path, err)
	}

	sum := sha256.Sum256(data)
	ir := &AgentIR{
		Source: Source{
			Dialect:  DialectClaudeCode,
			SpecDate: specDateClaudeCode,
			Path:     path,
			SHA256:   hex.EncodeToString(sum[:]),
		},
		SystemPrompt: strings.TrimSpace(string(body)),
		Role:         RoleMission,
		Posture:      PostureAskAlways,
	}

	ir.Name = strings.TrimSpace(stringField(fields, "name"))
	if ir.Name == "" {
		return nil, &ErrMissingField{Path: path, Field: "name"}
	}
	ir.Description = strings.TrimSpace(stringField(fields, "description"))
	if ir.Description == "" {
		return nil, &ErrMissingField{Path: path, Field: "description"}
	}
	delete(fields, "name")
	delete(fields, "description")

	if raw := strings.TrimSpace(stringField(fields, "model")); raw != "" && raw != "inherit" {
		ir.Model.Raw = raw
		if provider, id, ok := cfg.MapModel(raw); ok {
			ir.Model.Provider, ir.Model.ID = provider, id
		}
	}
	delete(fields, "model")

	if named, ok := fields["tools"]; ok && named != nil {
		allow, skipped, err := cfg.MapTools(listField(fields, "tools"))
		if err != nil {
			return nil, err
		}
		ir.Tools.Allow = allow
		ir.Unmapped = append(ir.Unmapped, skipped...)
	} else {
		// An omitted tools list inherits every tool, per the declaration format.
		ir.Tools.Inherit = true
	}
	delete(fields, "tools")

	deny, skippedDeny, err := cfg.MapTools(listField(fields, "disallowedTools"))
	if err != nil {
		return nil, err
	}
	ir.Tools.Deny = deny
	ir.Unmapped = append(ir.Unmapped, skippedDeny...)
	delete(fields, "disallowedTools")

	if mode := strings.TrimSpace(stringField(fields, "permissionMode")); mode != "" {
		posture, note := postureFromClaudeCode(mode)
		ir.Posture = posture
		if note != "" {
			ir.AddUnmapped("permissionMode", mode, note)
		}
	}
	delete(fields, "permissionMode")

	ir.Think = strings.TrimSpace(stringField(fields, "effort"))
	delete(fields, "effort")

	if turns, ok := intField(fields, "maxTurns"); ok {
		ir.Budgets.MaxTurns = &turns
	}
	delete(fields, "maxTurns")

	if err := parseMCPServers(path, fields, ir); err != nil {
		return nil, err
	}
	delete(fields, FieldMCPServers)

	if err := parseRemoteTools(path, fields, ir); err != nil {
		return nil, err
	}
	delete(fields, FieldRemoteTools)

	for field, value := range fields {
		ir.AddUnmapped(field, summarize(value), unmappedReasonClaudeCode(field))
	}
	return ir, nil
}

func postureFromClaudeCode(mode string) (Posture, string) {
	switch mode {
	case "acceptEdits", "auto":
		return PostureAutoEdit, ""
	case "dontAsk":
		return PostureReadOnly, "auto-deny has no contenox equivalent; imported as read-only, which denies the same calls without ending the run"
	case "plan":
		return PostureReadOnly, "plan is a runtime mode, not a policy; imported as read-only"
	case "bypassPermissions":
		return PostureUnsafe, ""
	default:
		return PostureAskAlways, ""
	}
}

func unmappedReasonClaudeCode(field string) string {
	switch field {
	case "hooks":
		// Six of the seven lifecycle events have a first-class answer here, so
		// naming them is more useful than reporting a shortfall. The seventh
		// genuinely has none.
		return "contenox governs these in the runtime rather than by running shell commands: " +
			"tool gating is the HITL policy, notifications are attention asks and the inbox, " +
			"stop conditions are the drive loop, and context is the system prompt and its macros. " +
			"Only PostToolUse has no equivalent"
	case "skills":
		return "skills are files the agent reads, not a runtime feature: put them in .contenox/skills/ " +
			"and write {{skills}} in the declaration body to give this agent the inventory"
	case "background":
		// Claude Code's background means "do not block on this subagent".
		// A dispatched contenox unit is driven detached already.
		return "already the default: a dispatched agent runs detached and reports back, so there is nothing to switch on"
	case "memory":
		return "no per-agent cross-session memory scope"
	case "isolation":
		return "worktree isolation is not applied by import; run the agent under the sandbox instead"
	case "color", "maxTokens":
		return "presentation or session concern, not part of a chain"
	default:
		return "unrecognized field for this dialect"
	}
}

// splitFrontmatter returns the YAML frontmatter and the body. A body line of
// three dashes does not terminate the block, which ends at the first such line
// after the opening fence only.
func splitFrontmatter(data []byte) (front, body []byte, ok bool) {
	text := strings.TrimPrefix(string(data), "\uFEFF")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimLeft(text, "\n")
	if !strings.HasPrefix(text, "---\n") && text != "---" {
		return nil, nil, false
	}
	rest := strings.TrimPrefix(text, "---\n")
	lines := strings.Split(rest, "\n")
	for i, line := range lines {
		if strings.TrimRight(line, " \t") != "---" {
			continue
		}
		return []byte(strings.Join(lines[:i], "\n")), []byte(strings.Join(lines[i+1:], "\n")), true
	}
	return nil, nil, false
}

func stringField(fields map[string]any, key string) string {
	v, ok := fields[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// listField accepts both YAML sequences and the comma-separated string form
// Claude Code's own documented example uses.
func listField(fields map[string]any, key string) []string {
	v, ok := fields[key]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case string:
		var out []string
		for _, part := range strings.Split(t, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		return out
	case []any:
		var out []string
		for _, item := range t {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	default:
		return nil
	}
}

func intField(fields map[string]any, key string) (int, bool) {
	v, ok := fields[key]
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	default:
		return 0, false
	}
}

func summarize(v any) string {
	s := fmt.Sprintf("%v", v)
	if len(s) > 120 {
		return s[:117] + "..."
	}
	return s
}
